// Package supervisor is the supervisor module (ADR-0015 §4, D-080, D-084):
// it checks services, runs the process services it owns, keeps their state
// across restarts, and emits service_failed and service_recovered, routed
// by each service's notifications.
//
// http services are checked over HTTP (http.go) by one loop each
// (service.go); process services are launched and watched by one loop
// each (process.go) once the central switch modules.supervisor.processes
// allows it. Tests give the module fake checkers (Options.NewChecker) and
// a fake launcher (Options.NewLauncher).
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/clock"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/config"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/module"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/state"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/platform/executor"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/platform/privilege"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/api"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/model"
)

const moduleName = "supervisor"

// saveEvery bounds how often counters alone are written: transitions are
// saved at once, the counters of a steady service at most this often
// (D-080 (3)).
const saveEvery = 60 * time.Second

// Options configure the module.
type Options struct {
	// NewChecker builds the checker of a service at Configure, so that
	// `sentinelctl validate` reports what it cannot build (a CA file,
	// D-080 (4)). It must not start goroutines or change the system.
	// The variation it isolates is the kind of check and the fakes of the
	// tests. nil: the HTTP checker.
	NewChecker func(Service) (Checker, error)
	// Lookup resolves the users and groups of process services at
	// Configure; the zero value reads the system's account files.
	Lookup privilege.Lookup
	// NewLauncher builds what starts and cleans up the processes of
	// process services, at Start. nil: the executor.
	NewLauncher func(clock.Clock) (launcher, error)
}

// Module is the unconfigured supervisor.
type Module struct{ opts Options }

// New returns the supervisor module.
func New(opts Options) *Module { return &Module{opts: opts} }

// Name implements module.Module.
func (*Module) Name() string { return moduleName }

// Commands implements module.Commander.
func (*Module) Commands() []api.Command {
	return []api.Command{api.SupervisorList, api.SupervisorStatus, api.SupervisorStart, api.SupervisorStop,
		api.SupervisorRestart, api.SupervisorEnable, api.SupervisorDisable, api.SupervisorReset}
}

// Configure implements module.Module: it decodes and checks the
// configuration and builds the checkers, without starting anything.
func (m *Module) Configure(mc config.ModuleConfig) (module.Configured, error) {
	p := parse(mc)
	in := &instance{settings: p.settings, processes: p.processes, warnings: p.warnings, byName: map[string]*service{},
		newLauncher: m.opts.NewLauncher, cleaning: map[string]chan struct{}{}, retired: map[string]*service{},
		ops: make(chan struct{}, 1)}
	if len(p.problems) == 0 {
		for i, def := range p.services {
			var c Checker
			var why string
			if def.Type == typeProcess {
				why = m.resolveProcess(&def.Process)
			} else {
				c, why = m.checker(def)
			}
			if why != "" {
				w := p.where[i]
				p.problems = append(p.problems, config.Problem{File: w.file, Path: "services",
					Message: fmt.Sprintf("line %d: %s", w.line, why)})
				continue
			}
			s := &service{def: def, checker: c, digest: digestOf(def)}
			in.services = append(in.services, s)
			in.byName[def.Name] = s
		}
	}
	if len(p.problems) > 0 {
		return nil, &config.ValidationError{Problems: p.problems, Warnings: p.warnings}
	}
	slices.SortFunc(in.services, func(a, b *service) int { return strings.Compare(a.def.Name, b.def.Name) })
	in.routes.Store(routesOf(in.services))
	return in, nil
}

// checker builds def's checker, or says why it cannot.
func (m *Module) checker(def Service) (Checker, string) {
	build := m.opts.NewChecker
	if build == nil {
		build = newHTTPChecker
	}
	c, err := build(def)
	switch {
	case err != nil:
		return nil, "the check cannot be built: " + err.Error() // checker errors name keys, never values
	case c == nil:
		return nil, "the check cannot be built"
	}
	return c, ""
}

