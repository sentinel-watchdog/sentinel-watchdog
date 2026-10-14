package daemon

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/audit"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/authz"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/config"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/logging"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/module"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/notify"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/transport"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/api"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/model"
)

// reloadCommand is core.reload, run by the control socket for peer.
func (d *Daemon) reloadCommand(ctx context.Context, peer authz.Peer, args jsontext.Value) (any, *api.Error) {
	run := noArgs(func() (any, *api.Error) {
		uid, gid := peer.UID, peer.GID
		return d.Reload(ctx, audit.Actor{Kind: audit.ActorSocket, UID: &uid, GID: &gid, PID: peer.PID})
	})
	return run(ctx, peer, args)
}

// reloadBudget bounds a whole reload, waiting for the gate included: each
// module call has daemon.shutdown_timeout, the reload twice that. Past it,
// the modules not started yet are reported as failed. A SIGHUP's context
// never ends: without the budget, a reload of many slow modules would have
// no limit.
func reloadBudget(cfg *config.Config) time.Duration { return 2 * cfg.Daemon.ShutdownTimeout.Std() }

// reloadExchange is how long the control socket holds a core.reload
// exchange: the budget of the longest shutdown_timeout, since a reload can
// raise it after the socket was opened, and the time to answer. Reload
// itself ends within its own budget.
const reloadExchange = 2*config.MaxShutdownTimeout + transport.DefaultTimeout

// plan is what a reload will do, prepared before any effect.
type plan struct {
	cfg        *config.Config // effective: restart-required keys kept
	configJSON []byte
	instances  map[string]module.Instance
	order      []string // the new start order
	policy     authz.Policy
	dispatcher *notify.Dispatcher // nil: the channels did not change
	level      string             // "" : unchanged
	warnings   []string
}

// Reload reads the configuration again and applies it (D-075): the whole
// configuration is validated first, and on any problem nothing changes
// (CodeInvalidConfig). Otherwise only the modules whose configuration
// changed are stopped and started again; daemon.access applies to the next
// requests, daemon.log.level at once, a change of the channels replaces
// the dispatcher (its queue is abandoned and counted, D-077), and keys
// bound at start-up (socket, its mode and group, state_dir, log.format)
// keep their value with a warning. Reloads and Stop run one at a time; a
// reload during start-up or shutdown is refused. The operation is audited
// (core.config_reload, with the old and new digests) before anything
// changes, for the socket and for SIGHUP alike, and has one time budget
// (reloadBudget).
func (d *Daemon) Reload(ctx context.Context, by audit.Actor) (api.ReloadResult, *api.Error) {
	if d.opts.Load == nil {
		return api.ReloadResult{}, &api.Error{Code: api.CodeUnsupported, Message: "this daemon cannot reload its configuration"}
	}
	ctx, cancel := context.WithTimeout(ctx, reloadBudget(d.config()))
	defer cancel()
	select {
	case d.gate <- struct{}{}:
	case <-ctx.Done():
		return api.ReloadResult{}, &api.Error{Code: api.CodeUnavailable, Message: "another reload or the shutdown did not finish in time"}
	}
	defer func() { <-d.gate }()
	d.mu.Lock()
	ready := d.started && d.bus != nil && !d.stopping
	d.mu.Unlock()
	if !ready {
		return api.ReloadResult{}, &api.Error{Code: api.CodeUnavailable, Message: "sentineld is starting or stopping"}
	}

	old := d.config()
	result := api.ReloadResult{OldDigest: old.Digest, Digest: old.Digest}
	correlation := rand.Text()
	if _, err := d.audit.Record(ctx, audit.Entry{Actor: by, Action: "core.config_reload", Result: audit.ResultIntent,
		CorrelationID: correlation, Detail: map[string]string{"old_digest": old.Digest}}); err != nil {
		d.log.Error("reload refused: the audit log cannot record it", "error", err)
		message := "the audit log cannot record the reload; nothing changed"
		if errors.Is(err, audit.ErrBroken) {
			message = "the audit chain is broken; the reload is refused until the evidence is restored"
		}
		return result, &api.Error{Code: api.CodeUnavailable, Message: message}
	}

	p, err := d.prepareWithin(ctx, old)
	if errors.Is(err, errPreparing) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		d.log.Warn("configuration reload interrupted while reading the configuration", "error", err)
		d.outcome(ctx, by, correlation, audit.ResultFailed, map[string]string{"reason": "interrupted", "old_digest": old.Digest})
		return result, &api.Error{Code: api.CodeUnavailable, Message: err.Error() + "; nothing changed"}
	}
	if err != nil {
		d.log.Warn("configuration reload rejected; the running configuration is kept", "problems", err.Error())
		d.publishCore(model.EventConfigReloadFailed, "configuration reload rejected: the running configuration is kept",
			map[string]any{"result": "rejected"})
		d.outcome(ctx, by, correlation, audit.ResultFailed, map[string]string{"reason": "invalid_configuration", "old_digest": old.Digest})
		d.setLastReload("rejected", "")
		return result, &api.Error{Code: api.CodeInvalidConfig, Message: err.Error()}
	}

	// A Stop that gave up waiting for this reload may have ended the
	// services meanwhile: change nothing then.
	d.mu.Lock()
	stopping := d.stopping
	d.mu.Unlock()
	if stopping || ctx.Err() != nil {
		d.outcome(ctx, by, correlation, audit.ResultFailed, map[string]string{"reason": "interrupted", "old_digest": old.Digest})
		return result, &api.Error{Code: api.CodeUnavailable, Message: "the reload was interrupted (shutdown or time limit); nothing changed"}
	}

	d.apply(ctx, old, p, &result)
	result.Digest = p.cfg.Digest
	outcome, info := audit.ResultOK, "ok"
	if len(result.Failed) > 0 {
		outcome, info = audit.ResultFailed, "partial"
	}
	d.outcome(ctx, by, correlation, outcome, map[string]string{
		"old_digest": old.Digest, "new_digest": p.cfg.Digest, "restarted": bounded(strings.Join(result.Restarted, ","))})
	d.setLastReload(info, p.cfg.Digest)
	attrs := map[string]any{"digest": p.cfg.Digest, "restarted": len(result.Restarted), "started": len(result.Started),
		"stopped": len(result.Stopped), "failed": len(result.Failed), "warnings": len(result.Warnings)}
	if info == "ok" {
		d.publishCore(model.EventConfigReloaded, "configuration reloaded", attrs)
	} else {
		attrs["result"] = info
		d.publishCore(model.EventConfigReloadFailed, "configuration reloaded in part: modules failed", attrs)
	}
	d.log.Info("configuration reloaded", "digest", p.cfg.Digest, "restarted", result.Restarted, "started", result.Started,
		"stopped", result.Stopped, "failed", result.Failed, "warnings", result.Warnings)
	return result, nil
}

