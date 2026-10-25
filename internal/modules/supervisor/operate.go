package supervisor

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/audit"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/module"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/api"
)

// action is an operator's command on one service (D-089).
type action string

const (
	actStart   action = "start"
	actStop    action = "stop"
	actRestart action = "restart"
	actEnable  action = "enable"
	actDisable action = "disable"
	actReset   action = "reset"
)

// actions maps the operator's commands to their action.
var actions = map[string]action{
	api.SupervisorStart.Name:   actStart,
	api.SupervisorStop.Name:    actStop,
	api.SupervisorRestart.Name: actRestart,
	api.SupervisorEnable.Name:  actEnable,
	api.SupervisorDisable.Name: actDisable,
	api.SupervisorReset.Name:   actReset,
}

const (
	// actionWait bounds how long a command waits for its action; past it
	// the reply says the action is still in progress (review 3b-5 C6).
	actionWait = 2 * time.Second
	// replyMargin is kept before the exchange's deadline, for the
	// transport to record the outcome and write the reply.
	replyMargin = 2 * time.Second
	// actionSlack bounds an action beyond its service's own stop or check
	// timeout: the end of its loop (an audit record, a cleanup) and the
	// first decision of the next one.
	actionSlack = 30 * time.Second
)

// actionResult is how an action ended.
type actionResult struct {
	changed  bool
	notSaved bool
	err      *api.Error
}

// act runs action a on the service name for an operator. It refuses at
// once what the service's state does not allow; otherwise it carries the
// action out in a goroutine of the instance, under the module's lifetime
// and the service's own bound, never the exchange's (a stop honours
// stop_timeout), and waits for it until the reply is due. One action runs
// at a time, never beside a reload (ops): another is refused, not queued.
func (in *instance) act(ctx context.Context, name string, a action) (any, *api.Error) {
	select {
	case in.ops <- struct{}{}:
	default:
		return nil, &api.Error{Code: api.CodeUnavailable, Message: "another supervisor action or a reload is in progress; try again"}
	}
	in.mu.Lock()
	s := in.byName[name]
	refused := &api.Error{Code: api.CodeBadRequest, Message: fmt.Sprintf("no service named %q", name)}
	if s != nil {
		refused = in.refusal(s, a, in.rt.Clock.Now())
	}
	in.mu.Unlock()
	if refused != nil {
		<-in.ops
		return nil, refused
	}
	request := audit.CorrelationFrom(ctx) // links the records of the action to the request (C7)
	done := make(chan actionResult, 1)
	actx, cancel := context.WithTimeout(in.life, in.bound(s))
	in.workers.Add(1)
	go func() {
		defer in.workers.Done()
		defer func() { <-in.ops }()
		defer cancel()
		defer func() {
			if recover() != nil {
				in.log.Error("supervisor operator action panicked", "service", name, "action", a)
				done <- actionResult{err: &api.Error{Code: api.CodeInternal, Message: "the action failed"}}
			}
		}()
		done <- in.carry(actx, s, a, request)
	}()
	// The exchange's deadline is on the wall clock, so the wait is too.
	timer := time.NewTimer(waitFor(ctx))
	defer timer.Stop()
	out := api.SupervisorActionResult{Name: name, Action: string(a)}
	select {
	case r := <-done:
		if r.err != nil {
			return nil, r.err
		}
		out.Changed, out.NotSaved, out.Done = r.changed, r.notSaved, true
	case <-timer.C:
	}
	in.mu.Lock()
	if cur := in.byName[name]; cur != nil {
		out.Phase = cur.phase
	}
	in.mu.Unlock()
	return out, nil
}

// waitFor is how long a command may wait for its action: actionWait, and
// never past the exchange's deadline less replyMargin.
func waitFor(ctx context.Context) time.Duration {
	d := actionWait
	if deadline, ok := ctx.Deadline(); ok {
		d = min(d, time.Until(deadline)-replyMargin)
	}
	return max(d, 0)
}