// service is one configured service and what the module knows about it.
type service struct {
	def     Service
	checker Checker
	digest  string

	// Guarded by instance.mu.
	st serviceState
	// Process services: the running process and its output sinks (nil
	// when none), its pid and start time, and its phase.
	proc       proc
	sinks      []io.Closer
	group      executor.Group
	pid        int
	startedAt  time.Time
	phase      string
	outputLost bool
	// rss samples the running process's memory (launched.rss); the last
	// sample and when it was taken, and whether samples fail (memUnknown,
	// D-090: the process keeps running). over is set once the process is
	// being stopped for its memory limit.
	rss          func() (uint64, error)
	memRSS       uint64
	memSampledAt time.Time
	memUnknown   bool
	over         *memoryOver
	// While the loop waits: the next launch (phaseRestarting,
	// phaseBlocked) and why it is blocked, or the end of the cooldown
	// (phaseExhausted).
	nextAttemptAt time.Time
	resumeAt      time.Time
	blocked       string

	prev         Health // the health before the last transition
	lastDuration time.Duration
	lastSlotWait time.Duration
	waitingSince time.Time
	stalled      bool
	// ending: a reload or an operator's action is ending the service's
	// loop (endLoop); a launch that completes from then on is stopped by
	// the loop itself, as after Stop collected the processes.
	ending bool

	// The service's own lifetime inside the module's (startLoop), so that
	// a reload or an operator's action can end one service alone (D-088,
	// D-089): cancel ends its loop, done is closed when the loop has
	// returned, decided once the loop has made its first decision (an
	// operator's command waits for it), and checks counts its http checks
	// still running, stalled ones included.
	cancel  context.CancelFunc
	done    chan struct{}
	decided chan struct{}
	decide  func() // closes decided, once
	checks  sync.WaitGroup
}

// instance is a configured supervisor.
type instance struct {
	settings  settings
	processes bool       // modules.supervisor.processes (D-084 (1))
	services  []*service // sorted by name
	byName    map[string]*service
	warnings  []config.Problem
	// routes are the services' notification routes by name, read by
	// NotificationChannels without a lock and replaced whole.
	routes atomic.Pointer[map[string]config.Route]
	// newLauncher is Options.NewLauncher.
	newLauncher func(clock.Clock) (launcher, error)
	// ops is held by the operator's action in progress or by a reload
	// (one slot): both end and start service loops (D-089).
	ops chan struct{}

	// Set by Start.
	rt       module.Runtime
	log      *slog.Logger
	launcher launcher
	// samplesMemory: the launcher's processes can be sampled, so memory
	// limits are enforced (memorySampler).
	samplesMemory bool
	// redact masks secrets in what processes print: the configuration's
	// and the values of the processes' environments (D-086).
	redact  func(string) string
	store   *state.Store
	life    context.Context // the module's own lifetime, parent of every loop
	cancel  context.CancelFunc
	slots   chan struct{} // max_concurrent_checks
	workers sync.WaitGroup
	checks  sync.WaitGroup // check goroutines, stalled ones included
	running atomic.Int64   // checks in flight

	saveMu sync.Mutex // one save at a time

	mu        sync.Mutex             // guards the services' runtime fields and what follows
	groups    map[string]groupRecord // fileState.Groups
	reserved  int                    // record slots taken by launches in progress
	collected bool                   // Stop has collected the processes to stop
	// cleaning maps a name to the cleanup that owns its group record
	// until it ends (startCleanup): a loop for that name waits for it.
	cleaning map[string]chan struct{}
	// retired holds, by name, the services a reload could not stop
	// (Reconfigure): they keep running as they were until Stop, and their
	// name takes no new definition until sentineld restarts. Not in the
	// state file; their group record is the name's.
	retired map[string]*service
	// stateRecovered: the state file was corrupt at Start; the recovery
	// budgets were treated as spent (D-084 (3)).
	stateRecovered bool
	gen            uint64 // bumped by every change of the state
	savedGen       uint64 // the generation the file holds
	lastSave       time.Time
	notSaved       bool // the last save failed
	startedOK      bool
}

// Warnings implements module.Warner.
func (in *instance) Warnings() []config.Problem { return in.warnings }

// NotificationChannels implements module.Router: a service's events go to
// its notifications. It reads the routes without a lock, so it never
// blocks.
func (in *instance) NotificationChannels(e model.Event) []string {
	r, ok := (*in.routes.Load())[e.Source]
	if !ok || !r.Matches(e.Type) {
		return nil
	}
	return slices.Clone(r.Channels)
}

