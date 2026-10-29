package supervisor

import (
	"context"
	"errors"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/platform/systemd"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/model"
)

// unitReader reads one unit's state. The variation it isolates is
// systemd's D-Bus (systemd.Bus) versus the fakes of the tests, which need
// no systemd, bus or root.
type unitReader interface {
	Unit(ctx context.Context, n systemd.UnitName) (systemd.UnitState, error)
	Close() error
}

// Phases of a systemd service whose last check decided nothing.
const (
	phaseUnavailable = "unavailable" // systemd or its bus could not be read
	phaseTransition  = "transition"  // the unit is unsettled, within transition_grace
)

// Why a check of a unit decided nothing (status' undecided).
const (
	undecidedUnavailable = "systemd_unavailable" // systemd could not be reached, or did not answer in time
	undecidedChanging    = "unit_changing"       // the unit changed while it was read
	undecidedInvalid     = "unit_invalid"        // systemd's reply could not be read: never a failure (review 3c-1 F4)
	undecidedTransition  = "unit_transition"     // the unit is unsettled, within transition_grace
)

// unitMemo is what the loop of a systemd service remembers between its
// observations, beyond the policy's streak. It is in memory only in Phase
// 3c-1: after a restart of sentineld the first observation sets the
// baseline again, so a restart by systemd during the downtime is not seen.
type unitMemo struct {
	last systemd.UnitState // the last state read; zero before the first
	// The baseline of restart detection: the invocation and restart count
	// at the last active observation; based is false until there is one,
	// or after another unit appears behind the name. Neutral and failed
	// observations never move it (review 3c-1 C3, F2): a unit seen failed
	// and then active with a new invocation was started again, which is a
	// restart too, so one that crashes and is restarted at every interval
	// never counts a success.
	based          bool
	baseInvocation string
	baseRestarts   uint32
	// unsettledSince is when the current unsettled period began; zero
	// while the unit is settled.
	unsettledSince time.Time
	// restarts counts the restarts by systemd seen so far.
	restarts uint64
}

// verdict is one observation's outcome: a failure, a reason to decide
// nothing (undecided), or neither, a success.
type verdict struct {
	failure   Failure
	undecided string
}

// judgeUnit decides what state st, read at now, means for a unit with
// memo m and transition_grace grace. It is a pure function: the clock,
// the state file and the events are the caller's.
func judgeUnit(m unitMemo, st systemd.UnitState, now time.Time, grace time.Duration) (verdict, unitMemo) {
	if m.last.ID != "" && st.ID != m.last.ID {
		m.based = false // another unit behind the name: start over
	}
	m.last = st
	switch st.Load {
	case systemd.LoadNotFound:
		return settledFailure(m, FailureUnitNotFound)
	case systemd.LoadMasked:
		return settledFailure(m, FailureUnitMasked)
	case systemd.LoadBadSetting, systemd.LoadError:
		return settledFailure(m, FailureUnitLoadFailed)
	}
	if unsettled(st) {
		if m.unsettledSince.IsZero() {
			m.unsettledSince = now
		}
		if now.Sub(m.unsettledSince) < grace {
			return verdict{undecided: undecidedTransition}, m
		}
		return verdict{failure: FailureUnitStuck}, m
	}
	m.unsettledSince = time.Time{}
	switch st.Active {
	case systemd.ActiveFailed:
		return settledFailure(m, FailureUnitFailed)
	case systemd.ActiveInactive:
		return settledFailure(m, FailureUnitInactive)
	}
	// Active: compare with the baseline, then move it here.
	var n uint64
	if m.based {
		if st.NRestarts > m.baseRestarts {
			n = uint64(st.NRestarts - m.baseRestarts)
		}
		if n == 0 && st.InvocationID != m.baseInvocation {
			n = 1 // started again with no count: a manual start resets NRestarts
		}
	}
	m.based, m.baseInvocation, m.baseRestarts = true, st.InvocationID, st.NRestarts
	if n > 0 {
		m.restarts += n
		return verdict{failure: FailureUnitRestarted}, m
	}
	return verdict{}, m
}

// settledFailure is the verdict of a settled unit that is not running: it
// ends any unsettled period and keeps the restart baseline.
func settledFailure(m unitMemo, f Failure) (verdict, unitMemo) {
	m.unsettledSince = time.Time{}
	return verdict{failure: f}, m
}

// unsettled reports whether st is on its way somewhere: a job queued, an
// automatic restart pending, or a transitional state; stub and merged
// are load states systemd passes through.
func unsettled(st systemd.UnitState) bool {
	switch {
	case st.Job, st.Sub == "auto-restart", st.Load == systemd.LoadStub, st.Load == systemd.LoadMerged:
		return true
	}
	switch st.Active {
	case systemd.ActiveActivating, systemd.ActiveDeactivating, systemd.ActiveReloading,
		systemd.ActiveRefreshing, systemd.ActiveMaintenance:
		return true
	}
	return false
}

// readResult is the outcome of one read of a unit.
type readResult struct {
	st  systemd.UnitState
	err error
}

// errStalled stands for a read still running from an earlier round.
var errStalled = errors.New("the previous read has not returned")

// runUnit is the loop of one systemd service: a read now, then one
// check_interval after each read ends (no overlap, no catch-up), as run
// does for http services. It has its own classification: a read that
// fails or does not return in time decides nothing, so systemd's slowness
// never fails a unit (review 3c-1 C2). It ends with ctx, or at once when
// g holds the service back (disabled).
func (in *instance) runUnit(ctx context.Context, s *service, g gate) {
	if g != gateNone {
		in.setPhase(s, g.phase())
		return
	}
	in.setPhase(s, "")
	s.decide()
	var stalled <-chan readResult
	for {
		stalled = in.iterateUnit(ctx, s, stalled)
		timer := in.rt.Clock.NewTimer(s.def.CheckInterval)
		select {
		case <-timer.C():
		case <-ctx.Done():
			timer.Stop()
			return
		}
	}
}