// prepare loads and checks the new configuration and builds what a reload
// needs, without changing anything: a problem here keeps the running
// configuration.
func (d *Daemon) prepare(old *config.Config) (plan, error) {
	cfg, instances, err := d.opts.Load()
	if err != nil {
		if cfg != nil { // the files loaded, a module refused its configuration
			err = redacted(cfg, err)
		}
		return plan{}, err
	}
	p, err := d.build(old, cfg, instances)
	if err != nil {
		// Only the new configuration knows the values it expanded (an
		// access group, a channel's header): the running one cannot redact
		// them.
		return plan{}, redacted(cfg, err)
	}
	return p, nil
}

// redacted returns err's text with cfg's secrets masked (Config.RedactText).
func redacted(cfg *config.Config, err error) error {
	return errors.New(cfg.RedactText(err.Error()))
}

// build turns a loaded configuration into a plan: access policy, log
// level, dispatcher and redacted JSON.
func (d *Daemon) build(old, cfg *config.Config, instances []module.Instance) (plan, error) {
	lookup := d.opts.LookupGroup
	if lookup == nil {
		lookup = authz.LookupGroupSystem
	}
	policy, err := authz.NewPolicy(cfg.Daemon.Access, lookup)
	if err != nil {
		return plan{}, err
	}
	p := plan{cfg: cfg, policy: policy, instances: map[string]module.Instance{}}
	for _, inst := range instances {
		p.instances[inst.Name] = inst
		p.order = append(p.order, inst.Name)
	}
	p.keepBound(old, d.opts.LogLevel != nil)
	if cfg.Daemon.Log.Level != old.Daemon.Log.Level && d.opts.LogLevel != nil {
		p.level = cfg.Daemon.Log.Level
		if _, err := logging.ParseLevel(p.level); err != nil {
			return plan{}, err
		}
	}
	if !reflect.DeepEqual(cfg.Notifications.Channels, old.Notifications.Channels) {
		if p.dispatcher, err = d.newDispatcher(cfg); err != nil {
			return plan{}, err
		}
	}
	if p.configJSON, err = json.Marshal(cfg.Redacted()); err != nil {
		return plan{}, fmt.Errorf("daemon: encode the redacted configuration: %w", err)
	}
	return p, nil
}