// routesOf returns the routes of services by name.
func routesOf(services []*service) *map[string]config.Route {
	routes := make(map[string]config.Route, len(services))
	for _, s := range services {
		routes[s.def.Name] = s.def.Route
	}
	return &routes
}

// Start implements module.Configured: it registers the event types, loads
// the state file, reconciles it with the configuration and starts one
// loop per service, plus one that cleans up the process groups of
// services no longer configured. It does not wait for processes or
// cleanups: those happen in the loops.
func (in *instance) Start(ctx context.Context, rt module.Runtime) error {
	in.rt, in.log = rt, rt.Logger
	if rt.Audit == nil || rt.Fault == nil {
		return errors.New("supervisor: the runtime has no audit log or fault reporter")
	}
	if err := rt.Events.Register(eventSpecs...); err != nil {
		return err
	}
	store, err := rt.State(stateVersion)
	if err != nil {
		return err
	}
	kept, report, err := state.LoadMigrating[*fileState](store, migrations)
	if err != nil { // a newer schema version, or an older one without migration: left untouched (D-015)
		return err
	}
	if report.Recovered {
		in.log.Warn("supervisor state file was corrupt; moved aside, starting fresh with the recovery budgets spent",
			"moved_to", report.QuarantinedTo)
		rt.Fault("state_load", "state_corrupt")
	}
	if report.MigratedFrom != 0 {
		in.log.Info("supervisor state file migrated", "from_schema", report.MigratedFrom, "to_schema", stateVersion)
	}
	newLauncher := in.newLauncher
	if newLauncher == nil {
		newLauncher = newExecLauncher
	}
	l, err := newLauncher(rt.Clock)
	if err != nil {
		return err
	}
	in.launcher = l
	sm, ok := l.(memorySampler)
	in.samplesMemory = ok && sm.SamplesMemory()
	in.redact = processRedactor(rt.Secrets, in.services)
	in.store = store
	in.mu.Lock()
	in.groups = map[string]groupRecord{}
	if kept != nil {
		maps.Copy(in.groups, kept.Groups)
	}
	if reconcile(kept, in.services) || report.FirstRun || report.Recovered {
		in.gen++
	}
	if report.Recovered {
		in.stateRecovered = true
		spendBudgets(in.services, rt.Clock.Now())
	}
	gates := make([]gate, len(in.services))
	for i, s := range in.services {
		s.prev = s.st.Health
		gates[i] = in.gateOf(s, in.processes)
	}
	gen := in.gen
	in.lastSave = rt.Clock.Now()
	in.mu.Unlock()
	if gen > 0 {
		in.save(gen)
	}
	// A Start the daemon gave up waiting for is not followed by Stop: it
	// must not leave loops that launch processes (review 3b-2 R1).
	if err := ctx.Err(); err != nil {
		in.launcher.Close()
		return err
	}
	ctx, cancel := context.WithCancel(context.Background()) // the module's own lifetime
	in.life, in.cancel = ctx, cancel
	in.slots = make(chan struct{}, in.settings.MaxConcurrentChecks)
	if stale := in.staleRecords(); len(stale) > 0 {
		in.startCleanup(ctx, stale)
	}
	for i, s := range in.services {
		in.startLoop(ctx, s, gates[i], cause{})
	}
	in.startedOK = true
	return nil
}

// gate is why a loop must not launch its process or check its service,
// fixed for the loop's life: a reload or an operator's action that changes
// it ends the loop and starts another (review 3b-5 C1). A gated loop still
// cleans up what a previous run left, then shows the gate and returns.
type gate string

const (
	gateNone     gate = ""
	gateConfig   gate = "configuration" // enabled: false (D-020: it wins)
	gateOperator gate = "operator"      // supervisor disable
	gateStopped  gate = "stopped"       // supervisor stop (process services)
	gateNotArmed gate = "not_armed"     // modules.supervisor.processes is false
)

// gateOf is the gate of a loop for s started now, with armed the central
// switch; the caller holds in.mu.
func (in *instance) gateOf(s *service, armed bool) gate {
	switch {
	case s.def.Disabled:
		return gateConfig
	case s.st.Disabled:
		return gateOperator
	case s.def.Type != typeProcess:
		return gateNone
	case s.st.Stopped:
		return gateStopped
	case !armed:
		return gateNotArmed
	}
	return gateNone
}

