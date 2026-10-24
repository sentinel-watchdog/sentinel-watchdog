package supervisor

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"maps"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/audit"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/module"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/redact"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/platform/executor"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/model"
)

// Phases of a process service, shown by status.
const (
	phaseNotArmed    = "not_armed"    // modules.supervisor.processes is false
	phaseStarting    = "starting"     // running, within startup_grace
	phaseRunning     = "running"      // running past startup_grace
	phaseExited      = "exited"       // ended by itself; recovery decides what follows
	phaseStartFailed = "start_failed" // could not be started
	phaseRestarting  = "restarting"   // waiting for the delay of the next attempt
	phaseExhausted   = "exhausted"    // the recovery budget is spent: waiting for the cooldown
	phaseBlocked     = "blocked"      // a launch waits for the audit log or the state file
	phaseStopping    = "stopping"     // being stopped by sentineld
	phaseStopped     = "stopped"      // stopped by sentineld
)

// Why a launch is blocked (phaseBlocked).
const (
	blockedAudit = "audit_unavailable" // the intent could not be recorded
	blockedState = "state_unavailable" // the attempt could not be saved
)

// Triggers of a launch, in its audit record and its events.
const (
	triggerStartup  = "startup"  // a service without an open incident: not an attempt
	triggerFailure  = "failure"  // an attempt of an open incident
	triggerCooldown = "cooldown" // the first attempt after the cooldown
)

// Audit actions of the supervisor's own actions (D-087).
const (
	actionStart   = "supervisor.start"
	actionRestart = "supervisor.restart"
	actionCleanup = "supervisor.cleanup"
)

const (
	// cleanupTimeout bounds the cleanup of one recorded group.
	cleanupTimeout = 5 * time.Second
	// auditTimeout bounds one audit record.
	auditTimeout = 5 * time.Second
	// failureCanceled is the audit failure of a launch that the module's
	// end interrupted.
	failureCanceled = "canceled"
)

// attempt is a launch the loop decided on.
type attempt struct {
	n           int    // the attempt's number in the window; 0: not an attempt
	trigger     string // triggerStartup, triggerFailure or triggerCooldown
	correlation string // the open incident, else a fresh id
}

// runProcess is the life of one process service (D-084, D-087): clean up
// what is left of its recorded group, launch it if the central switch
// allows, watch it, and after a failure start it again within its
// recovery budget. Cleanup comes first in every round and never waits for
// recovery or the audit log. A panic in the module's own code is logged
// without its value; a process already launched is still stopped by Stop.
func (in *instance) runProcess(ctx context.Context, s *service, armed bool) {
	defer func() {
		if recover() != nil {
			in.log.Error("supervisor process loop panicked", "service", s.def.Name)
		}
	}()
	if !in.waitCleanup(ctx, s.def.Name) {
		return
	}
	retry := s.def.Process.Recovery.MaxDelay
	cleanupFailing := false
	for ctx.Err() == nil {
		if !in.cleanupRecord(ctx, s.def.Name) {
			if ctx.Err() != nil {
				return
			}
			if !cleanupFailing { // one failure per streak, not one per retry
				in.processFailed(s, FailureCleanup, nil, false)
			}
			cleanupFailing = true
			in.sleep(ctx, retry)
			continue
		}
		cleanupFailing = false
		if !armed {
			in.setPhase(s, phaseNotArmed)
			return
		}
		a, ok := in.next(ctx, s)
		if ok {
			in.run1(ctx, s, a)
		}
	}
}

// next decides the next launch of s and waits for it: at once for a
// service without an open incident; otherwise within the recovery budget,
// after the attempt's delay, or not before the cooldown ends (then ok is
// false and the loop decides again).
func (in *instance) next(ctx context.Context, s *service) (attempt, bool) {
	r := s.def.Process.Recovery
	now := in.rt.Clock.Now()
	in.mu.Lock()
	if s.st.Health != HealthFailed {
		in.mu.Unlock()
		return attempt{trigger: triggerStartup, correlation: model.NewEventID()}, true
	}
	b, adm := admit(s.st.budget(), now, r)
	changed := !sameBudget(b, s.st.budget())
	s.st.setBudget(b)
	var ev *model.Event
	if !adm.OK {
		s.phase, s.resumeAt, s.nextAttemptAt, s.blocked = phaseExhausted, adm.ResumeAt, time.Time{}, ""
		if adm.Exhausted {
			e := exhaustedEvent(s, s.st.IncidentID, adm.ResumeAt)
			ev = &e
		}
	}
	a := attempt{n: adm.Attempt, trigger: triggerFailure, correlation: s.st.IncidentID}
	in.gen++
	gen := in.gen
	in.mu.Unlock()
	if changed {
		in.save(gen) // an exhaustion is also derived again from a full window
	}
	if !adm.OK {
		if ev != nil {
			in.log.Error("supervisor recovery exhausted; the process is not started again before the cooldown ends",
				"service", s.def.Name, "resume_at", adm.ResumeAt)
			in.publish(s, *ev)
		}
		in.sleep(ctx, adm.ResumeAt.Sub(now))
		return attempt{}, false
	}
	if adm.Renewed {
		a.trigger = triggerCooldown
		return a, true
	}
	d := delay(a.n, r)
	in.setPending(s, phaseRestarting, now.Add(d), "")
	return a, in.sleep(ctx, d)
}