// bound bounds an action on s: its stop or check timeout and the slack.
func (in *instance) bound(s *service) time.Duration {
	if s.def.Type == typeProcess {
		return s.def.Process.StopTimeout + actionSlack
	}
	return s.def.Timeout + actionSlack
}

// refusal says why a is refused on s now, in this order: a blocked name,
// the service's type, its configuration, the operator's flags, the
// central switch, the recovery budget; nil when it may go on. The caller
// holds in.mu.
func (in *instance) refusal(s *service, a action, now time.Time) *api.Error {
	name := s.def.Name
	conflict := func(format string, args ...any) *api.Error {
		return &api.Error{Code: api.CodeConflict, Message: fmt.Sprintf(format, args...)}
	}
	if in.retired[name] != nil {
		return conflict("service %s waits until sentineld restarts: its running instance did not stop", name)
	}
	if s.def.Type != typeProcess && a != actEnable && a != actDisable {
		return conflict("supervisor %s applies to process services; supervisor disable pauses the checks of %s", a, name)
	}
	if s.def.Disabled && (a == actStart || a == actRestart || a == actEnable) {
		return conflict("service %s is disabled by its configuration (enabled: false), which an operator cannot override", name)
	}
	if a != actStart && a != actRestart {
		return nil
	}
	switch {
	case s.st.Disabled:
		return conflict("service %s is disabled: supervisor enable %s first", name, name)
	case !in.processes:
		return conflict("process services are not armed: modules.supervisor.processes is false")
	case a == actStart && s.proc != nil:
		return nil // already running: nothing to do
	case !admits(s, now):
		return conflict("the recovery budget of %s is spent: supervisor reset %s renews it", name, name)
	}
	return nil
}

// admits reports whether a launch of s now is allowed by its recovery
// budget: always without an open incident (D-087 (2)). The caller holds
// in.mu.
func admits(s *service, now time.Time) bool {
	if s.st.Health != HealthFailed {
		return true
	}
	_, adm := admit(s.st.budget(), now, s.def.Process.Recovery)
	return adm.OK
}

// carry carries action a out on s, holding ops; request is the
// correlation id of the operator's request. Every action first ends the
// service's loop and waits for it: with the loop joined nothing else
// changes the service's budget, flags or process, so what is decided next
// holds (review 3b-5 C3). It then starts a new loop, which adopts a
// process still running.
func (in *instance) carry(ctx context.Context, s *service, a action, request string) actionResult {
	switch a {
	case actStop:
		return in.stop(ctx, s, request)
	case actDisable:
		return in.disable(ctx, s, request)
	case actStart, actRestart:
		return in.start(ctx, s, a == actRestart, request)
	case actEnable:
		return in.enable(ctx, s, request)
	case actReset:
		return in.reset(ctx, s, request)
	}
	return actionResult{err: &api.Error{Code: api.CodeInternal, Message: "unknown action"}}
}

// stop holds s's process stopped (persisted): the flag is applied first,
// then saved (a change that takes away, review 3b-5 C4).
func (in *instance) stop(ctx context.Context, s *service, request string) actionResult {
	in.mu.Lock()
	if s.st.Stopped && s.proc == nil {
		in.mu.Unlock()
		return in.held()
	}
	s.st.Stopped = true
	in.gen++
	gen := in.gen
	in.mu.Unlock()
	if !in.retireOne(ctx, s) {
		return in.stuck(s, actStop, request)
	}
	return in.restricted(ctx, s, actStop, gen, request)
}

// disable takes s out of supervision (persisted, D-020): its process is
// stopped, its checks end.
func (in *instance) disable(ctx context.Context, s *service, request string) actionResult {
	in.mu.Lock()
	if s.st.Disabled {
		in.mu.Unlock()
		return in.held()
	}
	s.st.Disabled = true
	in.gen++
	gen := in.gen
	in.mu.Unlock()
	if !in.retireOne(ctx, s) {
		return in.stuck(s, actDisable, request)
	}
	return in.restricted(ctx, s, actDisable, gen, request)
}

