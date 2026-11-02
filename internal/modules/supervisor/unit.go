package supervisor

import (
	"context"
	"errors"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/platform/systemd"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/model"
)

// unitClient reads one unit's state and restarts a unit (systemd.Bus's
// errors). The variation it isolates is systemd's D-Bus (systemd.Bus)
// versus the fakes of the tests, which need no systemd, bus or root.
type unitClient interface {
	Unit(ctx context.Context, n systemd.UnitName) (systemd.UnitState, error)
	Restart(ctx context.Context, n systemd.UnitName) (job uint32, err error)
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
	undecidedAlias       = "unit_alias"          // another service observes the same unit under another name
)

// unitMemo is what the loop of a systemd service remembers between its
// observations in memory, beyond the policy's streak and the baseline
// (unitBase, persisted). A restart of sentineld forgets it: the next
// unsettled period starts its grace again, which only delays a failure.
type unitMemo struct {
	last systemd.UnitState // the last state read; zero before the first
	// unsettledSince is when the current unsettled period began; zero
	// while the unit is settled.
	unsettledSince time.Time
	// activeSince is the first success of the current run of successes;
	// zero after a failure (stable_after).
	activeSince time.Time
	// waitLogged: the log says that this service's restart waits for a
	// sibling's first read (alias_unresolved), once per wait (review 3c-2
	// P4).
	waitLogged bool
	// inflight is a read of an attempt that has not returned, kept here so
	// that the loop still waits for it after a panic unwound the attempt
	// (re-review R2); nil when none.
	inflight <-chan readResult
}

// unitBase is the baseline of restart detection: the invocation and
// restart count at the last active observation, and the boot they were
// read in ("" when the boot id could not be read). Invocation is "" until
// there is one, or after another unit appears behind the name. Neutral
// and failed observations never move it (review 3c-1 C3, F2): a unit seen
// failed and then active with a new invocation was started again, which
// is a restart too, so one that crashes and is restarted at every
// interval never counts a success. It is persisted, so a restart by
// systemd while sentineld was down is seen (D-095).
type unitBase struct {
	Boot       string
	Invocation string
	NRestarts  uint32
	// Own is the invocation sentineld's own restart replaced, saved with
	// the attempt and cleared at the next active observation: a new
	// invocation then is sentineld's restart, not systemd's, and the
	// restart count, which a manual start resets, holds only systemd's
	// own restarts since (review 3c-2 C7).
	Own string
}

// verdict is one observation's outcome: a failure, a reason to decide
// nothing (undecided), or neither, a success.
type verdict struct {
	failure   Failure
	undecided string
}

// judgeUnit decides what state st, read at now in boot (the running
// boot's id, "" when unknown), means for a unit with memo m, baseline b
// and transition_grace grace; restarts is how many restarts by systemd it
// saw since the baseline. It is a pure function: the clock, the state
// file and the events are the caller's.
func judgeUnit(m unitMemo, b unitBase, st systemd.UnitState, now time.Time, grace time.Duration,
	boot string) (v verdict, _ unitMemo, _ unitBase, restarts uint64) {
	if m.last.ID != "" && st.ID != m.last.ID {
		b = unitBase{} // another unit behind the name: start over, wholly (review 3c-2 R3)
	}
	m.last = st
	settledFailure := func(f Failure) (verdict, unitMemo, unitBase, uint64) {
		m.unsettledSince = time.Time{} // it ends any unsettled period and keeps the baseline
		return verdict{failure: f}, m, b, 0
	}
	switch st.Load {
	case systemd.LoadNotFound:
		return settledFailure(FailureUnitNotFound)
	case systemd.LoadMasked:
		return settledFailure(FailureUnitMasked)
	case systemd.LoadBadSetting, systemd.LoadError:
		return settledFailure(FailureUnitLoadFailed)
	}
	if unsettled(st) {
		if m.unsettledSince.IsZero() {
			m.unsettledSince = now
		}
		if now.Sub(m.unsettledSince) < grace {
			return verdict{undecided: undecidedTransition}, m, b, 0
		}
		return verdict{failure: FailureUnitStuck}, m, b, 0
	}
	m.unsettledSince = time.Time{}
	switch st.Active {
	case systemd.ActiveFailed:
		return settledFailure(FailureUnitFailed)
	case systemd.ActiveInactive:
		return settledFailure(FailureUnitInactive)
	}
	// Active: compare with the baseline, then move it here. Another boot
	// started every unit again: that is no restart of this one. An unknown
	// boot compares as the same one, the cautious side (review 3c-2 C11).
	rebooted := boot != "" && b.Boot != "" && boot != b.Boot
	switch {
	case rebooted:
	case b.Own != "" && st.InvocationID != b.Own:
		restarts = uint64(st.NRestarts) // ours reset the count: what it holds is systemd's
	case b.Invocation != "":
		if st.NRestarts > b.NRestarts {
			restarts = uint64(st.NRestarts - b.NRestarts)
		}
		if restarts == 0 && st.InvocationID != b.Invocation {
			restarts = 1 // started again with no count: a manual start resets NRestarts
		}
	}
	if boot == "" {
		boot = b.Boot
	}
	b = unitBase{Boot: boot, Invocation: st.InvocationID, NRestarts: st.NRestarts}
	if st.InvocationID == "" {
		b = unitBase{} // an active unit always has one: systemd's reply is odd, start over
	}
	if restarts > 0 {
		return verdict{failure: FailureUnitRestarted}, m, b, restarts
	}
	return verdict{}, m, b, 0
}