// run1 launches s once for a and watches the process until it ends or the
// module stops: the intent is recorded in the audit log first, then an
// attempt is saved, then the process is launched and the outcome
// recorded (ADR-0011, D-014). A launch that cannot be recorded or saved
// waits for the next round; an intent refused uses no attempt.
func (in *instance) run1(ctx context.Context, s *service, a attempt) {
	retry := s.def.Process.Recovery.MaxDelay
	if err := in.audit(ctx, s, a, audit.ResultIntent, nil); err != nil {
		if ctx.Err() == nil {
			in.log.Error("supervisor cannot record the launch in the audit log; the process is not started",
				"service", s.def.Name, "error", err)
			in.setPending(s, phaseBlocked, in.rt.Clock.Now().Add(retry), blockedAudit)
			in.sleep(ctx, retry)
		}
		return
	}
	if a.n > 0 && !in.reserve(ctx, s, a) {
		in.sleep(ctx, retry)
		return
	}
	lp, sinks, late, code := in.launchProcess(ctx, s)
	if lp.proc == nil {
		failure := string(code)
		if failure == "" {
			failure = failureCanceled
		}
		in.outcome(ctx, s, a, audit.ResultFailed, map[string]string{"failure": failure})
		return
	}
	in.outcome(ctx, s, a, audit.ResultOK, map[string]string{"pid": strconv.Itoa(lp.pid)})
	if a.n > 0 {
		in.restarted(s, a)
	}
	if late {
		lateCtx, cancel := context.WithTimeout(context.Background(), s.def.Process.StopTimeout+cleanupTimeout)
		defer cancel()
		in.stopOne(lateCtx, s, lp.proc, sinks)
		return
	}
	in.watch(ctx, s, lp, sinks)
}

// reserve saves attempt a before its launch (D-014). On failure the
// attempt is taken back in memory, but a concurrent save may already have
// written it: it then counts, which is conservative (C2); so does one
// saved and never launched.
func (in *instance) reserve(ctx context.Context, s *service, a attempt) bool {
	now := in.rt.Clock.Now()
	in.mu.Lock()
	prev := s.st.budget()
	s.st.setBudget(reserve(prev, now))
	in.gen++
	gen := in.gen
	in.mu.Unlock()
	err := in.write(gen)
	if err == nil {
		return true
	}
	in.mu.Lock()
	s.st.setBudget(prev)
	in.mu.Unlock()
	in.log.Error("supervisor cannot save the recovery attempt; the process is not started", "service", s.def.Name, "error", err)
	in.outcome(ctx, s, a, audit.ResultFailed, map[string]string{"failure": blockedState})
	in.setPending(s, phaseBlocked, now.Add(s.def.Process.Recovery.MaxDelay), blockedState)
	return false
}

// restarted counts a launched attempt and reports it.
func (in *instance) restarted(s *service, a attempt) {
	in.mu.Lock()
	s.st.Restarts++
	e := restartedEvent(s, a)
	in.gen++
	gen := in.gen
	in.mu.Unlock()
	in.save(gen)
	in.log.Warn("supervisor restarted the process", "service", s.def.Name, "attempt", a.n, "trigger", a.trigger)
	in.publish(s, e)
}

