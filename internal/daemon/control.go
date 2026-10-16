package daemon

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"maps"
	"math"
	"slices"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/authz"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/module"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/transport"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/api"
)

// listen resolves the access groups and acquires the control socket. It
// runs before any module starts: a second daemon, a missing group or an
// unsafe socket directory stops sentineld before it does any work.
func (d *Daemon) listen() (*transport.Server, error) {
	lookup := d.opts.LookupGroup
	cfg := d.config()
	policy, err := authz.NewPolicy(cfg.Daemon.Access, lookup)
	if err != nil {
		return nil, err
	}
	gid := -1
	if name := cfg.Daemon.SocketGroup; name != "" {
		g, err := lookup(name)
		if err != nil {
			return nil, fmt.Errorf("daemon.socket_group: group %q: %w", name, err)
		}
		// chown takes an int, 32 bits on some platforms, and reads
		// 4294967295 as "no change": refuse what does not fit.
		if g > math.MaxInt32 {
			return nil, fmt.Errorf("daemon.socket_group: group %q has gid %d, out of range", name, g)
		}
		gid = int(g)
	}
	return transport.Listen(transport.Options{
		Path: cfg.Daemon.Socket, Mode: cfg.Daemon.SocketMode.Perm(), GID: gid,
		Policy: policy, Peer: d.opts.PeerCredentials, Logger: d.log, Clock: d.clock,
		Recorder: d.recorder,
		Bindings: append([]transport.Binding{
			{Command: api.CoreStatus, Run: noArgs(func() (any, *api.Error) { return d.status(), nil })},
			{Command: api.CoreModules, Run: noArgs(func() (any, *api.Error) { return d.modules(), nil })},
			{Command: api.CoreConfigShow, Run: noArgs(func() (any, *api.Error) { return jsontext.Value(d.current().configJSON), nil })},
			{Command: api.CoreEvents, Run: noArgs(func() (any, *api.Error) {
				return nil, &api.Error{Code: api.CodeUnsupported,
					Message: "recent events are kept from Phase 3a, when the first module emits them; see the journal"}
			})},
			{Command: api.CoreAudit, Run: d.auditTail},
			{Command: api.CoreReload, Run: d.reloadCommand, Timeout: reloadExchange},
		}, d.moduleBindings()...),
	})
}

// moduleBindings binds the commands of every module the binary knows, in
// name order. Which instance runs a command is decided per request.
func (d *Daemon) moduleBindings() []transport.Binding {
	var out []transport.Binding
	for _, name := range slices.Sorted(maps.Keys(d.opts.Commands)) {
		for _, c := range d.opts.Commands[name] {
			out = append(out, transport.Binding{Command: c, Run: d.moduleCommand(name, c.Name)})
		}
	}
	return out
}

// moduleCommand runs command on the running instance of module name. A
// module that is not enabled, failed at start, is stopping or stopped
// answers unavailable.
func (d *Daemon) moduleCommand(name, command string) transport.Run {
	return func(ctx context.Context, _ authz.Peer, args jsontext.Value) (any, *api.Error) {
		m, h := d.acquire(name)
		if h == nil {
			return nil, &api.Error{Code: api.CodeUnavailable, Message: fmt.Sprintf("module %s is not running", name)}
		}
		defer m.calls.Done()
		return d.handle(ctx, m, h, command, args)
	}
}

// acquire returns the running instance of module name and its Handler,
// with one more command in flight; nil when none runs or it is stopping.
func (d *Daemon) acquire(name string) (*moduleRun, module.Handler) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, m := range d.mods {
		if m.name != name || m.state != ModuleRunning || m.closing {
			continue
		}
		h, ok := m.mod.(module.Handler) // module.Registry.Configure checked it
		if !ok {
			return nil, nil
		}
		m.calls.Add(1)
		return m, h
	}
	return nil, nil
}

// handle runs one command of m and encodes its result. It is a module
// boundary: the result's own MarshalJSON is module code too, so it is
// encoded here, while the call is still counted. A panic is reported as for
// Start and Stop, and the client gets a fixed message.
func (d *Daemon) handle(ctx context.Context, m *moduleRun, h module.Handler, command string,
	args jsontext.Value) (result any, apiErr *api.Error) {
	failed := &api.Error{Code: api.CodeInternal, Message: fmt.Sprintf("module %s: command %s failed", m.name, command)}
	defer func() {
		if recover() != nil {
			d.report(m.name, "command "+command, 0, &panicError{frames: panicFrames()})
			result, apiErr = nil, failed
		}
	}()
	result, apiErr = h.Handle(ctx, command, args)
	if apiErr != nil {
		return nil, apiErr
	}
	encoded, err := api.Result(result)
	if err != nil {
		d.log.Error("module command result could not be encoded", "module", m.name, "command", command,
			"error", d.config().RedactText(err.Error()))
		return nil, failed
	}
	return encoded.Result, nil
}

// noArgs adapts a command without arguments, refusing any.
func noArgs(f func() (any, *api.Error)) transport.Run {
	return func(_ context.Context, _ authz.Peer, args jsontext.Value) (any, *api.Error) {
		empty := args.Clone()
		if len(empty) > 0 && (empty.Compact() != nil || string(empty) != "{}") {
			return nil, &api.Error{Code: api.CodeBadRequest, Message: "this command takes no arguments"}
		}
		return f()
	}
}

// status is the result of core.status.
func (d *Daemon) status() api.Status {
	return d.statusOf(d.Snapshot())
}

func (d *Daemon) statusOf(snap Snapshot) api.Status {
	v := d.current()
	s := api.Status{
		Version:       d.opts.Version,
		StartedAt:     d.startedAt.UTC(),
		UptimeSeconds: int64(d.clock.Now().Sub(d.startedAt).Seconds()),
		Modules:       d.moduleInfos(snap),
		NotifyDropped: snap.NotifyDropped,
		LogDropped:    snap.LogDropped,
		IgnoredDirs:   slices.Clone(v.cfg.IgnoredDirs),
		Audit:         d.auditStatus(),
		ConfigDigest:  v.cfg.Digest,
		LastReload:    v.lastReload,

		NotifyAbandoned: snap.NotifyAbandoned,
	}
	for _, name := range slices.Sorted(maps.Keys(snap.Channels)) {
		c := snap.Channels[name]
		s.Channels = append(s.Channels, api.ChannelStats{Name: name, Delivered: c.Delivered, Failed: c.Failed,
			Dropped: c.Dropped, Suppressed: c.Suppressed, SuppressedLost: c.SuppressedLost})
	}
	return s
}

// modules is the result of core.modules.
func (d *Daemon) modules() []api.ModuleInfo {
	return d.moduleInfos(d.Snapshot())
}

// moduleInfos lists every module this binary knows, sorted by name, with
// its switch and, when configured, its lifecycle state.
func (d *Daemon) moduleInfos(snap Snapshot) []api.ModuleInfo {
	cfg := d.config()
	names := slices.Collect(maps.Keys(d.opts.Modules))
	for name := range snap.Modules {
		if _, known := d.opts.Modules[name]; !known {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	infos := make([]api.ModuleInfo, 0, len(names))
	for _, name := range names {
		info := api.ModuleInfo{Name: name, State: string(snap.Modules[name])}
		if a, ok := d.opts.Modules[name]; ok {
			info.Availability = a.String()
		}
		if mc, ok := cfg.Module(name); ok {
			info.Enabled = mc.Enabled
		}
		infos = append(infos, info)
	}
	return infos
}
