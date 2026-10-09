package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/clock"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/config"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/events"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/module"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/notify"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/state"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/model"
)

// logQueueSize is the queue of the subscriber that logs every event.
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
	// Channels holds the counters of every enabled notification channel.
	Channels map[string]notify.Stats
}

// Daemon runs the core services and the configured modules. Start it once
// and Stop it once.
type Daemon struct {
	cfg     *config.Config
	opts    Options
	log     *slog.Logger
	clock   clock.Clock
	mods    []*moduleRun
	routers map[string]module.Router // read-only after New

	started    bool
	state      *state.Dir
	bus        *events.Bus
	dispatcher *notify.Dispatcher
	notifySub  *events.Subscription
	logSub     *events.Subscription
	cancel     context.CancelFunc // ends notification delivery
	workers    sync.WaitGroup     // dispatcher and event logger
	mu         sync.Mutex         // guards moduleRun.state for Snapshot
}

type moduleRun struct {
	name  string
	mod   module.Configured
	state ModuleState
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
	d := &Daemon{cfg: cfg, opts: opts, log: opts.Logger, clock: opts.Clock, routers: map[string]module.Router{}}
	for _, m := range mods {
		d.mods = append(d.mods, &moduleRun{name: m.Name, mod: m.Module, state: ModuleConfigured})
		if r, ok := m.Module.(module.Router); ok {
			d.routers[m.Name] = r
		}
	}
	return d
}

// Start opens the state directory, starts the event bus and the
// notification dispatcher, then starts every module in order. A module
// that fails to start is reported and the others still start; a failure
// of the core services is returned and nothing keeps running. When ctx
// ends (a signal), the module starting gets a cancelled ctx and no
// further module starts.
func (d *Daemon) Start(ctx context.Context) error {
	if d.started {
		return errors.New("daemon: already started")
	}
	d.started = true
	dir, err := state.OpenDir(d.cfg.Daemon.StateDir, d.clock)
	if err != nil {
		return err
	}
	bus := events.NewBus(events.NewRegistry(), events.Options{Hostname: d.opts.Hostname, Version: d.opts.Version, Clock: d.clock})
	dispatcher, err := notify.NewDispatcher(d.cfg.Notifications.Channels, d.route,
		notify.Options{Clock: d.clock, NewSender: d.opts.NewSender})
	if err != nil {
		_ = dir.Close() // the dispatcher error is the one to report
		return err
	}
	d.state, d.bus, d.dispatcher = dir, bus, dispatcher
	d.notifySub = bus.Subscribe(notify.DefaultQueueSize)
	d.logSub = bus.Subscribe(logQueueSize)
	// Delivery has its own context: it must outlive a cancelled start or a
	// signal, so the events of a failing shutdown are still delivered.
	delivery, cancel := context.WithCancel(context.Background())
	d.cancel = cancel
	d.workers.Go(func() {
		_ = dispatcher.Run(delivery, d.notifySub.Events()) // first and only Run: cannot fail
	})
	d.workers.Go(func() { d.logEvents(d.logSub.Events()) })

	for _, m := range d.mods {
		if ctx.Err() != nil {
			d.log.Warn("start interrupted: remaining modules not started", "next", m.name)
			break
		}
		d.start(ctx, m)
	}
	d.log.Info("sentineld ready", "modules", len(d.mods))
	return nil
}

// start starts one module with its runtime. A module whose Start failed
// may have started part of its work, so it gets a bounded Stop.
func (d *Daemon) start(ctx context.Context, m *moduleRun) {
	rt := module.Runtime{
		Logger: d.log.With("module", m.name),
		Clock:  d.clock,
		Events: d.bus.Emitter(m.name),
		State:  func(version int) (*state.Store, error) { return d.state.Store(m.name, version) },
	}
	timeout := d.cfg.Daemon.ShutdownTimeout.Std()
	err := d.call(ctx, m.name, "start", timeout, func(ctx context.Context) error { return m.mod.Start(ctx, rt) })
	switch {
	case err == nil:
		d.setState(m, ModuleRunning)
	case errors.Is(err, errAbandoned):
		d.setState(m, ModuleAbandoned)
	default:
		d.setState(m, ModuleFailed)
		if stopErr := d.call(ctx, m.name, "stop", timeout, m.mod.Stop); stopErr != nil {
			d.setState(m, stateAfter(stopErr))
		}
	}
}

// Stop stops the running modules in reverse start order, then closes the
// bus, lets the dispatcher deliver what is queued and closes the state
// directory, all within daemon.shutdown_timeout: the modules share the
// budget, and a quarter of it is kept for the last deliveries. A module
// that does not stop in its share is abandoned; the state directory then
// stays open, since that module may still use it. Delivery that has not
// finished by the deadline is abandoned too. Stop returns an error naming
// every module that failed or was abandoned, at start or at stop: the
// shutdown was not clean.
func (d *Daemon) Stop(ctx context.Context) error {
	if !d.started || d.bus == nil {
		return nil
	}
	timeout := d.cfg.Daemon.ShutdownTimeout.Std()
	deadline := d.clock.Now().Add(timeout)
	reserve := timeout / 4 // for the last deliveries
	grace := reserve / 2   // for the workers to return once delivery is cancelled
	var running []*moduleRun
	for _, m := range d.mods {
		if d.moduleState(m) == ModuleRunning {
			running = append(running, m)
		}
	}
	for i := len(running) - 1; i >= 0; i-- {
		m := running[i]
		share := max((deadline.Sub(d.clock.Now())-reserve)/time.Duration(i+1), 0)
		d.setState(m, stateAfter(d.call(ctx, m.name, "stop", share, m.mod.Stop)))
	}
	var errs []error
	abandoned := false
	for _, m := range d.mods {
		switch s := d.moduleState(m); s {
		case ModuleFailed, ModuleAbandoned:
			errs = append(errs, fmt.Errorf("module %s %s", m.name, s))
			abandoned = abandoned || s == ModuleAbandoned
		}
	}

	d.bus.Close() // subscriptions end: the workers finish what is queued
	if !d.waitWorkers(ctx, max(deadline.Sub(d.clock.Now())-grace, 0), grace) {
		errs = append(errs, errors.New("daemon: notifications still pending at the shutdown deadline were abandoned"))
	}
	d.log.Info("sentineld stopped", "snapshot", d.Snapshot())
	if abandoned {
		d.log.Warn("state directory left open: a module did not return in time")
	} else if err := d.state.Close(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
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
	s := Snapshot{Modules: map[string]ModuleState{}}
	d.mu.Lock()
	for _, m := range d.mods {
		s.Modules[m.name] = m.state
	}
	d.mu.Unlock()
	if d.dispatcher != nil {
		s.NotifyDropped, s.LogDropped = d.notifySub.Dropped(), d.logSub.Dropped()
		s.Channels = d.dispatcher.Stats()
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
		attrs = append(attrs, "error", d.cfg.RedactText(err.Error()))
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
	if e.Module == model.ModuleCore {
		if d.cfg.Notifications.Core.Matches(e.Type) {
			return d.cfg.Notifications.Core.Channels
		}
		return nil
	}
	r, ok := d.routers[e.Module]
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

// logEvents logs every event at a level matching its severity.
func (d *Daemon) logEvents(evs <-chan model.Event) {
	for e := range evs {
		d.log.Log(context.Background(), logLevel(e.Severity), e.Message,
			"event_id", e.ID, "module", e.Module, "event_type", e.Type,
			"source", e.Source, "source_type", e.SourceType, "severity", e.Severity)
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