// held answers a stop or a disable that finds the service already held:
// nothing changes, but a hold whose save failed is saved now (review
// 3b-5 R1), so repeating the command is how an operator makes it last.
func (in *instance) held() actionResult {
	in.mu.Lock()
	gen := in.gen
	in.mu.Unlock()
	return actionResult{notSaved: in.write(gen) != nil}
}

// restricted ends a stop or a disable whose service has ended: it saves
// the flag, starts the service's gated loop and waits for it (it cleans up,
// shows the gate and returns), and records the action.
func (in *instance) restricted(ctx context.Context, s *service, a action, gen uint64, request string) actionResult {
	notSaved := in.write(gen) != nil
	if notSaved {
		in.log.Error("supervisor applied an operator's action but could not save it; a restart of sentineld would not remember it",
			"service", s.def.Name, "action", a)
	}
	in.wait(ctx, in.restartLoop(s, cause{}))
	in.auditAction(s, a, request, audit.ResultOK)
	return actionResult{changed: true, notSaved: notSaved}
}

// start launches s for an operator (restart: stops its process first). A
// held stop is cleared, saved before anything runs. With an open incident
// the launch is an attempt (D-087 (2)); the budget is checked again once
// the loop is joined, and a running process is never stopped when the
// launch would be refused: the new loop adopts it instead.
func (in *instance) start(ctx context.Context, s *service, restart bool, request string) actionResult {
	in.mu.Lock()
	running, stopped := s.proc != nil, s.st.Stopped
	in.mu.Unlock()
	if running && !restart {
		return actionResult{}
	}
	if stopped {
		if err := in.commit(s, func(st *serviceState) { st.Stopped = false }); err != nil {
			return in.notCommitted(s, err)
		}
	}
	if !in.endLoop(ctx, s) {
		return in.stuck(s, actStop, request)
	}
	in.mu.Lock()
	p, sinks := s.proc, s.sinks
	ok := admits(s, in.rt.Clock.Now())
	in.mu.Unlock()
	if !ok {
		in.restartLoop(s, cause{})
		return actionResult{changed: stopped, err: &api.Error{Code: api.CodeConflict,
			Message: fmt.Sprintf("the recovery budget of %s is spent: supervisor reset %s renews it", s.def.Name, s.def.Name)}}
	}
	if p != nil && restart {
		in.setPhase(s, phaseStopping)
		if !in.stopOne(ctx, s, p, sinks) {
			return in.stuck(s, actStop, request)
		}
		in.auditAction(s, actStop, request, audit.ResultOK)
	}
	in.wait(ctx, in.restartLoop(s, cause{operator: true, request: request}))
	return actionResult{changed: true}
}

// enable puts s back under supervision; it runs unless it is held
// stopped or not armed.
func (in *instance) enable(ctx context.Context, s *service, request string) actionResult {
	in.mu.Lock()
	disabled := s.st.Disabled
	in.mu.Unlock()
	if !disabled {
		return actionResult{}
	}
	if err := in.commit(s, func(st *serviceState) { st.Disabled = false }); err != nil {
		return in.notCommitted(s, err)
	}
	if !in.endLoop(ctx, s) { // the gated loop has returned already
		return in.stuck(s, actEnable, request)
	}
	in.wait(ctx, in.restartLoop(s, cause{operator: true, request: request}))
	in.auditAction(s, actEnable, request, audit.ResultOK)
	return actionResult{changed: true}
}

// reset clears s's recovery budget (D-013), saved before it applies; the
// incident stays open until stable_after. An exhausted or waiting service
// is launched at once, as an operator's attempt; a running process is
// left alone.
func (in *instance) reset(ctx context.Context, s *service, request string) actionResult {
	if !in.endLoop(ctx, s) {
		return in.stuck(s, actReset, request)
	}
	in.mu.Lock()
	b := s.st.budget()
	in.mu.Unlock()
	changed := len(b.Attempts) > 0 || !b.ExhaustedAt.IsZero()
	if changed {
		if err := in.commit(s, func(st *serviceState) { st.Attempts, st.ExhaustedAt = nil, time.Time{} }); err != nil {
			in.restartLoop(s, cause{})
			return in.notCommitted(s, err)
		}
	}
	in.wait(ctx, in.restartLoop(s, cause{operator: true, request: request}))
	in.auditAction(s, actReset, request, audit.ResultOK)
	return actionResult{changed: changed}
}

