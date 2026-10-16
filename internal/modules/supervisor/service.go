package supervisor

import (
	"context"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/model"
)

// run is the loop of one service: a check now, then one check_interval
// after each check ends (no overlap, no catch-up). It ends with ctx.
func (in *instance) run(ctx context.Context, s *service) {
	defer in.workers.Done()
	var stalled <-chan Result // a check that timed out and has not returned
	for {
		stalled = in.iterate(ctx, s, stalled)
		timer := in.rt.Clock.NewTimer(s.def.CheckInterval)
		select {
		case <-timer.C():
		case <-ctx.Done():
			timer.Stop()
			return
		}
	}
}

// iterate runs one check of s and records its outcome. A check still
// stalled from an earlier round counts as timed out again, without a new
// call: a service has at most one check running. It returns the stalled
// check, if any. A panic in the module's own code here is logged without
// its value and the loop goes on.
func (in *instance) iterate(ctx context.Context, s *service, stalled <-chan Result) (still <-chan Result) {
	still = stalled
	defer func() {
		if recover() != nil {
			in.log.Error("supervisor loop panicked; the service goes on", "service", s.def.Name)
		}
	}()
	if stalled != nil {
		select {
		case <-stalled: // returned meanwhile; its result is stale
			still = nil
			in.setStalled(s, false)
		default:
			in.observe(s, Result{Failure: FailureTimeout}, 0, s.def.Timeout)
			return still
		}
	}
	waitStart := in.rt.Clock.Now()
	in.setWaiting(s, waitStart)
	select {
	case in.slots <- struct{}{}:
	case <-ctx.Done():
		in.setWaiting(s, time.Time{})
		return still
	}
	begin := in.rt.Clock.Now()
	in.setWaiting(s, time.Time{})
	done := in.launch(ctx, s)
	timer := in.rt.Clock.NewTimer(s.def.Timeout)
	defer timer.Stop()
	select {
	case r := <-done.result:
		done.cancel()
		in.observe(s, normalize(r), begin.Sub(waitStart), in.rt.Clock.Now().Sub(begin))
	case <-timer.C():
		done.cancel() // the check should end now; until it does, it keeps its slot
		in.setStalled(s, true)
		in.observe(s, Result{Failure: FailureTimeout}, begin.Sub(waitStart), s.def.Timeout)
		return done.result
	case <-ctx.Done():
		done.cancel() // shutdown is not a failure: nothing is recorded; Stop waits for the check
	}
	return still
}

// running is one check in flight.
type running struct {
	result <-chan Result // receives the result once; buffered, so the check never blocks
	cancel context.CancelFunc
}

// launch runs s's check in its own goroutine, holding a slot that the
// caller took: the slot is given back only when Check returns, so
// max_concurrent_checks counts real work. A panicking checker gives
// FailurePanic, logged without its value.
func (in *instance) launch(ctx context.Context, s *service) running {
	cctx, cancel := context.WithCancel(ctx)
	result := make(chan Result, 1)
	in.checks.Add(1)
	in.running.Add(1)
	go func() {
		defer in.checks.Done()
		defer in.running.Add(-1)
		defer func() { <-in.slots }()
		defer func() {
			if recover() != nil {
				in.log.Error("supervisor check panicked", "service", s.def.Name)
				result <- Result{Failure: FailurePanic}
			}
		}()
		result <- s.checker.Check(cctx)
	}()
	return running{result: result, cancel: cancel}
}

// observe applies one outcome to s: counters, policy, and on a
// transition the state file (first) and the event (then). A crash between
// the two may lose or repeat one event (D-080 (3), no outbox).
func (in *instance) observe(s *service, r Result, waited, took time.Duration) {
	now := in.rt.Clock.Now()
	in.mu.Lock()
	st := &s.st
	next, tr := step(streak{st.Health, st.ConsecutiveFailures, st.ConsecutiveSuccesses}, r.OK(), s.def.Policy)
	prev := st.Health
	st.Health, st.ConsecutiveFailures, st.ConsecutiveSuccesses = next.Health, next.Failures, next.Successes
	st.Checks++
	if !r.OK() {
		st.Failures++
		st.LastFailure = r.Failure
	}
	st.LastCheck = now
	s.lastDuration, s.lastSlotWait = took, waited
	var ev *model.Event
	switch tr {
	case becameUp:
		st.Since = now
	case failed:
		s.prev = prev
		st.IncidentID = model.NewEventID()
		st.Since = now
		e := failedEvent(s, st.IncidentID)
		ev = &e
	case recovered:
		s.prev = prev
		e := recoveredEvent(s, st.IncidentID, now.Sub(st.Since))
		ev = &e
		st.IncidentID = ""
		st.Since = now
	}
	in.gen++
	gen := in.gen
	due := tr != noChange || now.Sub(in.lastSave) >= saveEvery || in.notSaved
	in.mu.Unlock()
	if due {
		in.save(gen)
	}
	if ev != nil {
		if _, err := in.rt.Events.Publish(*ev); err != nil {
			in.log.Error("supervisor event not published", "service", s.def.Name, "event_type", ev.Type, "error", err)
		}
	}
}

func (in *instance) setWaiting(s *service, since time.Time) {
	in.mu.Lock()
	s.waitingSince = since
	in.mu.Unlock()
}

func (in *instance) setStalled(s *service, stalled bool) {
	in.mu.Lock()
	s.stalled = stalled
	in.mu.Unlock()
}
