package supervisor

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/config"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/module"
)

// blockedReload is why a service waits after a reload: the old instance
// of its name did not stop, so the new one is not started beside it until
// sentineld restarts (D-088, as D-078 (3) for a module).
const blockedReload = "reload_stop_failed"

// Reconfigure implements module.Reconfigurer (D-088): it takes the
// configuration of next, a supervisor configured but never started, and
// keeps every service whose definition did not change, routes aside: its
// loop, its process, its incident and its budget go on. A changed service
// is stopped, then started again from its new definition, with its state
// when what runs is the same (D-082, D-087 (1)); a removed one is stopped
// and its state dropped, its group record kept until a cleanup confirms
// the group empty; a new one is started. A change of
// settings.max_concurrent_checks asks for a restart (module.ErrRestart):
// the checks in flight hold slots of the running limit.
//
// A service whose loop, check or process does not stop within ctx keeps
// running as it was, owned by the instance and stopped by Stop; its new
// definition waits, blocked, until sentineld restarts, and every
// Reconfigure until then returns an error naming it (the daemon offers the
// configuration again at the next reload, which keeps it blocked).
func (in *instance) Reconfigure(ctx context.Context, next module.Configured, rt module.Runtime) error {
	nx, ok := next.(*instance)
	if !ok {
		return errors.New("supervisor: reconfigure from another module's instance")
	}
	if !in.startedOK || nx.settings.MaxConcurrentChecks != in.settings.MaxConcurrentChecks {
		return module.ErrRestart
	}
	// An operator's action ends and starts loops too: wait for it, or
	// change nothing (the daemon offers the configuration again at the next
	// reload, D-088 (1)).
	select {
	case in.ops <- struct{}{}:
		defer func() { <-in.ops }()
	case <-ctx.Done():
		return errors.New("supervisor: an operator's action is still in progress; the configuration was not applied")
	}

	// Classify, then stop what goes before anything starts.
	in.mu.Lock()
	var keep, start, retire []*service
	for _, s := range nx.services {
		cur := in.byName[s.def.Name]
		switch {
		case in.retired[s.def.Name] != nil:
			start = append(start, s) // stays blocked: see below
		case cur != nil && sameRun(cur, s, in.processes, nx.processes):
			keep = append(keep, cur)
		default:
			start = append(start, s)
			if cur != nil {
				retire = append(retire, cur)
			}
		}
	}
	for _, s := range in.services {
		if _, ok := nx.byName[s.def.Name]; !ok && in.retired[s.def.Name] == nil {
			retire = append(retire, s)
		}
	}
	in.mu.Unlock()
	stuck := in.retire(ctx, retire)

	in.mu.Lock()
	for _, s := range stuck {
		in.retired[s.def.Name] = s
	}
	// The replacements start from the state of the services they replace
	// when what runs is the same.
	old := &fileState{Services: map[string]serviceState{}}
	for _, s := range in.services {
		old.Services[s.def.Name] = s.st
	}
	reconcile(old, start)
	for _, s := range start {
		// What the loop of a systemd service saw of its unit carries over
		// to the same definition: its loop ended, so the memo is still
		// (review 3c-1 C7).
		if cur := in.byName[s.def.Name]; cur != nil && cur.def.Type == typeSystemd && cur.digest == s.digest &&
			in.retired[s.def.Name] == nil {
			s.unit = cur.unit
		}
	}
	var launch []*service
	var gates []gate
	for _, s := range start {
		s.prev = s.st.Health
		if in.retired[s.def.Name] != nil {
			s.phase, s.blocked = phaseBlocked, blockedReload
			continue
		}
		launch = append(launch, s)
		gates = append(gates, in.gateOf(s, nx.processes))
	}
	services := slices.Concat(keep, start)
	slices.SortFunc(services, func(a, b *service) int { return strings.Compare(a.def.Name, b.def.Name) })
	in.services = services
	in.byName = make(map[string]*service, len(services))
	for _, s := range services {
		in.byName[s.def.Name] = s
	}
	routes := map[string]config.Route{}
	for _, s := range nx.services { // a kept service has its old definition but takes its new route
		routes[s.def.Name] = s.def.Route
	}
	in.routes.Store(&routes)
	in.redact = processRedactor(rt.Secrets, services)
	in.processes = nx.processes
	in.settings = nx.settings
	stale := in.staleLocked()
	blocked := slices.Sorted(maps.Keys(in.retired))
	in.gen++
	gen := in.gen
	in.mu.Unlock()
	in.save(gen)

	if len(stale) > 0 {
		in.startCleanup(in.life, stale)
	}
	for i, s := range launch {
		in.startLoop(in.life, s, gates[i], cause{})
	}
	if len(stuck) > 0 {
		in.log.Error("supervisor services did not stop for a reload; their new definition waits until sentineld restarts",
			"services", blocked)
	}
	// Every blocked name, this reload's and earlier ones': the configuration
	// is not applied in full while one waits (review 3b-4 C3).
	if len(blocked) > 0 {
		return fmt.Errorf("supervisor: services not replaced, their running instance did not stop: %s", strings.Join(blocked, ", "))
	}
	return nil
}

// sameRun reports whether next runs as cur does: the same definition,
// routes aside, and for a process service the same central switch.
func sameRun(cur, next *service, armed, nextArmed bool) bool {
	a, b := cur.def, next.def
	a.Route, b.Route = config.Route{}, config.Route{}
	if a.Type == typeProcess && armed != nextArmed {
		return false
	}
	return reflect.DeepEqual(a, b)
}

// retire ends the services in parallel: each loop, its checks and its
// process, within ctx. It returns those that did not end; they keep their
// process and are stopped by Stop.
func (in *instance) retire(ctx context.Context, services []*service) (stuck []*service) {
	results := make(chan *service, len(services))
	for _, s := range services {
		go func() {
			if in.retireOne(ctx, s) {
				results <- nil
			} else {
				results <- s
			}
		}()
	}
	for range services {
		if s := <-results; s != nil {
			stuck = append(stuck, s)
		}
	}
	return stuck
}

// retireOne ends s and reports whether nothing of it runs any more: its
// loop (a launch it completes from now on is stopped by the loop itself),
// its checks and its process.
func (in *instance) retireOne(ctx context.Context, s *service) bool {
	if !in.endLoop(ctx, s) {
		return false
	}
	if !waitGroup(ctx, &s.checks) { // a check that ignores its cancellation keeps its name (C1)
		return false
	}
	in.mu.Lock()
	p, sinks := s.proc, s.sinks
	if p != nil {
		s.phase = phaseStopping
	}
	in.mu.Unlock()
	return p == nil || in.stopOne(ctx, s, p, sinks)
}

// staleLocked returns the names whose group records belong to no
// configured process service and to no retired one (their record is the
// running process's), and are not being cleaned up already. The caller
// holds in.mu.
func (in *instance) staleLocked() []string {
	var names []string
	for name := range in.groups {
		s, ok := in.byName[name]
		if ok && s.def.Type == typeProcess || in.retired[name] != nil || in.cleaning[name] != nil {
			continue
		}
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}
