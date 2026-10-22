package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/audit"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/authz"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/clock"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/config"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/events"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/module"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/notify"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/state"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/transport"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/platform/peercred"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/api"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/model"
)

// logQueueSize is the queue of the subscriber that logs every event and
// keeps the recent ones (core.events).
const logQueueSize = 256

// Options configure a Daemon.
type Options struct {
	// Logger receives the daemon's and the modules' logs; defaults to
	// slog.Default().
	Logger *slog.Logger
	// Clock times module calls and the shutdown; defaults to clock.Real().
	Clock clock.Clock
	// Hostname and Version are stamped on every event.
	Hostname, Version string
	// NewSender builds a notification channel's sender; nil means the
	// real webhook. Tests replace it to observe deliveries.
	NewSender func(config.Channel) (notify.Sender, error)
	// Modules lists every module the binary knows with its availability
	// (module.Registry.Availability), for core.modules.
	Modules map[string]config.ModuleAvailability
	// Commands lists the control-socket commands of every module the
	// binary knows (module.Registry.Commands), enabled or not: the socket
	// binds them once, and a reload may start a module later.
	Commands map[string][]api.Command
	// PeerCredentials reads a control-socket peer's identity; nil means the
	// kernel's (internal/platform/peercred). Tests replace it to act as
	// another user.
	PeerCredentials func(*net.UnixConn) (authz.Peer, error)
	// LookupGroup resolves daemon.socket_group and daemon.access groups;
	// nil means the system's (authz.LookupGroupSystem).
	LookupGroup authz.LookupGroup
	// Load reads the configuration again for a reload, as Load did at
	// start (the same files and registry); nil means a reload is not
	// supported. Tests give configurations directly.
	Load func() (*config.Config, []module.Instance, error)
	// LogLevel is the level of Logger, which a reload changes in place;
	// nil makes daemon.log.level a restart-required key.
	LogLevel *slog.LevelVar
}

// ModuleState is where a module is in its lifecycle.
type ModuleState string

// Module states.
const (
	ModuleConfigured ModuleState = "configured" // not started yet
	ModuleRunning    ModuleState = "running"
	ModuleFailed     ModuleState = "failed"    // Start or Stop failed or panicked
	ModuleAbandoned  ModuleState = "abandoned" // Start or Stop did not return in time
	ModuleStopped    ModuleState = "stopped"
)

// Snapshot is a copy of the daemon's counters, for logs and status.
type Snapshot struct {
	Modules map[string]ModuleState
	// NotifyDropped and LogDropped count events lost by the notification
	// and the logging subscribers because their queue was full.
	NotifyDropped, LogDropped uint64
	// Channels holds the counters of every enabled notification channel,
	// since the last reload that changed the channels.
	Channels map[string]notify.Stats
	// NotifyAbandoned counts deliveries queued when a reload replaced the
	// channels: they were not sent.
	NotifyAbandoned uint64
}

// Daemon runs the core services and the configured modules. Start it once
// and Stop it once; Reload in between.
type Daemon struct {
	opts  Options
	log   *slog.Logger
	clock clock.Clock

	// gate serialises Reload and Stop: one changes the modules at a time.
	// A channel, so that waiting for it honours a context.
	gate chan struct{}
	// preparing is closed when a preparation that a reload gave up on
	// returns; nil when none runs. Guarded by gate.
	preparing chan struct{}

	started   bool
	stopped   bool
	startedAt time.Time
	server    *transport.Server
	state     *state.Dir
	audit     *audit.Log
	// recorder is the audit log as every runtime writer uses it (the
	// control socket, Reload): one failure is published once, whoever
	// finds it.
	recorder  *auditRecorder
	bus       *events.Bus
	notifySub *events.Subscription
	logSub    *events.Subscription
	recent    *recentEvents      // fed by the log subscriber, for core.events
	swaps     chan swap          // to the forwarder: replace the dispatcher
	forwarded chan struct{}      // closed when the forwarder returns
	delivery  context.Context    // every dispatcher's; outlives a signal
	cancel    context.CancelFunc // ends notification delivery
	workers   sync.WaitGroup     // forwarder, dispatchers and event logger

	mu        sync.Mutex // guards mods, moduleRun.state and .unstopped, gen and stopping
	mods      []*moduleRun
	gen       *generation // the dispatcher receiving events
	stopping  bool
	abandoned atomic.Uint64 // deliveries abandoned by reloads

	viewMu sync.RWMutex
	view   view // replaced as a whole by a reload
}