// errPreparing refuses a reload while an earlier preparation still runs.
var errPreparing = errors.New("an earlier reload is still reading the configuration")

// prepareWithin runs prepare until ctx ends. Reading files and resolving
// groups (NSS) cannot be interrupted, so a preparation that outlasts ctx
// is left to finish on its own and its result is dropped; until it
// returns, every reload is refused (errPreparing), so stalled
// preparations never pile up. The caller holds the gate.
func (d *Daemon) prepareWithin(ctx context.Context, old *config.Config) (plan, error) {
	if d.preparing != nil {
		select {
		case <-d.preparing:
			d.preparing = nil
		default:
			return plan{}, errPreparing
		}
	}
	type prepared struct {
		p   plan
		err error
	}
	result := make(chan prepared, 1) // the late result is dropped
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		p, err := d.prepare(old)
		result <- prepared{p, err}
	}()
	select {
	case r := <-result:
		return r.p, r.err
	case <-ctx.Done():
		d.preparing = finished
		return plan{}, fmt.Errorf("reading the configuration: %w", ctx.Err())
	}
}

// keepBound keeps the keys bound at start-up at their running value, with
// a warning when the file changed them: the socket, the state directory
// and the log format are opened once.
func (p *plan) keepBound(old *config.Config, liveLevel bool) {
	n, o := &p.cfg.Daemon, old.Daemon
	keep := func(key string, changed bool, restore func()) {
		if changed {
			p.warnings = append(p.warnings, "daemon."+key+" changed: restart sentineld to apply it")
			restore()
		}
	}
	keep("socket", n.Socket != o.Socket, func() { n.Socket = o.Socket })
	keep("socket_mode", n.SocketMode != o.SocketMode, func() { n.SocketMode = o.SocketMode })
	keep("socket_group", n.SocketGroup != o.SocketGroup, func() { n.SocketGroup = o.SocketGroup })
	keep("state_dir", n.StateDir != o.StateDir, func() { n.StateDir = o.StateDir })
	keep("log.format", n.Log.Format != o.Log.Format, func() { n.Log.Format = o.Log.Format })
	if !liveLevel {
		keep("log.level", n.Log.Level != o.Log.Level, func() { n.Log.Level = o.Log.Level })
	}
}