// audit records a launch of s in the audit log, bounded by auditTimeout.
// It names the service, never the program, its arguments or its
// environment.
func (in *instance) audit(ctx context.Context, s *service, a attempt, result audit.Result, extra map[string]string) error {
	detail := map[string]string{"trigger": a.trigger}
	action := actionStart
	if a.n > 0 {
		action = actionRestart
		detail["attempt"] = strconv.Itoa(a.n)
		detail["max_attempts"] = strconv.Itoa(s.def.Process.Recovery.MaxAttempts)
	}
	maps.Copy(detail, extra)
	ctx, cancel := context.WithTimeout(ctx, auditTimeout)
	defer cancel()
	return in.rt.Audit.Record(ctx, module.AuditEntry{Action: action, Target: s.def.Name, Result: result,
		CorrelationID: a.correlation, Detail: detail})
}

// outcome records the result of a launch whose intent is in the log, also
// while the module stops: an intent must not stay unanswered (C5). When it
// cannot be recorded, the audit log is broken and the daemon has reported
// it; a launched process stays owned and watched, never launched again to
// repair the record.
func (in *instance) outcome(ctx context.Context, s *service, a attempt, result audit.Result, detail map[string]string) {
	if err := in.audit(context.WithoutCancel(ctx), s, a, result, detail); err != nil {
		in.log.Error("supervisor could not record the outcome of a launch in the audit log", "service", s.def.Name, "error", err)
	}
}

// sleep waits d on the module's clock; false when ctx ended first.
func (in *instance) sleep(ctx context.Context, d time.Duration) bool {
	t := in.rt.Clock.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C():
		return true
	case <-ctx.Done():
		return false
	}
}

// setPending shows that s waits in phase until until, for reason; an
// earlier cooldown is over (review 3b-3 R3).
func (in *instance) setPending(s *service, phase string, until time.Time, reason string) {
	in.mu.Lock()
	s.phase, s.nextAttemptAt, s.blocked, s.resumeAt = phase, until, reason, time.Time{}
	in.mu.Unlock()
}

func sameBudget(a, b budget) bool {
	return slices.EqualFunc(a.Attempts, b.Attempts, time.Time.Equal) && a.ExhaustedAt.Equal(b.ExhaustedAt)
}

// cleanupRecord kills what is left of the group recorded for name, if
// any, and forgets the record once nothing of it is alive. It reports
// whether the name is free to launch. The cleanup is recorded in the
// audit log when it can be, never waiting for it (D-084 (6)).
func (in *instance) cleanupRecord(ctx context.Context, name string) bool {
	in.mu.Lock()
	rec, ok := in.groups[name]
	in.mu.Unlock()
	if !ok {
		return true
	}
	cctx, cancel := context.WithTimeout(ctx, cleanupTimeout)
	defer cancel()
	err := in.launcher.Cleanup(cctx, rec.group())
	in.auditCleanup(ctx, name, rec.Pgid, err == nil)
	if err != nil {
		if ctx.Err() == nil {
			in.log.Error("supervisor could not clean up the process group a previous run left; the service is not started",
				"service", name, "pgid", rec.Pgid, "error", err)
		}
		return false
	}
	in.mu.Lock()
	if cur, ok := in.groups[name]; ok && cur == rec { // not a newer record of the name
		delete(in.groups, name)
	}
	in.gen++
	gen := in.gen
	in.mu.Unlock()
	in.save(gen)
	in.log.Info("supervisor cleaned up the process group a previous run left", "service", name, "pgid", rec.Pgid)
	return true
}

// waitCleanup waits for the cleanup that owns name's group record, if
// any (startCleanup); false when ctx ended first.
func (in *instance) waitCleanup(ctx context.Context, name string) bool {
	in.mu.Lock()
	owner := in.cleaning[name]
	in.mu.Unlock()
	if owner == nil {
		return true
	}
	select {
	case <-owner:
		return true
	case <-ctx.Done():
		return false
	}
}

// auditCleanup records a cleanup, best effort.
func (in *instance) auditCleanup(ctx context.Context, name string, pgid int, ok bool) {
	result := audit.ResultOK
	if !ok {
		result = audit.ResultFailed
	}
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditTimeout)
	defer cancel()
	err := in.rt.Audit.Record(actx, module.AuditEntry{Action: actionCleanup, Target: name, Result: result,
		Detail: map[string]string{"pgid": strconv.Itoa(pgid)}})
	if err != nil {
		in.log.Error("supervisor could not record a cleanup in the audit log; the cleanup is not held back",
			"service", name, "error", err)
	}
}

// cleanupStale cleans up the records of names that are no longer process
// services (removed, renamed, changed to http). A failure keeps the
// record for the next start.
func (in *instance) cleanupStale(ctx context.Context, names []string) {
	for _, name := range names {
		if ctx.Err() != nil {
			return
		}
		in.cleanupRecord(ctx, name)
	}
}