// view is what a reload publishes at once. Readers copy it (current) and
// never hold viewMu while they encode, route or log.
type view struct {
	cfg        *config.Config // the effective configuration
	configJSON []byte         // core.config_show: cfg, redacted
	routers    map[string]module.Router
	lastReload *api.ReloadInfo
}

// generation is one dispatcher and the channel the forwarder feeds it.
type generation struct {
	dispatcher *notify.Dispatcher
	in         chan model.Event
	ctx        context.Context
	cancel     context.CancelFunc
	done       chan struct{} // closed when Run returns
	retired    atomic.Bool   // replaced by a reload: its abandoned deliveries count
}

// swap asks the forwarder to feed next and to hand back the generation it
// replaced.
type swap struct {
	next *generation
	prev chan *generation
}

type moduleRun struct {
	name  string
	mod   module.Configured
	state ModuleState
	// unstopped: a Stop failed or did not return, so the instance may
	// still run whatever its state says; no reload starts another beside
	// it (afterStop).
	unstopped bool
	// closing: Stop has begun, so the instance takes no more commands;
	// calls counts the commands in flight, which Stop waits for. Adding
	// to calls happens under d.mu and only while closing is false, so
	// every Add happens before stop's Wait.
	closing bool
	calls   sync.WaitGroup
}

// New prepares a daemon for cfg and the configured modules, in start
// order. It starts nothing.
func New(cfg *config.Config, mods []module.Instance, opts Options) *Daemon {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Clock == nil {
		opts.Clock = clock.Real()
	}
	if opts.PeerCredentials == nil {
		opts.PeerCredentials = peercred.Read
	}
	if opts.LookupGroup == nil {
		opts.LookupGroup = authz.LookupGroupSystem
	}
	d := &Daemon{opts: opts, log: opts.Logger, clock: opts.Clock, gate: make(chan struct{}, 1), recent: newRecentEvents()}
	d.recorder = &auditRecorder{d: d}
	for _, m := range mods {
		d.mods = append(d.mods, &moduleRun{name: m.Name, mod: m.Module, state: ModuleConfigured})
	}
	d.view = view{cfg: cfg, routers: routersOf(d.mods)}
	return d
}

// routersOf returns the Router of every module that has one.
func routersOf(runs []*moduleRun) map[string]module.Router {
	routers := map[string]module.Router{}
	for _, m := range runs {
		if r, ok := m.mod.(module.Router); ok {
			routers[m.name] = r
		}
	}
	return routers
}

// current returns the published view.
func (d *Daemon) current() view {
	d.viewMu.RLock()
	defer d.viewMu.RUnlock()
	return d.view
}

// config returns the effective configuration.
func (d *Daemon) config() *config.Config { return d.current().cfg }