// phase is how a gated service shows in status.
func (g gate) phase() string {
	switch g {
	case gateConfig, gateOperator:
		return phaseDisabled
	case gateStopped:
		return phaseStopped
	}
	return phaseNotArmed
}

// startLoop starts the loop of s in its own lifetime inside ctx, the
// module's (D-088). g is fixed for the whole life of the loop; c is what
// asked for its first launch. It is called once the previous loop of s,
// if any, has returned (endLoop), and never for a service whose process
// did not stop (blocked): no other goroutine uses s's lifetime fields
// then. It returns what the loop's first decision closes.
func (in *instance) startLoop(ctx context.Context, s *service, g gate, c cause) <-chan struct{} {
	ctx, cancel := context.WithCancel(ctx)
	decided := make(chan struct{})
	in.mu.Lock()
	s.cancel, s.done, s.decided, s.decide = cancel, make(chan struct{}), decided, sync.OnceFunc(func() { close(decided) })
	s.ending = false
	done := s.done
	in.mu.Unlock()
	in.workers.Add(1)
	go func() {
		defer in.workers.Done()
		defer close(done)
		defer s.decide() // a loop that ends before deciding releases who waits
		defer cancel()   // a loop that returns by itself (gated) frees its context
		if s.def.Type == typeProcess {
			in.runProcess(ctx, s, g, c)
		} else {
			in.run(ctx, s, g)
		}
	}()
	return decided
}