// launchProcess starts s's process and owns it before it looks at ctx
// again. lp.proc is nil when nothing runs: code is then the failure
// (already recorded), or "" when the module's end interrupted the launch.
// late reports a launch that completed once Stop had collected the
// processes (it may have given up waiting for this loop), or once a reload
// began to retire s: the caller stops it (review 3b-2 R1); one that
// completes before is stopped by Stop or by the reload.
func (in *instance) launchProcess(ctx context.Context, s *service) (lp launched, sinks []io.Closer, late bool, code Failure) {
	spec, sinks, code := in.prepare(s)
	if code != "" {
		in.processFailed(s, code, nil, true)
		return launched{}, nil, false, code
	}
	// A record slot is reserved before the launch, so that concurrent
	// launches never write more records than a state file may hold
	// (review 3b-2 R5).
	in.mu.Lock()
	full := len(in.groups)+in.reserved >= maxGroups
	if !full {
		in.reserved++
	}
	in.mu.Unlock()
	if full {
		closeAll(sinks)
		in.log.Error("supervisor keeps too many process group records; the service is not started", "service", s.def.Name)
		in.processFailed(s, FailureCleanup, nil, true)
		return launched{}, nil, false, FailureCleanup
	}
	lp, err := in.launcher.Launch(ctx, spec)
	if err != nil {
		in.mu.Lock()
		in.reserved--
		in.mu.Unlock()
		closeAll(sinks)
		if ctx.Err() != nil {
			return launched{}, nil, false, ""
		}
		code = launchFailure(err)
		in.log.Error("supervisor could not start the process", "service", s.def.Name, "error", in.redactor()(err.Error()))
		in.processFailed(s, code, nil, true)
		return launched{}, nil, false, code
	}
	now := in.rt.Clock.Now()
	in.mu.Lock()
	in.reserved--
	s.proc, s.pid, s.startedAt, s.phase, s.sinks, s.group = lp.proc, lp.pid, now, phaseStarting, sinks, lp.group
	s.nextAttemptAt, s.resumeAt, s.blocked = time.Time{}, time.Time{}, ""
	s.st.Starts++
	if !lp.group.IsZero() {
		in.groups[s.def.Name] = recordOf(lp.group)
	}
	late = in.collected || s.retiring
	if late {
		s.phase = phaseStopping
	}
	in.gen++
	gen := in.gen
	in.mu.Unlock()
	in.save(gen) // the record first: a crash from now on leaves it for the next run
	in.log.Info("supervisor started the process", "service", s.def.Name, "pid", lp.pid)
	return lp, sinks, late, ""
}

// release forgets the group record of s once nothing of the group is
// alive. The executor kills the group when its leader ends, but a member
// may survive that (stuck in the kernel), so the group is checked like a
// previous run's before the record goes (review 3b-2 R3); otherwise the
// record stays for the next start.
func (in *instance) release(ctx context.Context, s *service, g executor.Group) {
	if !g.IsZero() {
		cctx, cancel := context.WithTimeout(ctx, cleanupTimeout)
		defer cancel()
		if err := in.launcher.Cleanup(cctx, g); err != nil {
			in.log.Error("supervisor process ended but members of its group survive; the record is kept for the next start",
				"service", s.def.Name, "pgid", g.Pgid, "error", err)
			return
		}
	}
	in.mu.Lock()
	if rec, ok := in.groups[s.def.Name]; ok && rec == recordOf(g) {
		delete(in.groups, s.def.Name)
	}
	in.gen++
	gen := in.gen
	in.mu.Unlock()
	in.save(gen)
}