// Start resolves the access groups and acquires the control socket, opens
// the state directory, starts the event bus and the notification
// dispatcher, then starts every module in order and serves the socket. A module
// that fails to start is reported and the others still start; a failure
// of the core services is returned and nothing keeps running. When ctx
// ends (a signal), the module starting gets a cancelled ctx and no
// further module starts.
func (d *Daemon) Start(ctx context.Context) error {
	if d.started {
		return errors.New("daemon: already started")
	}
	d.started = true
	d.startedAt = d.clock.Now()
	cfg := d.config()
	configJSON, err := json.Marshal(cfg.Redacted())
	if err != nil {
		return fmt.Errorf("daemon: encode the redacted configuration: %w", err)
	}
	d.viewMu.Lock()
	d.view.configJSON = configJSON
	d.viewMu.Unlock()
	dir, err := state.OpenDir(cfg.Daemon.StateDir, d.clock)
	if err != nil {
		return err
	}
	// The socket's lock first: a second sentineld stops here, before the
	// audit check, which may update the head the running daemon writes.
	server, err := d.listen()
	if err != nil {
		_ = dir.Close() // the socket error is the one to report
		return err
	}
	if d.audit, err = openAudit(dir, d); err != nil {
		// The audit error is the one to report.
		_ = server.Close(context.Background())
		_ = dir.Close()
		return err
	}
	bus := events.NewBus(events.NewRegistry(), events.Options{Hostname: d.opts.Hostname, Version: d.opts.Version, Clock: d.clock})
	dispatcher, err := d.newDispatcher(cfg)
	if err != nil {
		// The dispatcher error is the one to report.
		_ = server.Close(context.Background())
		_ = d.audit.Close(context.Background())
		_ = dir.Close()
		return err
	}
	d.server, d.state, d.bus = server, dir, bus
	d.notifySub = bus.Subscribe(notify.DefaultQueueSize)
	d.logSub = bus.Subscribe(logQueueSize)
	// Delivery has its own context: it must outlive a cancelled start or a
	// signal, so the events of a failing shutdown are still delivered.
	delivery, cancel := context.WithCancel(context.Background())
	d.delivery, d.cancel = delivery, cancel
	gen := newGeneration(delivery, dispatcher)
	d.runGeneration(gen)
	d.mu.Lock()
	d.gen = gen
	d.mu.Unlock()
	d.swaps, d.forwarded = make(chan swap), make(chan struct{})
	d.workers.Go(func() {
		defer close(d.forwarded)
		d.forward(d.notifySub.Events(), gen)
	})
	d.workers.Go(func() { d.logEvents(d.logSub.Events()) })
	d.reportAudit()

	for _, m := range d.mods {
		if ctx.Err() != nil {
			d.log.Warn("start interrupted: remaining modules not started", "next", m.name)
			break
		}
		d.start(ctx, m, cfg, func() time.Duration { return cfg.Daemon.ShutdownTimeout.Std() })
	}
	d.server.Serve()
	d.log.Info("sentineld ready", "modules", len(d.mods), "socket", cfg.Daemon.Socket, "config_digest", cfg.Digest)
	return nil
}

// newDispatcher builds the dispatcher of cfg's channels.
func (d *Daemon) newDispatcher(cfg *config.Config) (*notify.Dispatcher, error) {
	return notify.NewDispatcher(cfg.Notifications.Channels, d.route,
		notify.Options{Clock: d.clock, NewSender: d.opts.NewSender})
}

// newGeneration prepares dispatcher as a new generation, on a context of
// its own below delivery: a reload cancels one generation, Stop all of
// them. It does not run yet (runGeneration).
func newGeneration(delivery context.Context, dispatcher *notify.Dispatcher) *generation {
	ctx, cancel := context.WithCancel(delivery)
	return &generation{dispatcher: dispatcher, in: make(chan model.Event), ctx: ctx, cancel: cancel, done: make(chan struct{})}
}

// runGeneration runs g's dispatcher as one of the workers. After Start,
// only the forwarder calls it, while it is itself a running worker: no
// worker is added once Stop waits for them.
func (d *Daemon) runGeneration(g *generation) {
	d.workers.Go(func() {
		defer close(g.done)
		_ = g.dispatcher.Run(g.ctx, g.in) // first and only Run: cannot fail
		// Counted here, once the workers are done, however late: a reload
		// gives up waiting for a dispatcher held by a slow delivery.
		if g.retired.Load() {
			for _, st := range g.dispatcher.Stats() {
				d.abandoned.Add(st.Abandoned)
			}
		}
	})
}

// forward feeds the notification subscription to the current dispatcher.
// A reload replaces the dispatcher through d.swaps; forward closes the old
// one's input, so it is the only sender on it. When the bus closes, the
// current dispatcher's input is closed: it delivers what is queued.
func (d *Daemon) forward(events <-chan model.Event, gen *generation) {
	for {
		select {
		case e, ok := <-events:
			if !ok {
				close(gen.in)
				return
			}
			gen = d.deliver(e, gen)
		case s := <-d.swaps:
			gen = d.swapTo(gen, s)
		}
	}
}

// deliver hands e to gen's dispatcher and returns the generation now fed.
// A swap asked meanwhile is served at once and e goes to the new
// dispatcher: a dispatcher held by a blocking router must not hold a
// reload as well.
func (d *Daemon) deliver(e model.Event, gen *generation) *generation {
	for {
		select {
		case gen.in <- e:
			return gen
		case <-gen.done: // cancelled at the shutdown deadline: dropped
			return gen
		case s := <-d.swaps:
			gen = d.swapTo(gen, s)
		}
	}
}