// unsettled reports whether st is on its way somewhere: a job queued, an
// automatic restart pending, or a transitional state; stub and merged
// are load states systemd passes through.
func unsettled(st systemd.UnitState) bool {
	switch {
	case st.Job != 0, st.Sub == "auto-restart", st.Load == systemd.LoadStub, st.Load == systemd.LoadMerged:
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
// never fails a unit (review 3c-1 C2). After each read, a loop armed by
// modules.supervisor.systemd_restart (fixed for its life, as the gate)
// and the service's recovery restarts the unit when its open incident
// asks for it (D-095); the next read then comes sooner when an attempt is
// due sooner. c attributes the first attempt to an operator (enable,
// reset). It ends with ctx, or at once when g holds the service back
// (disabled).
func (in *instance) runUnit(ctx context.Context, s *service, g gate, c cause) {
	if g != gateNone {
		in.setPhase(s, g.phase())
		return
	}
	in.mu.Lock()
	armed := in.systemdRestart && s.def.Unit.HasRecovery
	s.phase = ""
	in.mu.Unlock()
	s.decide()
	var stalled <-chan readResult
	var plan unitPlan
	for {
		stalled = in.iterateUnit(ctx, s, stalled)
		wait := s.def.CheckInterval
		if armed && stalled == nil && ctx.Err() == nil {
			wait, stalled = in.recoverUnit(ctx, s, &plan, wait, c)
		}
		c = cause{} // it applies to the first round only
		timer := in.rt.Clock.NewTimer(wait)
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
	var flap *model.Event
	baseChanged := false
	switch {
	case errors.Is(r.err, systemd.ErrChanging):
		v.undecided = undecidedChanging
	case errors.Is(r.err, systemd.ErrInvalid):
		v.undecided = undecidedInvalid
	case r.err != nil:
		v.undecided = undecidedUnavailable
	case in.aliasOfLocked(s, r.st.ID) != nil:
		// Two names of one unit: neither decides nor restarts until the
		// configuration is fixed (D-095, Q2). The Id is kept, so that the
		// other service sees the conflict too.
		v.undecided = undecidedAlias
		s.unit.last = r.st
	default:
		var b unitBase
		var n uint64
		own := s.st.UnitOwnRestart // judgeUnit clears it at an active read
		stored := s.st.base()
		if c := s.st.UnitCounter.ID; s.unit.last.ID == "" && c != "" && r.st.ID != "" && r.st.ID != c {
			// Another unit behind the name while sentineld was down: the
			// memo does not know, the counter does (review 3c-2 R21).
			s.st.setBase(unitBase{})
		}
		v, s.unit, b, n = judgeUnit(s.unit, s.st.base(), r.st, now, s.def.Unit.TransitionGrace, in.boot)
		rebased := b != stored // the invalidation above is saved too (re-review R3)
		s.st.setBase(b)
		var auto uint64
		prev := s.st.UnitCounter
		auto, s.st.UnitCounter = countRestarts(prev, r.st, in.boot, own)
		baseChanged = rebased || n > 0 || s.st.UnitCounter != prev
		flap = in.countSystemdRestartsLocked(s, auto, now)
	}
	s.lastDuration, s.lastSlotWait = took, waited
	was := s.undecided
	s.undecided = v.undecided
	var ev *model.Event
	var gen uint64
	var due bool
	if v.undecided != "" {
		gen, due = in.undecidedLocked(s, v.undecided, now)
		due = due || baseChanged // a count moved in a transition is saved before service_flapping (review 3c-2 R15)
	} else {
		s.phase = ""
		switch { // the run of successes that stable_after measures
		case v.failure != "":
			s.unit.activeSince = time.Time{}
		case s.unit.activeSince.IsZero():
			s.unit.activeSince = now
		}
		ev, gen, due = in.applyLocked(s, Result{Failure: v.failure}, now, v.failure != "" || s.stableAt(now))
		// A moved baseline is saved with the observation, so that a restart
		// of sentineld neither misses a restart nor counts one twice.
		due = due || baseChanged
	}
	in.mu.Unlock()
	in.logUndecided(s, was, v.undecided)
	in.settle(s, ev, gen, due)
	in.publishFlapping(s, flap)
}

// publishFlapping logs and publishes e, a service_flapping, if any; the
// state it follows is saved first.
func (in *instance) publishFlapping(s *service, e *model.Event) {
	if e == nil {
		return
	}
	in.log.Warn("supervisor: systemd keeps restarting the unit", "service", s.def.Name,
		"restarts", e.Attributes["restarts"], "window", s.def.Recovery.Window)
	in.publish(s, *e)
}

// aliasOfLocked returns another systemd service whose unit was last read
// with canonical name id, nil when none: two names of one unit (an alias,
// or a name and its Id). A service disabled reads nothing and conflicts
// with no one. The caller holds in.mu.
func (in *instance) aliasOfLocked(s *service, id string) *service {
	for _, o := range in.services {
		if o != s && o.def.Type == typeSystemd && !o.def.Disabled && !o.st.Disabled && id != "" && o.unit.last.ID == id {
			return o
		}
	}
	return nil
}

// unresolvedLocked reports whether another systemd service that is not
// disabled has not read its unit yet: it may be an alias of s's, which a
// restart must not race (review 3c-2 C4). The caller holds in.mu.
func (in *instance) unresolvedLocked(s *service) bool {
	for _, o := range in.services {
		// A name blocked until sentineld restarts never reads: it cannot
		// hold the others back (review 3c-2 R14).
		if o != s && o.def.Type == typeSystemd && !o.def.Disabled && !o.st.Disabled && o.unit.last.ID == "" &&
			in.retired[o.def.Name] == nil {
			return true
		}
	}
	return false
}

// stableAt reports whether s has run long enough at now to close its
// incident: a service sentineld restarts must also have run for
// stable_after since its current run of successes began (D-087, D-095
// Q6); one only observed closes it on the success threshold alone. The
// caller holds in.mu.
func (s *service) stableAt(now time.Time) bool {
	return !s.def.Unit.HasRecovery || now.Sub(s.unit.activeSince) >= s.def.Recovery.StableAfter
}

// countRestarts returns how many automatic restarts systemd made of a
// unit read in st, in boot ("" when unknown), since its count c was last
// read, and the count to keep. own is sentineld's restart in progress or
// done (unitBase.Own). It reads the count in any state, so a unit caught
// crashing in auto-restart at every check is counted too (review 3c-2
// R4). The count restarts from 0 with a start that is not automatic:
//   - another boot, or sentineld's own restart (a new invocation since
//     own), each applied once: all of the count is new;
//   - otherwise a count that went down (an administrator's start): what
//     it holds now came after.
//
// Another unit behind the name starts over without counting (R13). A
// unit that is not loaded has no count. It is a pure function.
func countRestarts(c unitCounter, st systemd.UnitState, boot, own string) (n uint64, _ unitCounter) {
	if st.Load != systemd.LoadLoaded || st.ID == "" {
		return 0, c
	}
	if boot == "" {
		boot = c.Boot // unknown: compare as the same boot (review 3c-2 C11)
	}
	switch {
	case c.ID != st.ID:
		return 0, unitCounter{Count: st.NRestarts, ID: st.ID, Boot: boot, Own: own}
	case c.Boot != "" && boot != c.Boot,
		own != "" && own != c.Own && st.InvocationID != own:
		n = uint64(st.NRestarts)
		c.Own = own
	case st.NRestarts >= c.Count:
		n = uint64(st.NRestarts - c.Count)
	default:
		n = uint64(st.NRestarts)
	}
	c.Count, c.Boot = st.NRestarts, boot
	return n, c
}

// countSystemdRestartsLocked counts n automatic restarts of s's unit by
// systemd seen at now, apart from sentineld's own attempts (D-095, Q4),
// and returns service_flapping when they reach the service's threshold:
// max_attempts within window of its recovery, or of the defaults for a
// service that is only observed. The caller holds in.mu and saves the
// state.
func (in *instance) countSystemdRestartsLocked(s *service, n uint64, now time.Time) *model.Event {
	st := &s.st
	if n == 0 && len(st.UnitRestartTimes) == 0 {
		return nil
	}
	st.UnitRestarts += n
	var due bool
	st.UnitRestartTimes, st.FlappingAt, due = flapping(st.UnitRestartTimes, st.FlappingAt, n, now, s.def.Recovery)
	if !due {
		return nil
	}
	e := flappingEvent(s, len(st.UnitRestartTimes))
	return &e
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
	case now == undecidedAlias:
		in.log.Error("supervisor: two services observe the same systemd unit under two names; neither decides nor restarts until one is removed",
			"service", s.def.Name, "unit", s.def.Unit.Name.String())
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