// iterateUnit reads s once, or counts the read still stalled from an
// earlier round as undecided without a new one, and records the outcome.
// It returns the read still stalled, if any. A panic in the module's own
// code here is logged without its value and the loop goes on.
func (in *instance) iterateUnit(ctx context.Context, s *service, stalled <-chan readResult) (still <-chan readResult) {
	still = stalled
	defer func() {
		if recover() != nil {
			in.log.Error("supervisor unit loop panicked; the service goes on", "service", s.def.Name)
		}
	}()
	if ctx.Err() != nil {
		return still
	}
	if stalled != nil {
		select {
		case <-stalled: // returned meanwhile; its result is stale
			still = nil
			in.setStalled(s, false)
		default:
			in.recordUnit(ctx, s, readResult{err: errStalled}, 0, s.def.Timeout)
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
	result, cancel := in.readUnit(ctx, s)
	timer := in.rt.Clock.NewTimer(s.def.Timeout)
	defer timer.Stop()
	select {
	case r := <-result:
		cancel()
		in.recordUnit(ctx, s, r, begin.Sub(waitStart), in.rt.Clock.Now().Sub(begin))
	case <-timer.C():
		cancel() // the read should end now; until it does, it keeps its slot
		in.setStalled(s, true)
		in.recordUnit(ctx, s, readResult{err: context.DeadlineExceeded}, begin.Sub(waitStart), s.def.Timeout)
		return result
	case <-ctx.Done():
		cancel() // shutdown decides nothing; Stop waits for the read
	}
	return still
}

// readUnit reads s's unit in its own goroutine, with a deadline of the
// service's timeout (the reader bounds its calls by it), holding a slot
// that the caller took until the read returns. A panicking reader decides
// nothing, logged without its value.
func (in *instance) readUnit(ctx context.Context, s *service) (<-chan readResult, context.CancelFunc) {
	rctx, cancel := context.WithTimeout(ctx, s.def.Timeout)
	result := make(chan readResult, 1)
	in.checks.Add(1)
	s.checks.Add(1)
	in.running.Add(1)
	go func() {
		defer in.checks.Done()
		defer s.checks.Done()
		defer in.running.Add(-1)
		defer func() { <-in.slots }()
		defer func() {
			if recover() != nil {
				in.log.Error("supervisor unit read panicked", "service", s.def.Name)
				result <- readResult{err: errors.New("the read panicked")}
			}
		}()
		st, err := in.units.Unit(rctx, s.def.Unit.Name)
		result <- readResult{st: st, err: err}
	}()
	return result, cancel
}

// recordUnit records one read of s unless the module has ended (shutdown
// is never a failure): the verdict, the observed state and, for a decided
// read, the policy, all under one lock, so status and the event agree with
// the observation (review 3c-1 C5).
func (in *instance) recordUnit(ctx context.Context, s *service, r readResult, waited, took time.Duration) {
	if ctx.Err() != nil {
		return
	}
	now := in.rt.Clock.Now()
	in.mu.Lock()
	var v verdict
	switch {
	case errors.Is(r.err, systemd.ErrChanging):
		v.undecided = undecidedChanging
	case errors.Is(r.err, systemd.ErrInvalid):
		v.undecided = undecidedInvalid
	case r.err != nil:
		v.undecided = undecidedUnavailable
	default:
		v, s.unit = judgeUnit(s.unit, r.st, now, s.def.Unit.TransitionGrace)
	}
	s.lastDuration, s.lastSlotWait = took, waited
	was := s.undecided
	s.undecided = v.undecided
	var ev *model.Event
	var gen uint64
	var due bool
	if v.undecided != "" {
		gen, due = in.undecidedLocked(s, v.undecided, now)
	} else {
		s.phase = ""
		ev, gen, due = in.applyLocked(s, Result{Failure: v.failure}, now)
	}
	in.mu.Unlock()
	in.logUndecided(s, was, v.undecided)
	in.settle(s, ev, gen, due)
}

// undecidedLocked records a check of s that decided nothing, for reason:
// it counts as a check and shows in the phase, and moves no streak. The
// caller holds in.mu, then calls settle.
func (in *instance) undecidedLocked(s *service, reason string, now time.Time) (gen uint64, due bool) {
	s.phase = phaseUnavailable
	if reason == undecidedTransition {
		s.phase = phaseTransition
	}
	s.st.Checks++
	s.st.LastCheck = now
	in.gen++
	return in.gen, now.Sub(in.lastSave) >= saveEvery || in.notSaved
}

// logUndecided logs when the checks of s stop or start deciding, once per
// streak and with fixed codes only: a unit whose state cannot be read is
// unwatched, which must not go unnoticed (review 3c-1 F4). A transition is
// expected and not logged.
func (in *instance) logUndecided(s *service, was, now string) {
	switch {
	case now == was:
	case now != "" && now != undecidedTransition:
		in.log.Warn("supervisor cannot read a systemd unit: its checks decide nothing until it can",
			"service", s.def.Name, "reason", now)
	case was != "" && was != undecidedTransition:
		in.log.Info("supervisor reads the systemd unit again", "service", s.def.Name)
	}
}

// unitAttributes are the attributes of a systemd service's events: the
// unit and its last observed state. The caller holds in.mu.
func unitAttributes(s *service, attrs map[string]any) map[string]any {
	attrs["unit"] = s.def.Unit.Name.String()
	attrs["active_state"] = string(s.unit.last.Active)
	attrs["sub_state"] = s.unit.last.Sub
	return attrs
}