// prepare builds the Spec of s's process for this launch: the program
// resolved and checked again (it may have changed since Configure), and
// the output sinks opened. code is the failure when it cannot.
func (in *instance) prepare(s *service) (executor.Spec, []io.Closer, Failure) {
	pr := s.def.Process
	path, err := executor.TrustedExecutable(pr.Command)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		in.log.Error("supervisor cannot start the process: the program is missing", "service", s.def.Name, "error", err)
		return executor.Spec{}, nil, FailureNotFound
	case err != nil:
		in.log.Error("supervisor refuses to start the process: the program is not trusted", "service", s.def.Name, "error", err)
		return executor.Spec{}, nil, FailureUntrusted
	}
	spec := executor.Spec{Path: path, Arg0: pr.Command, Args: pr.Args, Dir: pr.WorkingDir, Env: pr.Env, Cred: pr.Cred}
	// The process keeps the redaction of the configuration it was
	// launched under, whatever a reload does later (review 3b-4 C4).
	redact := in.redactor()
	var sinks []io.Closer
	switch pr.Output.Kind {
	case outputLog:
		for _, stream := range []string{"stdout", "stderr"} {
			w := executor.NewLineWriter(in.rt.Clock, executor.DefaultLineOptions, func(line string) {
				in.log.Info("service output", "service", s.def.Name, "stream", stream, "line", redact(line))
			})
			if stream == "stdout" {
				spec.Stdout = w
			} else {
				spec.Stderr = w
			}
			sinks = append(sinks, w)
		}
	case outputFile:
		f, err := executor.OpenAppend(pr.Output.File, pr.Output.Mode)
		if err != nil {
			in.log.Error("supervisor cannot open the output file", "service", s.def.Name, "error", err)
			return executor.Spec{}, nil, FailureOutput
		}
		spec.Stdout, spec.Stderr = f, f
		sinks = append(sinks, f)
	}
	return spec, sinks, ""
}

// launchFailure classifies a start error.
func launchFailure(err error) Failure {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return FailureNotFound
	case errors.Is(err, fs.ErrPermission), errors.Is(err, syscall.EPERM):
		return FailurePermission
	case errors.Is(err, executor.ErrUnsupported):
		return FailureUnsupported
	}
	return FailureExec
}

// watch follows a launched process until it ends or the module stops.
func (in *instance) watch(ctx context.Context, s *service, lp launched, sinks []io.Closer) {
	grace := in.rt.Clock.NewTimer(s.def.Process.StartupGrace)
	defer grace.Stop()
	stable := in.rt.Clock.NewTimer(s.def.Process.Recovery.StableAfter)
	defer stable.Stop()
	starting := true
	for {
		select {
		case <-lp.proc.Done():
			if ctx.Err() != nil {
				return // shutdown is not a failure; Stop records the end
			}
			closeAll(sinks)
			exit, outErr := lp.proc.Result()
			in.processExited(s, exit, outErr, starting)
			in.release(ctx, s, lp.group)
			return
		case <-grace.C():
			starting = false
			in.passedGrace(s)
		case <-stable.C():
			in.stable(s)
		case <-ctx.Done():
			return
		}
	}
}

// passedGrace marks s running; a service seen for the first time, or
// healthy, is healthy now. A failed one stays failed until stable_after.
func (in *instance) passedGrace(s *service) {
	now := in.rt.Clock.Now()
	in.mu.Lock()
	if s.phase == phaseStarting {
		s.phase = phaseRunning
	}
	changed := false
	if s.st.Health == HealthUnknown {
		s.prev, s.st.Health, s.st.Since = s.st.Health, HealthHealthy, now
		changed = true
	}
	in.gen++
	gen := in.gen
	in.mu.Unlock()
	if changed {
		in.save(gen)
	}
}

// stable closes the open incident of a process that has run for
// stable_after (D-084 (6)). The attempts stay: the window, not stability,
// frees the budget, so a slow flapper is still bounded (D-087).
func (in *instance) stable(s *service) {
	now := in.rt.Clock.Now()
	in.mu.Lock()
	if s.st.Health != HealthFailed {
		in.mu.Unlock()
		return
	}
	incident, since := s.st.IncidentID, s.st.Since
	s.prev, s.st.Health, s.st.IncidentID, s.st.Since = HealthFailed, HealthHealthy, "", now
	e := processRecoveredEvent(s, incident, now.Sub(since))
	in.gen++
	gen := in.gen
	in.mu.Unlock()
	in.save(gen)
	in.publish(s, e)
}

// processExited records the unrequested end of s's process: always a
// failure, status 0 included (D-084 (6)).
func (in *instance) processExited(s *service, exit executor.Exit, outErr error, startup bool) {
	code := FailureExited
	if exit.Signal != "" {
		code = FailureKilled
	}
	in.log.Warn("supervisor process ended", "service", s.def.Name, "exit", exit.String(), "during_startup", startup)
	in.processFailed(s, code, &exit, startup)
	in.mu.Lock()
	s.outputLost = outErr != nil
	in.mu.Unlock()
}