// swapTo retires prev, runs the next generation, hands prev back to the
// reload and returns the generation to feed. prev is cancelled before its
// input closes, which would otherwise let it deliver its queue to the
// channels being replaced: it abandons its queue instead (D-077).
func (d *Daemon) swapTo(prev *generation, s swap) *generation {
	prev.retired.Store(true)
	prev.cancel()
	close(prev.in)
	d.runGeneration(s.next)
	s.prev <- prev // buffered: never blocks
	return s.next
}

// start starts one module, configured from cfg, with its runtime. A
// module whose Start failed may have started part of its work, so it gets
// a bounded Stop. timeout gives each call its limit, asked again before
// the Stop: a reload's budget shrinks while Start runs.
func (d *Daemon) start(ctx context.Context, m *moduleRun, cfg *config.Config, timeout func() time.Duration) {
	rt := module.Runtime{
		Logger: d.log.With("module", m.name),
		Clock:  d.clock,
		Events: d.bus.Emitter(m.name),
		State:  func(version int) (*state.Store, error) { return d.state.Store(m.name, version) },
		// The secrets of the configuration the module was configured
		// from: an instance kept across a reload keeps its own.
		Redact: cfg.RedactText,
	}
	err := d.call(ctx, m.name, "start", timeout(), func(ctx context.Context) error { return m.mod.Start(ctx, rt) })
	switch {
	case err == nil:
		d.setState(m, ModuleRunning)
	case errors.Is(err, errAbandoned):
		d.setState(m, ModuleAbandoned)
	default:
		d.setState(m, ModuleFailed)
		if stopErr := d.stop(ctx, m, timeout()); stopErr != nil {
			d.afterStop(m, stopErr)
		}
	}
}

// stop stops m within timeout. It closes m's commands first and waits for
// those in flight inside the same call, so Stop never runs beside Handle;
// a command that outlives timeout makes the stop abandoned. The commands
// end with their exchange, so the wait is bounded even when the call is
// abandoned.
func (d *Daemon) stop(ctx context.Context, m *moduleRun, timeout time.Duration) error {
	d.mu.Lock()
	m.closing = true
	d.mu.Unlock()
	return d.call(ctx, m.name, "stop", timeout, func(ctx context.Context) error {
		m.calls.Wait()
		return m.mod.Stop(ctx)
	})
}