// endLoop ends the loop of s and waits for it; false when ctx ended
// first. Its process, if any, keeps running: the caller stops it or a new
// loop adopts it. A launch the loop completes from now on is stopped by
// the loop itself (ending).
func (in *instance) endLoop(ctx context.Context, s *service) bool {
	in.mu.Lock()
	s.ending = true
	cancel, done := s.cancel, s.done
	in.mu.Unlock()
	cancel()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

// startCleanup cleans up the group records of names in its own goroutine,
// which owns them until it ends: a loop started for one of the names
// waits for it, so an old cleanup never races a new launch of the same
// name (review 3b-4 C2).
func (in *instance) startCleanup(ctx context.Context, names []string) {
	done := make(chan struct{})
	in.mu.Lock()
	for _, name := range names {
		in.cleaning[name] = done
	}
	in.mu.Unlock()
	in.workers.Add(1)
	go func() {
		defer in.workers.Done()
		defer func() {
			in.mu.Lock()
			for _, name := range names {
				if in.cleaning[name] == done {
					delete(in.cleaning, name)
				}
			}
			in.mu.Unlock()
			close(done)
		}()
		in.cleanupStale(ctx, names)
	}()
}

// spendBudgets treats the recovery budget of every process service as
// spent at now, after the attempts were lost with a corrupt state file
// (D-084 (3)): the processes still start, but a crash in the next window
// exhausts the budget instead of restarting a crash loop the file may
// have been counting.
func spendBudgets(services []*service, now time.Time) {
	for _, s := range services {
		if s.def.Type != typeProcess {
			continue
		}
		attempts := make([]time.Time, s.def.Process.Recovery.MaxAttempts)
		for i := range attempts {
			attempts[i] = now
		}
		s.st.Attempts = attempts
	}
}

// staleRecords returns the names whose group records belong to no
// configured process service.
func (in *instance) staleRecords() []string {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.staleLocked()
}

// Stop implements module.Configured: it ends the loops, stops the
// processes the module owns, saves the state, then waits for the checks
// still running. It never leaves a check or a process behind silently: one
// that does not end keeps Stop waiting until ctx ends, and the daemon then
// treats the instance as not stopped (D-078); a process's group record
// stays for the next start. The processes are stopped even when a loop
// did not end.
func (in *instance) Stop(ctx context.Context) error {
	if !in.startedOK {
		return nil
	}
	defer in.launcher.Close()
	in.cancel()
	var errs []error
	if !waitGroup(ctx, &in.workers) {
		errs = append(errs, errors.New("supervisor: service loops did not end in time"))
	}
	if left := in.stopProcesses(ctx); left > 0 {
		errs = append(errs, fmt.Errorf("supervisor: %d processes did not end", left))
	}
	in.mu.Lock()
	in.gen++
	gen := in.gen
	in.mu.Unlock()
	in.save(gen)
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	if !waitGroup(ctx, &in.checks) {
		in.log.Error("supervisor checks still running at stop: they ignore their cancellation", "checks", in.running.Load())
		return errors.New("supervisor: checks still running")
	}
	return nil
}

// waitGroup waits for wg until ctx ends. The goroutine that waits ends
// with wg; when ctx ends first it stays until then.
func waitGroup(ctx context.Context, wg *sync.WaitGroup) bool {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

// redactor returns the redaction of process output in use now.
func (in *instance) redactor() func(string) string {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.redact
}

// save writes the state if generation gen is not on disk yet. A failed
// save is logged and shown in status; supervision goes on, and the next
// save retries.
func (in *instance) save(gen uint64) {
	if err := in.write(gen); err != nil {
		in.log.Error("supervisor state not saved; supervision goes on and the next change retries", "error", err)
	}
}

// write writes the state if generation gen is not on disk yet and returns
// the error: a recovery attempt is launched only once it is on disk
// (D-014). Writes are serialised; one that finds a newer write already
// covered gen returns, so a burst of transitions may cost fewer writes.
func (in *instance) write(gen uint64) error {
	in.saveMu.Lock()
	defer in.saveMu.Unlock()
	in.mu.Lock()
	if gen <= in.savedGen {
		in.mu.Unlock()
		return nil
	}
	snap := in.snapshotLocked()
	cur := in.gen
	in.mu.Unlock()
	err := in.store.Save(&snap)
	in.mu.Lock()
	if err == nil {
		in.savedGen, in.lastSave, in.notSaved = cur, in.rt.Clock.Now(), false
	} else {
		in.notSaved = true
	}
	in.mu.Unlock()
	return err
}

// errUncertain: a commit failed and the file it may have replaced could
// not be written back either.
var errUncertain = errors.New("the state file may hold the change")

// commit applies change to s's state only once a file holding it is on
// disk (review 3b-5 C4): it writes a snapshot with change applied to a
// copy while it holds saveMu, so no other save can write the change first,
// and applies it in memory after the save succeeded. On failure nothing
// changed in memory. A save can fail after the file was replaced (the
// sync of its directory, review 3b-5 R2), so the state of memory is then
// written back; if that fails too, the error wraps errUncertain. A change
// that lets a service run (enable, start, reset) goes through commit; one
// that takes away is applied first.
func (in *instance) commit(s *service, change func(*serviceState)) error {
	in.saveMu.Lock()
	defer in.saveMu.Unlock()
	in.mu.Lock()
	snap := in.snapshotLocked()
	st := snap.Services[s.def.Name]
	change(&st)
	snap.Services[s.def.Name] = st
	cur := in.gen
	in.mu.Unlock()
	err := in.store.Save(&snap)
	if err != nil {
		// What the file holds is no longer known: a new generation makes
		// the next save write it whatever was saved before (review 3b-5 R4).
		in.mu.Lock()
		in.gen++
		back, gen := in.snapshotLocked(), in.gen
		in.notSaved = true
		in.mu.Unlock()
		if rerr := in.store.Save(&back); rerr != nil {
			return fmt.Errorf("%w: %w (restoring it: %w)", errUncertain, err, rerr)
		}
		in.mu.Lock()
		in.savedGen, in.lastSave, in.notSaved = max(in.savedGen, gen), in.rt.Clock.Now(), false
		in.mu.Unlock()
		return err
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	change(&s.st)
	in.gen++ // the file holds cur and the change; what changed since is saved later
	in.savedGen, in.lastSave, in.notSaved = cur, in.rt.Clock.Now(), false
	return nil
}

// snapshotLocked is the state file of now; the caller holds in.mu. The
// slices are copied: the file is written after the lock is released (C6).
func (in *instance) snapshotLocked() fileState {
	snap := fileState{Services: make(map[string]serviceState, len(in.services)), Groups: maps.Clone(in.groups)}
	for _, s := range in.services {
		st := s.st
		st.Attempts = slices.Clone(st.Attempts)
		snap.Services[s.def.Name] = st
	}
	return snap
}
