package supervisor

import (
	"context"
	"errors"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/audit"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/platform/systemd"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/model"
)

// restartPoll is how often an attempt reads its unit while systemd's job
// runs: the job's end is read from the Job property, not from a signal
// (D-093 (3)).
const restartPoll = time.Second

// Why an attempt on a unit failed, in its audit record (never a Failure
// code: the next observation decides the service's health).
const (
	failureJobFailed      = "job_failed"      // the job ended and the unit is not running again
	failureJobTimeout     = "job_timeout"     // restart_timeout passed with the job still there
	failureOutcomeUnknown = "outcome_unknown" // no job id and no new invocation by restart_timeout
	failureUnavailable    = "systemd_unavailable"
)

// blockedAliasUnresolved: a restart waits for every other systemd service's
// first read (review 3c-2 C4).
const blockedAliasUnresolved = "alias_unresolved"

// unitPlan is the next automatic attempt of a systemd service: due at at,
// once its delay has passed. Zero: none is planned.
type unitPlan struct {
	a  attempt
	at time.Time
	// blocked: the attempt waits because the audit log or the state file
	// failed (blockedAudit, blockedState).
	blocked string
}

// restartable reports whether sentineld may restart a unit seen in st:
// loaded, with no job (an administrator's or systemd's own: mode fail
// would refuse it anyway), and for an automatic attempt settled not
// running (failed or inactive). A unit stuck in a transition is systemd's
// to finish: a restart would race it.
func restartable(st systemd.UnitState, explicit bool) bool {
	if st.Load != systemd.LoadLoaded || st.Job != 0 {
		return false
	}
	return explicit || (!unsettled(st) && (st.Active == systemd.ActiveFailed || st.Active == systemd.ActiveInactive))
}

// recoverUnit decides, after an observation, whether s's unit is to be
// restarted now, and does it (D-095). It returns how long the loop waits
// before its next read: wait, or less when an attempt or the end of a
// cooldown is due sooner, and the read still stalled, if any. The loop is
// armed (the switch and the service's recovery). A panic here is the
// loop's (iterateUnit recovers its own; this one ends the loop's round).
//
// c, when an operator's, attributes an attempt planned now to them.
func (in *instance) recoverUnit(ctx context.Context, s *service, plan *unitPlan, wait time.Duration, c cause) (time.Duration, <-chan readResult) {
	now := in.rt.Clock.Now()
	in.mu.Lock()
	if plan.blocked != "" && s.st.Health == HealthFailed && s.undecided != "" {
		// An attempt blocked by the audit log or the state file keeps its
		// max_delay through checks that decide nothing (review 3c-2 R6);
		// once due, it waits for a decided check at the usual pace (R17).
		in.mu.Unlock()
		if now.Before(plan.at) {
			wait = min(wait, plan.at.Sub(now))
		}
		return wait, nil
	}
	if s.st.Health != HealthFailed || s.undecided != "" || !restartable(s.unit.last, false) {
		*plan = unitPlan{}
		s.nextAttemptAt, s.resumeAt, s.blocked = time.Time{}, time.Time{}, ""
		if s.phase == phaseRestarting || s.phase == phaseExhausted {
			s.phase = ""
		}
		in.mu.Unlock()
		return wait, nil
	}
	if plan.at.IsZero() {
		r := s.def.Recovery
		b, adm := admit(s.st.budget(), now, r)
		changed := !sameBudget(b, s.st.budget())
		s.st.setBudget(b)
		if !adm.OK {
			s.phase, s.resumeAt, s.nextAttemptAt, s.blocked = phaseExhausted, adm.ResumeAt, time.Time{}, ""
			var ev *model.Event
			if adm.Exhausted {
				e := exhaustedEvent(s, s.st.IncidentID, adm.ResumeAt)
				ev = &e
			}
			in.gen++
			gen := in.gen
			in.mu.Unlock()
			if changed {
				in.save(gen)
			}
			if ev != nil {
				in.log.Error("supervisor recovery exhausted; the unit is not restarted again before the cooldown ends or a reset",
					"service", s.def.Name, "resume_at", adm.ResumeAt)
				in.publish(s, *ev)
			}
			if !adm.ResumeAt.IsZero() {
				wait = min(wait, max(adm.ResumeAt.Sub(now), 0))
			}
			return wait, nil
		}
		plan.a = attempt{n: adm.Attempt, trigger: triggerFailure, correlation: s.st.IncidentID}
		plan.at = now.Add(delay(adm.Attempt, r))
		switch {
		case c.operator: // asked now (reset, enable): no delay
			plan.a.trigger, plan.a.request, plan.at = triggerOperator, c.request, now
		case adm.Renewed:
			plan.a.trigger, plan.at = triggerCooldown, now
		}
		s.phase, s.nextAttemptAt, s.resumeAt, s.blocked = phaseRestarting, plan.at, time.Time{}, ""
		in.gen++
		gen := in.gen
		in.mu.Unlock()
		if changed {
			in.save(gen)
		}
		in.mu.Lock()
	}
	if now.Before(plan.at) {
		s.phase, s.nextAttemptAt, s.blocked = phaseRestarting, plan.at, plan.blocked // an observation clears the phase
		if plan.blocked != "" {
			s.phase = phaseBlocked
		}
		in.mu.Unlock()
		return min(wait, plan.at.Sub(now)), nil
	}
	a := plan.a
	in.mu.Unlock()
	*plan = unitPlan{}
	out, still := in.restartUnit(ctx, s, a, false)
	if out.blocked != "" {
		// The audit log or the state file failed: the same attempt waits
		// max_delay, whatever the observations in between (review 3c-2 R6).
		*plan = unitPlan{a: a, at: in.rt.Clock.Now().Add(s.def.Recovery.MaxDelay), blocked: out.blocked}
		wait = min(wait, s.def.Recovery.MaxDelay)
	}
	return wait, still
}

