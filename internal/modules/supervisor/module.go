// Package supervisor is the supervisor module (ADR-0015 §4, D-080): it
// checks services, keeps their state across restarts, and emits
// service_failed and service_recovered, routed by each service's
// notifications.
//
// Services are checked over HTTP (http.go); tests give the module fake
// checkers (Options.NewChecker).
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/config"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/module"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/state"
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
	in := &instance{settings: p.settings, warnings: p.warnings, byName: map[string]*service{}}
	if len(p.problems) == 0 {
		for i, def := range p.services {
			c, why := m.checker(def)
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
	st           serviceState
	prev         Health // the health before the last transition
	lastDuration time.Duration
	lastSlotWait time.Duration
	waitingSince time.Time
	stalled      bool
}

// instance is a configured supervisor.
type instance struct {
	settings settings
	services []*service // sorted by name
	byName   map[string]*service
	warnings []config.Problem

	// Set by Start.
	rt      module.Runtime
	log     *slog.Logger
	store   *state.Store
	cancel  context.CancelFunc
	slots   chan struct{} // max_concurrent_checks
	workers sync.WaitGroup
	checks  sync.WaitGroup // check goroutines, stalled ones included
	running atomic.Int64   // checks in flight

	saveMu sync.Mutex // one save at a time

	mu        sync.Mutex // guards the services' runtime fields and what follows
	gen       uint64     // bumped by every change of the state
	savedGen  uint64     // the generation the file holds
	lastSave  time.Time
	notSaved  bool // the last save failed
	startedOK bool
}

// Warnings implements module.Warner.
func (in *instance) Warnings() []config.Problem { return in.warnings }

// NotificationChannels implements module.Router: a service's events go to
// its notifications. It reads only what Configure built, so it never
// blocks.
func (in *instance) NotificationChannels(e model.Event) []string {
	s, ok := in.byName[e.Source]
	if !ok || !s.def.Route.Matches(e.Type) {
		return nil
	}
	return slices.Clone(s.def.Route.Channels)
}

// Start implements module.Configured: it registers the event types, loads
// the state file, reconciles it with the configuration and starts one
// loop per service.
func (in *instance) Start(_ context.Context, rt module.Runtime) error {
	in.rt, in.log = rt, rt.Logger
	if err := rt.Events.Register(eventSpecs...); err != nil {
		return err
	}
	store, err := rt.State(stateVersion)
	if err != nil {
		return err
	}
	kept, report, err := state.Load[*fileState](store)
	if err != nil { // another schema version: left untouched (D-015)
		return err
	}
	if report.Recovered {
		in.log.Warn("supervisor state file was corrupt; moved aside, starting fresh", "moved_to", report.QuarantinedTo)
	}
	in.store = store
	in.mu.Lock()
	if reconcile(kept, in.services) || report.FirstRun || report.Recovered {
		in.gen++
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
	ctx, cancel := context.WithCancel(context.Background()) // the module's own lifetime
	in.cancel = cancel
	in.slots = make(chan struct{}, in.settings.MaxConcurrentChecks)
	for _, s := range in.services {
		in.workers.Add(1)
		go in.run(ctx, s)
	}
	in.startedOK = true
	return nil
}

// Stop implements module.Configured: it ends the loops, saves the state,
// then waits for the checks still running. It never leaves a check
// behind: one that ignores its cancellation keeps Stop waiting until ctx
// ends, and the daemon then treats the instance as not stopped (D-078).
func (in *instance) Stop(ctx context.Context) error {
	if !in.startedOK {
		return nil
	}
	in.cancel()
	if !waitGroup(ctx, &in.workers) {
		return errors.New("supervisor: service loops did not end in time")
	}
	in.mu.Lock()
	gen := in.gen
	in.mu.Unlock()
	in.save(gen)
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

// save writes the state if generation gen is not on disk yet. Saves are
// serialised; one that finds a newer save already covered gen returns, so
// a burst of transitions may cost fewer writes. A failed save is logged
// and shown in status; supervision goes on, and the next save retries.
func (in *instance) save(gen uint64) {
	in.saveMu.Lock()
	defer in.saveMu.Unlock()
	in.mu.Lock()
	if gen <= in.savedGen {
		in.mu.Unlock()
		return
	}
	snap := fileState{Services: make(map[string]serviceState, len(in.services))}
	for _, s := range in.services {
		snap.Services[s.def.Name] = s.st
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
	if err != nil {
		in.log.Error("supervisor state not saved; supervision goes on and the next change retries", "error", err)
	}
}
