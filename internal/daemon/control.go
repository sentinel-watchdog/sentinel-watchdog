package daemon

import (
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
	"maps"
	"math"
	"slices"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/authz"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/transport"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/api"
)

// listen resolves the access groups and acquires the control socket. It
// runs before any module starts: a second daemon, a missing group or an
// unsafe socket directory stops sentineld before it does any work.
func (d *Daemon) listen() (*transport.Server, error) {
	lookup := d.opts.LookupGroup
	if lookup == nil {
		lookup = authz.LookupGroupSystem
	}
	policy, err := authz.NewPolicy(d.cfg.Daemon.Access, lookup)
	if err != nil {
		return nil, err
	}
	gid := -1
	if name := d.cfg.Daemon.SocketGroup; name != "" {
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
	configJSON, err := json.Marshal(d.cfg.Redacted())
	if err != nil {
		return nil, fmt.Errorf("daemon: encode the redacted configuration: %w", err)
	}
	return transport.Listen(transport.Options{
		Path: d.cfg.Daemon.Socket, Mode: d.cfg.Daemon.SocketMode.Perm(), GID: gid,
		Policy: policy, Peer: d.opts.PeerCredentials, Logger: d.log,
		Bindings: []transport.Binding{
			{Command: api.CoreStatus, Run: noArgs(func() (any, *api.Error) { return d.status(), nil })},
			{Command: api.CoreModules, Run: noArgs(func() (any, *api.Error) { return d.modules(), nil })},
			{Command: api.CoreConfigShow, Run: noArgs(func() (any, *api.Error) { return jsontext.Value(configJSON), nil })},
			{Command: api.CoreEvents, Run: noArgs(func() (any, *api.Error) {
				return nil, &api.Error{Code: api.CodeUnsupported,
					Message: "recent events are kept from Phase 3a, when the first module emits them; see the journal"}
			})},
		},
	})
}

// noArgs adapts a command without arguments, refusing any.
func noArgs(f func() (any, *api.Error)) transport.Run {
	return func(_ context.Context, args jsontext.Value) (any, *api.Error) {
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
	s := api.Status{
		Version:       d.opts.Version,
		StartedAt:     d.startedAt.UTC(),
		UptimeSeconds: int64(d.clock.Now().Sub(d.startedAt).Seconds()),
		Modules:       d.moduleInfos(snap),
		NotifyDropped: snap.NotifyDropped,
		LogDropped:    snap.LogDropped,
		IgnoredDirs:   slices.Clone(d.cfg.IgnoredDirs),
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
		if mc, ok := d.cfg.Module(name); ok {
			info.Enabled = mc.Enabled
		}
		infos = append(infos, info)
	}
	return infos
}