// restartOutcome is how an attempt on a unit ended: done when systemd
// restarted it (a new invocation, active); otherwise why says why not,
// for an operator, and refused that nothing was asked of systemd and no
// attempt spent (the fresh read found nothing to restart, or the attempt
// could not be recorded).
type restartOutcome struct {
	done    bool
	refused bool
	why     string
	// blocked: refused because the audit log or the state file failed.
	blocked string
}

// restartUnit restarts s's unit once for a (D-095): a fresh read decides
// whether it still applies, then the intent is audited, the attempt (when
// a counts as one) saved with the invocation it replaces, systemd asked
// in mode fail, and its job read every restartPoll until it ends or
// restart_timeout passes. explicit is an operator's restart, which a
// running unit takes too. It returns the read still stalled, if any,
// which the caller's loop must wait for. The job is never cancelled: when
// ctx ends first, it is systemd's.
func (in *instance) restartUnit(ctx context.Context, s *service, a attempt, explicit bool) (restartOutcome, <-chan readResult) {
	r, still := in.readFresh(ctx, s, nil)
	if r.err != nil {
		return restartOutcome{refused: true, why: "the unit cannot be read now"}, still
	}
	if !restartable(r.st, explicit) {
		why := "the unit does not need a restart"
		if r.st.Job != 0 {
			why = "systemd has a job queued for the unit"
		} else if r.st.Load != systemd.LoadLoaded {
			why = "the unit is not loaded (" + string(r.st.Load) + ")"
		}
		return restartOutcome{refused: true, why: why}, nil
	}
	in.mu.Lock()
	alias, unresolved := in.aliasOfLocked(s, r.st.ID) != nil, in.unresolvedLocked(s)
	if unresolved && !alias {
		s.blocked = blockedAliasUnresolved
	}
	in.mu.Unlock()
	switch {
	case alias:
		return restartOutcome{refused: true, why: "another service observes the same unit under another name"}, nil
	case unresolved:
		return restartOutcome{refused: true, why: "another systemd service has not been read yet; it may name the same unit"}, nil
	}
	before := r.st.InvocationID
	retry := s.def.Recovery.MaxDelay
	if err := in.audit(ctx, s, a, audit.ResultIntent, nil); err != nil {
		if ctx.Err() == nil {
			in.log.Error("supervisor cannot record the restart in the audit log; the unit is not restarted",
				"service", s.def.Name, "error", err)
			in.setPending(s, phaseBlocked, in.rt.Clock.Now().Add(retry), blockedAudit)
		}
		return restartOutcome{refused: true, why: "the audit log cannot record the restart", blocked: blockedAudit}, nil
	}
	in.mu.Lock()
	// The fresh read's automatic restarts are counted before ours resets
	// the count (review 3c-2 F3); the mark is saved with the attempt, so
	// our restart is not systemd's (C7); stable_after starts again (F4).
	var auto uint64
	auto, s.st.UnitCounter = countRestarts(s.st.UnitCounter, r.st, in.boot, s.st.UnitOwnRestart)
	flap := in.restartedBySystemdLocked(s, auto, in.rt.Clock.Now())
	s.st.UnitOwnRestart = before
	s.unit.activeSince = time.Time{}
	in.mu.Unlock()
	defer in.publishFlapping(s, flap)
	if a.n > 0 {
		if !in.reserve(ctx, s, a) {
			in.mu.Lock()
			s.st.UnitOwnRestart = ""
			in.mu.Unlock()
			return restartOutcome{refused: true, why: "the state file cannot record the attempt", blocked: blockedState}, nil
		}
	} else {
		in.mu.Lock()
		in.gen++
		gen := in.gen
		in.mu.Unlock()
		in.save(gen) // not an attempt: a failed save only risks counting this restart as systemd's
	}
	in.setPending(s, phaseRestarting, time.Time{}, "")
	// The call and every wait and read of its job end at restart_timeout,
	// a slot taken or not (review 3c-2 R2, R19); ctx ending first is a
	// cancellation.
	deadline := in.rt.Clock.Now().Add(s.def.Recovery.RestartTimeout)
	pctx, pcancel := context.WithDeadline(ctx, deadline)
	defer pcancel()
	cctx, cancel := context.WithTimeout(pctx, s.def.Timeout)
	job, err := in.units.Restart(cctx, s.def.Unit.Name)
	cancel()
	var refusal *systemd.RefusedError
	switch {
	case err == nil:
	case errors.As(err, &refusal):
		in.notQueued(s)
		in.restartFailed(ctx, s, a, refusal.Code)
		return restartOutcome{why: "systemd refused the restart: " + refusal.Code}, nil
	case !errors.Is(err, systemd.ErrUnknownOutcome):
		in.notQueued(s)
		if ctx.Err() != nil {
			in.restartFailed(ctx, s, a, failureCanceled)
		} else {
			in.restartFailed(ctx, s, a, failureUnavailable)
		}
		return restartOutcome{why: "systemd cannot be reached"}, nil
	default:
		in.log.Warn("supervisor does not know whether systemd took the restart; it reads the unit to find out",
			"service", s.def.Name)
	}
	for {
		in.sleep(pctx, min(restartPoll, max(deadline.Sub(in.rt.Clock.Now()), 0)))
		if ctx.Err() != nil {
			in.restartFailed(ctx, s, a, failureCanceled)
			return restartOutcome{why: "the supervisor stopped waiting"}, still
		}
		var p readResult
		p, still = in.readFresh(pctx, s, still)
		if ctx.Err() != nil { // a cancellation during the read is not restart_timeout (review 3c-2 R16)
			in.restartFailed(ctx, s, a, failureCanceled)
			return restartOutcome{why: "the supervisor stopped waiting"}, still
		}
		over := pctx.Err() != nil || !in.rt.Clock.Now().Before(deadline)
		switch {
		case p.err != nil: // unreadable or stalled: the next poll decides
		case job != 0 && p.st.Job == job, job == 0 && p.st.Job != 0:
			// Our job, or perhaps ours (the outcome was unknown), still runs.
		case p.st.Active == systemd.ActiveActive && p.st.InvocationID != before:
			in.restartDone(ctx, s, a)
			return restartOutcome{done: true}, still
		case job == 0 && p.st.InvocationID == before:
			// No job and the same invocation: the unknown restart did not
			// happen; the attempt is spent (D-093 (3)), the mark goes
			// (review 3c-2 R20).
			in.notQueued(s)
			in.restartFailed(ctx, s, a, failureOutcomeUnknown)
			return restartOutcome{why: "systemd did not restart the unit"}, still
		default:
			in.restartFailed(ctx, s, a, failureJobFailed)
			return restartOutcome{why: "the unit did not start again"}, still
		}
		if over {
			code := failureJobTimeout
			if job == 0 {
				code = failureOutcomeUnknown
			}
			in.restartFailed(ctx, s, a, code)
			return restartOutcome{why: "systemd's job did not end within restart_timeout"}, still
		}
	}
}

