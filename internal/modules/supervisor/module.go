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
	return []api.Command{api.SupervisorList, api.SupervisorStatus}
}

// Configure implements module.Module: it decodes and checks the
// configuration and builds the checkers, without starting anything.
func (m *Module) Configure(mc config.ModuleConfig) (module.Configured, error) {
	p := parse(mc)
	in := &instance{settings: p.settings, processes: p.processes, warnings: p.warnings, byName: map[string]*service{},
		newLauncher: m.opts.NewLauncher, cleaning: map[string]chan struct{}{}, retired: map[string]*service{}}
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
	// retiring: a reload is ending the service; a launch that completes
	// from then on is stopped by its own loop, as after Stop collected
	// the processes.
	retiring bool

	// The service's own lifetime inside the module's (startLoop), so that
	// a reload can end one service alone (D-088): cancel ends its loop,
	// done is closed when the loop has returned, and checks counts its
	// http checks still running, stalled ones included.
	cancel context.CancelFunc
	done   chan struct{}
	checks sync.WaitGroup
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

	// Set by Start.
	rt       module.Runtime
	log      *slog.Logger
	launcher launcher
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
	for _, s := range in.services {
		s.prev = s.st.Health
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
	for _, s := range in.services {
		in.startLoop(ctx, s, in.processes)
	}
	in.startedOK = true
	return nil
}

// startLoop starts the loop of s in its own lifetime inside ctx, the
// module's (D-088). armed is the central switch for the whole life of the
// loop: a reload that changes it ends every process loop and starts new
// ones. No other goroutine uses s's lifetime fields yet.
func (in *instance) startLoop(ctx context.Context, s *service, armed bool) {
	ctx, cancel := context.WithCancel(ctx)
	s.cancel, s.done = cancel, make(chan struct{})
	in.workers.Add(1)
	go func() {
		defer in.workers.Done()
		defer close(s.done)
		defer cancel() // a loop that returns by itself (not armed) frees its context
		if s.def.Type == typeProcess {
			in.runProcess(ctx, s, armed)
		} else {
			in.run(ctx, s)
		}
	}()
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
	snap := fileState{Services: make(map[string]serviceState, len(in.services)), Groups: maps.Clone(in.groups)}
	for _, s := range in.services {
		st := s.st
		st.Attempts = slices.Clone(st.Attempts) // serialised after the lock is released (C6)
		snap.Services[s.def.Name] = st
	}
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