// Stop stops the running modules in reverse start order, then closes the
// bus, lets the dispatcher deliver what is queued and closes the state
// directory, all within daemon.shutdown_timeout: the modules share the
// budget, and a quarter of it is kept for the last deliveries. A module
// that does not stop in its share is abandoned; the state directory then
// stays open, since that module may still use it. Delivery that has not
// finished by the deadline is abandoned too. Stop returns an error naming
// every module that failed or was abandoned, at start or at stop: the
// shutdown was not clean. A second Stop does nothing: in particular it
// never releases a lock the first one kept.
func (d *Daemon) Stop(ctx context.Context) error {
	if !d.started || d.bus == nil || d.stopped {
		return nil
	}
	d.stopped = true
	d.mu.Lock()
	d.stopping = true // a reload waiting for the gate gives up
	d.mu.Unlock()
	timeout := d.config().Daemon.ShutdownTimeout.Std()
	deadline := d.clock.Now().Add(timeout)
	reserve := timeout / 4 // for the last deliveries
	grace := reserve / 2   // for the workers to return once delivery is cancelled
	// The control socket goes first: no new request while modules stop,
	// and a reload asked through it is cancelled. Its lock is kept until
	// the end, so a second sentineld cannot start beside modules still
	// stopping or an audit log still open.
	closing, cancel := context.WithTimeout(ctx, timeout/8)
	serverErr := d.server.Shutdown(closing)
	cancel()
	errs := []error{serverErr}
	// A reload in progress finishes first (its module calls are bounded);
	// one that does not within the budget leaves the modules to it.
	// mayRun: something may still change the state beside a new sentineld.
	gateLost := !d.waitGate(ctx, max(deadline.Sub(d.clock.Now())-reserve, 0))
	mayRun := gateLost
	if gateLost {
		errs = append(errs, errors.New("daemon: a reload did not finish before the shutdown deadline"))
	} else {
		defer func() { <-d.gate }()
		d.stopModules(ctx, deadline, reserve)
	}
	for _, m := range d.runs() {
		switch s := d.moduleState(m); s {
		case ModuleFailed, ModuleAbandoned:
			errs = append(errs, fmt.Errorf("module %s %s", m.name, s))
		}
		mayRun = mayRun || d.mayRun(m) // abandoned, or a Stop that failed
	}

	d.bus.Close() // subscriptions end: the workers finish what is queued
	if !d.waitWorkers(ctx, max(deadline.Sub(d.clock.Now())-grace, 0), grace) {
		errs = append(errs, errors.New("daemon: notifications still pending at the shutdown deadline were abandoned"))
	}
	// After the modules: a module that may still run gets ErrClosed.
	if n := d.abandoned.Load(); n > 0 {
		d.log.Info("notifications abandoned by reloads", "count", n)
	}
	closing, cancel = context.WithTimeout(ctx, max(deadline.Sub(d.clock.Now()), 0))
	auditErr := d.audit.Close(closing)
	cancel()
	if auditErr != nil {
		errs = append(errs, fmt.Errorf("daemon: close the audit log: %w", auditErr))
	}
	d.log.Info("sentineld stopped", "snapshot", d.Snapshot())
	if mayRun {
		d.log.Warn("state directory left open: a module may still run")
	} else if err := d.state.Close(); err != nil {
		errs = append(errs, err)
	}
	// The lock goes last, and only when nothing is left running: a module
	// that may still run or an audit record still being written could
	// change the state beside a new sentineld. Otherwise it is released when the
	// process exits.
	if mayRun || auditErr != nil {
		d.log.Warn("control socket lock kept: something may still write the state directory")
	} else {
		d.server.Release()
	}
	return errors.Join(errs...)
}

// stopModules stops the running modules in reverse order, sharing the time
// until deadline less reserve. The caller holds the gate.
func (d *Daemon) stopModules(ctx context.Context, deadline time.Time, reserve time.Duration) {
	var running []*moduleRun
	for _, m := range d.runs() {
		if d.moduleState(m) == ModuleRunning {
			running = append(running, m)
		}
	}
	for i := len(running) - 1; i >= 0; i-- {
		m := running[i]
		share := max((deadline.Sub(d.clock.Now())-reserve)/time.Duration(i+1), 0)
		d.afterStop(m, d.stop(ctx, m, share))
	}
}

// waitGate takes the gate, waiting at most wait or until ctx ends.
func (d *Daemon) waitGate(ctx context.Context, wait time.Duration) bool {
	select {
	case d.gate <- struct{}{}:
		return true
	default:
	}
	timer := d.clock.NewTimer(wait)
	defer timer.Stop()
	select {
	case d.gate <- struct{}{}:
		return true
	case <-timer.C():
		return false
	case <-ctx.Done():
		return false
	}
}

// runs returns a copy of the module list.
func (d *Daemon) runs() []*moduleRun {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.mods)
}

// waitWorkers waits up to wait for the dispatcher and the event logger to
// finish their queues, and reports whether they did. Past it (or when ctx
// ends) delivery is cancelled and the workers get grace to return; a
// worker blocked elsewhere (a module's router, a full log pipe) is then
// abandoned, as an abandoned module call is.
func (d *Daemon) waitWorkers(ctx context.Context, wait, grace time.Duration) bool {
	done := make(chan struct{})
	go func() { d.workers.Wait(); close(done) }()
	inTime := d.waitFor(ctx, done, wait)
	d.cancel()
	if !inTime {
		d.waitFor(context.Background(), done, grace)
	}
	return inTime
}

// waitFor reports whether done closed within wait and before ctx ended.
func (d *Daemon) waitFor(ctx context.Context, done <-chan struct{}, wait time.Duration) bool {
	timer := d.clock.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C():
		return false
	case <-ctx.Done():
		return false
	}
}