// notQueued forgets the invocation saved for an attempt that reached
// nothing in systemd (refused, or never sent): a new invocation later is
// not sentineld's (review 3c-2 R5).
func (in *instance) notQueued(s *service) {
	in.mu.Lock()
	s.st.UnitOwnRestart = ""
	in.gen++
	gen := in.gen
	in.mu.Unlock()
	in.save(gen)
}

// readFresh reads s's unit once outside the loop's round, with the
// protections of a round: a slot, the service's timeout, and no new read
// while stalled (an earlier read that has not returned) is pending. It
// returns the read, or an error when none could be made, and the read
// still stalled. It records nothing: an attempt's reads are not
// observations, so they never move the policy's streaks.
func (in *instance) readFresh(ctx context.Context, s *service, stalled <-chan readResult) (readResult, <-chan readResult) {
	if stalled != nil {
		select {
		case <-stalled:
			in.setStalled(s, false)
		default:
			return readResult{err: errStalled}, stalled
		}
	}
	select {
	case in.slots <- struct{}{}:
	case <-ctx.Done():
		return readResult{err: ctx.Err()}, nil
	}
	result, cancel := in.readUnit(ctx, s)
	timer := in.rt.Clock.NewTimer(s.def.Timeout)
	defer timer.Stop()
	select {
	case r := <-result:
		cancel()
		return r, nil
	case <-timer.C():
		cancel() // until the read returns, it keeps its slot
		in.setStalled(s, true)
		return readResult{err: context.DeadlineExceeded}, result
	case <-ctx.Done():
		cancel()
		return readResult{err: ctx.Err()}, result
	}
}