// processFailed records a failure of s: a start that failed, or a process
// that ended. The first failure of an incident emits service_failed;
// while an incident is open, later ones only count. State first, then the
// event, as observe does.
func (in *instance) processFailed(s *service, code Failure, exit *executor.Exit, startup bool) {
	now := in.rt.Clock.Now()
	in.mu.Lock()
	st := &s.st
	st.Failures++
	st.LastFailure = code
	if exit != nil {
		st.LastExit = &exitRecord{Code: exit.Code, Signal: exit.Signal, At: now}
		s.phase = phaseExited
		s.proc, s.pid, s.startedAt, s.sinks, s.group = nil, 0, time.Time{}, nil, executor.Group{}
	} else {
		s.phase = phaseStartFailed
	}
	var ev *model.Event
	if st.Health != HealthFailed {
		s.prev = st.Health
		st.Health, st.IncidentID, st.Since = HealthFailed, model.NewEventID(), now
		e := processFailedEvent(s, st.IncidentID, exit, startup)
		ev = &e
	}
	in.gen++
	gen := in.gen
	in.mu.Unlock()
	in.save(gen)
	if ev != nil {
		in.publish(s, *ev)
	}
}

// publish publishes e, logging a failure.
func (in *instance) publish(s *service, e model.Event) {
	if _, err := in.rt.Events.Publish(e); err != nil {
		in.log.Error("supervisor event not published", "service", s.def.Name, "event_type", e.Type, "error", err)
	}
}

func (in *instance) setPhase(s *service, phase string) {
	in.mu.Lock()
	s.phase = phase
	in.mu.Unlock()
}

// stopProcesses stops every process the module owns, in parallel, each
// with its stop signal and timeout under ctx (the executor kills the group
// when ctx ends first). A process sentineld stops is not a failure. It
// returns how many did not end; their records stay for the next start.
func (in *instance) stopProcesses(ctx context.Context) int {
	type owned struct {
		s     *service
		p     proc
		sinks []io.Closer
	}
	var all []owned
	in.mu.Lock()
	in.collected = true // a launch that completes from now on stops itself
	for _, s := range in.services {
		if s.proc != nil {
			all = append(all, owned{s, s.proc, s.sinks})
			s.phase = phaseStopping
		}
	}
	in.mu.Unlock()
	results := make(chan bool, len(all))
	for _, o := range all {
		go func() { results <- in.stopOne(ctx, o.s, o.p, o.sinks) }()
	}
	left := 0
	for range all {
		if !<-results {
			left++
		}
	}
	return left
}

// stopOne stops p, the process of s, with its stop signal and timeout
// under ctx, and reports whether it ended. A process sentineld stops is
// not a failure.
func (in *instance) stopOne(ctx context.Context, s *service, p proc, sinks []io.Closer) bool {
	pr := s.def.Process
	exit, err := p.Stop(ctx, pr.StopSignal, pr.StopTimeout)
	if err != nil {
		in.log.Error("supervisor process did not end when stopped", "service", s.def.Name, "error", err)
		return false
	}
	closeAll(sinks)
	now := in.rt.Clock.Now()
	in.mu.Lock()
	g := s.group
	s.st.LastExit = &exitRecord{Code: exit.Code, Signal: exit.Signal, At: now}
	s.phase = phaseStopped
	s.proc, s.pid, s.startedAt, s.sinks, s.group = nil, 0, time.Time{}, nil, executor.Group{}
	in.mu.Unlock()
	in.release(ctx, s, g)
	return true
}

func closeAll(cs []io.Closer) {
	for _, c := range cs {
		_ = c.Close() // a log line writer never fails; an output file is append-only
	}
}

// processRedactor masks, in a process's output, the configuration's
// secrets and the values of every process service's environment, which may
// be literal tokens the configuration does not know as secrets (D-086), in
// one pass over the original text, longest first: a secret that contains
// another is masked whole (review 3b-2 R4). Values shorter than four bytes
// are left alone, as by redact.Text; arguments and scripts are not masked:
// secrets belong in the environment.
func processRedactor(configSecrets []string, services []*service) func(string) string {
	secrets := slices.Clone(configSecrets)
	for _, s := range services {
		for _, kv := range s.def.Process.Env {
			if k, v, _ := strings.Cut(kv, "="); k != "PATH" {
				secrets = append(secrets, v)
			}
		}
	}
	slices.SortFunc(secrets, func(a, b string) int { return len(b) - len(a) })
	secrets = slices.Compact(secrets)
	return func(text string) string { return redact.Text(text, secrets...) }
}