// Snapshot returns a copy of the daemon's counters. It is safe to call
// concurrently with Stop and with the modules' work, once Start has
// returned.
func (d *Daemon) Snapshot() Snapshot {
	s := Snapshot{Modules: map[string]ModuleState{}, NotifyAbandoned: d.abandoned.Load()}
	d.mu.Lock()
	for _, m := range d.mods {
		s.Modules[m.name] = m.state
	}
	gen := d.gen
	d.mu.Unlock()
	if gen != nil {
		s.NotifyDropped, s.LogDropped = d.notifySub.Dropped(), d.logSub.Dropped()
		s.Channels = gen.dispatcher.Stats()
	}
	return s
}

// report logs a failed module call and publishes a daemon_error. The event
// leaves the host, so it carries what failed and how, never the module's
// error text or a panic's frames: those stay in the local log.
func (d *Daemon) report(name, op string, timeout time.Duration, err error) {
	failure, message := "error", fmt.Sprintf("module %s: %s failed", name, op)
	attrs := []any{"module", name, "operation", op}
	var pe *panicError
	switch {
	case errors.As(err, &pe):
		failure, message = "panic", fmt.Sprintf("module %s: %s panicked", name, op)
		attrs = append(attrs, "frames", pe.frames)
	case errors.Is(err, errAbandoned):
		failure, message = "timeout", fmt.Sprintf("module %s: %s did not return within %s", name, op, timeout)
	default:
		attrs = append(attrs, "error", d.config().RedactText(err.Error()))
	}
	d.log.Error(message, append(attrs, "failure", failure)...)
	_, perr := d.bus.Publish(model.Event{
		Module: model.ModuleCore, Type: model.EventDaemonError,
		Source: name, SourceType: model.SourceTypeModule, Message: message,
		Attributes: map[string]any{"operation": op, "failure": failure},
	})
	if perr != nil {
		d.log.Error("daemon_error not published", "module", name, "error", perr)
	}
}

// route tells the dispatcher where an event goes: core events follow
// notifications.core, a module's events follow its Router. A panicking
// Router must not take the dispatcher down: its event is only logged.
func (d *Daemon) route(e model.Event) (channels []string) {
	v := d.current()
	if e.Module == model.ModuleCore {
		if v.cfg.Notifications.Core.Matches(e.Type) {
			return v.cfg.Notifications.Core.Channels
		}
		return nil
	}
	r, ok := v.routers[e.Module]
	if !ok {
		return nil
	}
	defer func() {
		if recover() != nil {
			channels = nil
			d.log.Error("notification route panicked; event only logged", "module", e.Module, "event_id", e.ID)
		}
	}()
	return r.NotificationChannels(e)
}

// logEvents logs every event at a level matching its severity and keeps
// it for core.events.
func (d *Daemon) logEvents(evs <-chan model.Event) {
	for e := range evs {
		d.log.Log(context.Background(), logLevel(e.Severity), e.Message,
			"event_id", e.ID, "module", e.Module, "event_type", e.Type,
			"source", e.Source, "source_type", e.SourceType, "severity", e.Severity)
		d.recent.add(e)
	}
}

func logLevel(s model.Severity) slog.Level {
	switch s {
	case model.SeverityInfo:
		return slog.LevelInfo
	case model.SeverityWarning:
		return slog.LevelWarn
	default:
		return slog.LevelError
	}
}

func stateAfter(err error) ModuleState {
	switch {
	case err == nil:
		return ModuleStopped
	case errors.Is(err, errAbandoned):
		return ModuleAbandoned
	default:
		return ModuleFailed
	}
}

// afterStop records the outcome of m's Stop. An error or no return leaves
// the instance possibly running, which a reload never starts beside.
func (d *Daemon) afterStop(m *moduleRun, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	m.state = stateAfter(err)
	m.unstopped = m.unstopped || err != nil
}

// mayRun reports whether m's instance may still run after a Stop that
// failed, or after a Start or Stop that did not return.
func (d *Daemon) mayRun(m *moduleRun) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return m.unstopped || m.state == ModuleAbandoned
}

func (d *Daemon) setState(m *moduleRun, s ModuleState) {
	d.mu.Lock()
	defer d.mu.Unlock()
	m.state = s
}

func (d *Daemon) moduleState(m *moduleRun) ModuleState {
	d.mu.Lock()
	defer d.mu.Unlock()
	return m.state
}