// restartDone records an attempt systemd carried out: the unit runs again
// with a new invocation. An attempt of an incident is reported; the
// incident closes on the observations, after stable_after.
func (in *instance) restartDone(ctx context.Context, s *service, a attempt) {
	in.outcome(ctx, s, a, audit.ResultOK, nil)
	in.mu.Lock()
	s.st.Starts++
	s.phase, s.nextAttemptAt = "", time.Time{}
	in.gen++
	gen := in.gen
	in.mu.Unlock()
	in.save(gen)
	if a.n > 0 {
		in.restarted(s, a)
		return
	}
	in.log.Info("supervisor restarted the unit", "service", s.def.Name, "trigger", a.trigger)
}

// restartFailed records an attempt that did not restart the unit, for
// code; a saved attempt stays spent (D-014).
func (in *instance) restartFailed(ctx context.Context, s *service, a attempt, code string) {
	in.outcome(ctx, s, a, audit.ResultFailed, map[string]string{"failure": code})
	in.setPhase(s, "")
	in.log.Warn("supervisor could not restart the unit", "service", s.def.Name, "failure", code, "attempt", a.n)
}

// unitAuditDetail adds the unit to an audit record of s, a systemd
// service: the configured name only.
func unitAuditDetail(s *service, detail map[string]string) {
	if s.def.Type == typeSystemd {
		detail["unit"] = s.def.Unit.Name.String()
	}
}