// restartLoop starts a new loop for s, whose previous loop has ended, with
// the gate of now, and returns what its first decision closes.
func (in *instance) restartLoop(s *service, c cause) <-chan struct{} {
	in.mu.Lock()
	g := in.gateOf(s, in.processes)
	in.mu.Unlock()
	return in.startLoop(in.life, s, g, c)
}

// wait waits for decided until ctx ends.
func (in *instance) wait(ctx context.Context, decided <-chan struct{}) {
	select {
	case <-decided:
	case <-ctx.Done():
	}
}

// notCommitted reports a change that lets a service run and could not be
// saved: nothing changed in memory, and on disk unless even restoring the
// file failed (errUncertain).
func (in *instance) notCommitted(s *service, err error) actionResult {
	if errors.Is(err, errUncertain) {
		in.log.Error("supervisor cannot save an operator's action; it does not apply now, but the state file may hold it",
			"service", s.def.Name, "error", err)
		return actionResult{err: &api.Error{Code: api.CodeUnavailable,
			Message: "the state file cannot be written; the change does not apply, but the file may hold it: a restart of sentineld could apply it"}}
	}
	in.log.Error("supervisor cannot save an operator's action; nothing was changed", "service", s.def.Name, "error", err)
	return actionResult{err: &api.Error{Code: api.CodeUnavailable, Message: "the state file cannot be written; nothing was changed"}}
}

// stuck reports a loop or a process of s that did not end within the
// action's bound. While the module stops, that is Stop's to handle;
// otherwise s is blocked as a reload blocks a service that does not stop
// (D-088 (3)): it keeps running as it was, owned and stopped by Stop, and
// its name takes no new launch until sentineld restarts.
func (in *instance) stuck(s *service, a action, request string) actionResult {
	if in.life.Err() != nil {
		return actionResult{err: &api.Error{Code: api.CodeUnavailable, Message: "the supervisor is stopping"}}
	}
	in.mu.Lock()
	in.retired[s.def.Name] = s
	b := &service{def: s.def, checker: s.checker, digest: s.digest, st: s.st, prev: s.prev,
		phase: phaseBlocked, blocked: blockedOperator}
	b.st.Attempts = slices.Clone(s.st.Attempts)
	for i, cur := range in.services {
		if cur == s {
			in.services[i] = b
		}
	}
	in.byName[s.def.Name] = b
	in.gen++
	gen := in.gen
	in.mu.Unlock()
	in.save(gen)
	in.log.Error("supervisor service did not stop for an operator's action; it waits until sentineld restarts",
		"service", s.def.Name, "action", a)
	in.auditAction(s, a, request, audit.ResultFailed)
	return actionResult{changed: true, err: &api.Error{Code: api.CodeUnavailable,
		Message: fmt.Sprintf("service %s did not stop; it is blocked until sentineld restarts (see supervisor status %s)", s.def.Name, s.def.Name)}}
}

// auditAction records the outcome of an operator's action the module
// carried out, linked to the request; a launch has its own records. Best
// effort: the request's intent is in the log already, and a stop is never
// held back by the audit log.
func (in *instance) auditAction(s *service, a action, request string, result audit.Result) {
	ctx, cancel := context.WithTimeout(context.Background(), auditTimeout)
	defer cancel()
	err := in.rt.Audit.Record(ctx, module.AuditEntry{Action: moduleName + "." + string(a), Target: s.def.Name,
		Result: result, CorrelationID: request, Detail: map[string]string{"trigger": triggerOperator}})
	if err != nil {
		in.log.Error("supervisor could not record an operator's action in the audit log", "service", s.def.Name, "action", a, "error", err)
	}
}