// apply carries out p, filling result. The caller holds the gate. Modules
// whose configuration changed are stopped (old start order reversed) and
// started again (new order); a module whose old instance did not stop
// cleanly is never started beside it. Unchanged modules keep running.
func (d *Daemon) apply(ctx context.Context, old *config.Config, p plan, result *api.ReloadResult) {
	result.Warnings = append(result.Warnings, p.warnings...)
	retimed := p.cfg.Daemon.Timezone != old.Daemon.Timezone // modules schedule in it: all restart
	runs := d.runs()
	byName := map[string]*moduleRun{}
	for _, r := range runs {
		byName[r.name] = r
	}
	changed := func(name string) bool {
		o, _ := old.Module(name)
		n, _ := p.cfg.Module(name)
		return retimed || !o.Equal(n)
	}

	// Each module call has shutdown_timeout, cut to what is left of the
	// reload's budget, as Stop shares its own.
	deadline, _ := ctx.Deadline() // set by Reload
	within := func(timeout time.Duration) time.Duration {
		return min(timeout, max(deadline.Sub(d.clock.Now()), 0))
	}

	// Stop: modules gone, and changed ones that run.
	blocked := map[string]bool{} // may still run: keep the old run, start no other
	for _, r := range slices.Backward(runs) {
		_, kept := p.instances[r.name]
		if kept && !changed(r.name) {
			continue
		}
		if d.mayRun(r) {
			// The change asked for cannot happen: the reload is partial.
			blocked[r.name] = true
			result.Failed = append(result.Failed, r.name)
			then := "start it again"
			if !kept {
				then = "stop it"
			}
			result.Warnings = append(result.Warnings, fmt.Sprintf("module %s did not stop earlier (%s) and may still run: restart sentineld to %s",
				r.name, d.moduleState(r), then))
			continue
		}
		if d.moduleState(r) != ModuleRunning {
			continue // configured, failed at start, stopped: nothing runs
		}
		d.afterStop(r, d.call(ctx, r.name, "stop", within(old.Daemon.ShutdownTimeout.Std()), r.mod.Stop))
		if d.mayRun(r) {
			blocked[r.name] = true
			result.Failed = append(result.Failed, r.name)
			result.Warnings = append(result.Warnings, fmt.Sprintf("module %s did not stop (%s): restart sentineld to start it again",
				r.name, d.moduleState(r)))
		} else if !kept {
			result.Stopped = append(result.Stopped, r.name)
		}
	}

	// The new list, in the new order; runs that could not stop stay, so
	// that Stop and Snapshot still see them.
	var next []*moduleRun
	var starts []*moduleRun
	for _, name := range p.order {
		r, had := byName[name]
		switch {
		case had && blocked[name]:
			next = append(next, r)
		case had && !changed(name):
			next = append(next, r)
		default:
			n := &moduleRun{name: name, mod: p.instances[name].Module, state: ModuleConfigured}
			next = append(next, n)
			starts = append(starts, n)
			if had {
				result.Restarted = append(result.Restarted, name)
			} else {
				result.Started = append(result.Started, name)
			}
		}
	}
	for _, r := range runs {
		if _, kept := p.instances[r.name]; !kept && blocked[r.name] {
			next = append(next, r)
		}
	}

	// Publish before starting: the new modules get the new configuration
	// (timeouts, routes) and the status shows them as they start.
	d.server.SetPolicy(p.policy)
	if p.level != "" {
		level, _ := logging.ParseLevel(p.level) // checked by prepare
		d.opts.LogLevel.Set(level)
	}
	d.mu.Lock()
	d.mods = next
	d.mu.Unlock()
	d.viewMu.Lock()
	d.view = view{cfg: p.cfg, configJSON: p.configJSON, routers: routersOf(next), lastReload: d.view.lastReload}
	d.viewMu.Unlock()
	if p.dispatcher != nil && !d.replaceDispatcher(ctx, p.dispatcher, within(p.cfg.Daemon.ShutdownTimeout.Std()/4)) {
		result.Warnings = append(result.Warnings, "notification channels not replaced: sentineld is stopping")
	}

	for _, r := range starts {
		if ctx.Err() != nil {
			result.Failed = append(result.Failed, r.name)
			result.Warnings = append(result.Warnings, "reload interrupted: module "+r.name+" not started")
			continue
		}
		d.start(ctx, r, func() time.Duration { return within(p.cfg.Daemon.ShutdownTimeout.Std()) })
		if d.moduleState(r) != ModuleRunning {
			result.Failed = append(result.Failed, r.name)
		}
	}
}

// replaceDispatcher hands next to the forwarder, which cancels the old
// dispatcher (its queue is abandoned and counted, D-077: nothing is sent
// to a channel just removed or changed), then runs next and feeds it from
// then on. The old one is given wait to return. When the
// forwarder has ended (a Stop that did not wait for this reload), next
// never runs and replaceDispatcher reports false.
func (d *Daemon) replaceDispatcher(ctx context.Context, next *notify.Dispatcher, wait time.Duration) bool {
	gen := newGeneration(d.delivery, next)
	reply := make(chan *generation, 1)
	select {
	case d.swaps <- swap{next: gen, prev: reply}:
	case <-d.forwarded:
		gen.cancel()
		return false
	}
	prev := <-reply // the forwarder answers at once
	d.mu.Lock()
	d.gen = gen
	d.mu.Unlock()
	if !d.waitFor(ctx, prev.done, wait) {
		d.log.Warn("the replaced notification dispatcher did not return in time; it is abandoned")
	}
	return true
}

// outcome records the result of a reload; a failure stops the audit log
// (audit.Log.Record), which the next privileged request finds.
func (d *Daemon) outcome(ctx context.Context, by audit.Actor, correlation string, result audit.Result, detail map[string]string) {
	done, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := (&auditRecorder{d: d}).Record(done, audit.Entry{Actor: by, Action: "core.config_reload", Result: result,
		CorrelationID: correlation, Detail: detail}); err != nil {
		d.log.Error("reload outcome not recorded in the audit log", "error", err)
	}
}

func (d *Daemon) setLastReload(result, digest string) {
	d.viewMu.Lock()
	defer d.viewMu.Unlock()
	d.view.lastReload = &api.ReloadInfo{At: d.clock.Now().UTC(), Result: result, Digest: digest}
}

// bounded cuts s to an audit detail value.
func bounded(s string) string {
	const limit = 512
	if len(s) <= limit {
		return s
	}
	return s[:limit-3] + "..."
}
