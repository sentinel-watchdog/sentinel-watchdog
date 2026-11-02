package supervisor

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/audit"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/module"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/platform/systemd"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/api"
)

// Regression tests of the depth-L review panel of 3c-2
// (.plans/supervisor-3c/panel-3c2/findings.md).

// R17 = P1 = F2: an attempt blocked past max_delay while the checks
// decide nothing keeps the check interval: no busy loop.
func TestBlockedAttemptUndecidedPastMaxDelayKeepsInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fu := &fakeUnits{}
		newFakeSystemd(fu, dead)
		in := configureRestart(t, armedUnits, "    recovery: {delay: 1s, max_delay: 1m}\n", fu)
		r := start(t, in, "")
		defer r.stop(t)
		r.audit.setFail(func(e module.AuditEntry) bool {
			return e.Action == actionRestart && e.Result == audit.ResultIntent
		})
		synctest.Wait()
		tick(time.Second) // blocked by the audit log until max_delay
		var reads atomic.Int32
		hold := make(chan struct{})
		defer close(hold)
		fu.set(func(context.Context) (systemd.UnitState, error) {
			if reads.Add(1) > 100 {
				<-hold // park a spinning loop so the bubble can settle
			}
			return systemd.UnitState{}, systemd.ErrUnavailable
		})
		tick(2 * time.Minute)
		if n := reads.Load(); n > 20 {
			t.Fatalf("%d reads in 2 minutes: the loop spins once max_delay has passed", n)
		}
	})
}

// R18 = F1: reset joins the reads of the loop it ends.
func TestUnitResetPreservesOutstandingRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fu := &fakeUnits{}
		in := configureRestart(t, armedUnits, "    recovery: {}\n", fu)
		r := start(t, in, "")
		synctest.Wait()
		release := make(chan struct{})
		var reads atomic.Int32
		fu.set(func(context.Context) (systemd.UnitState, error) {
			if reads.Add(1) == 1 {
				<-release // ignores its cancellation
			}
			return runningUnit('a', 0), nil
		})
		tick(10 * time.Second)
		_, _ = actFor(in, actReset, "reset-stalled")
		synctest.Wait()
		if got := reads.Load(); got != 1 {
			t.Fatalf("%d reads before the outstanding read returned", got)
		}
		close(release)
		synctest.Wait()
		r.stop(t)
	})
}

// R19: the restart call itself ends by restart_timeout, also with a
// longer timeout.
func TestRestartSubmissionUsesRestartDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fu := &fakeUnits{}
		body := strings.Replace(restartService, "check_interval: 10s", "check_interval: 10s\n    timeout: 5m", 1)
		in := configureUnits(t, map[string]string{"sentinel.yaml": armedUnits,
			"supervisor/a.yaml": body + "    recovery: {restart_timeout: 1m}\n"}, fu)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		s := in.byName["web"]
		in.endLoop(context.Background(), s)
		var late bool
		fu.onRestart(func(ctx context.Context) (uint32, error) {
			if deadline, ok := ctx.Deadline(); !ok || deadline.After(time.Now().Add(time.Minute)) {
				late = true
			}
			return 0, systemd.ErrUnavailable
		})
		in.restartUnit(context.Background(), s, attempt{trigger: triggerOperator}, true)
		if late {
			t.Error("the restart call may outlive restart_timeout")
		}
	})
}

// R20 = P5: an unknown outcome found to have changed nothing forgets the
// mark of sentineld's restart.
func TestReconciledNoopClearsOwnRestartAttribution(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fu := &fakeUnits{}
		f := newFakeSystemd(fu, runningUnit('a', 10))
		in := configureRestart(t, armedUnits, "    recovery: {delay: 10m}\n", fu)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		f.set(unitAt(systemd.ActiveFailed, "failed", 'a', 10))
		tick(10 * time.Second)
		f.edit(func(f *fakeSystemd) { f.err, f.queue = systemd.ErrUnknownOutcome, false })
		_, _ = actFor(in, actRestart, "unknown-noop")
		tick(5 * time.Second)
		f.set(runningUnit('b', 11))
		tick(10 * time.Second)
		if got := statusOf(t, in, "web").UnitRestarts; got != 1 {
			t.Fatalf("%d automatic restarts, want 1", got)
		}
	})
}

// R21: another unit behind the name while sentineld was down is not
// compared with the old unit's baseline.
func TestCanonicalReplacementAcrossDaemonRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fu := &fakeUnits{}
		first := configureRestart(t, centralFile, "", fu)
		r := start(t, first, "")
		synctest.Wait()
		dir := r.stateDir
		r.stop(t)
		replacement := runningUnit('b', 0)
		replacement.ID = "replacement.service"
		fu.state(replacement, nil)
		second := configureRestart(t, centralFile, "", fu)
		r2 := start(t, second, dir)
		defer r2.stop(t)
		synctest.Wait()
		if got := statusOf(t, second, "web"); got.State != "healthy" || got.ConsecutiveFailures != 0 {
			t.Fatalf("replacement inherited another unit's baseline: %+v", got)
		}
	})
}

// P2: an operator restart that overran its bound retires the service only
// when a read is really outstanding.
func TestOperatorRestartPastBoundDoesNotRetire(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		var hog atomic.Bool
		in := configureUnitsNamed(t, []string{"a", "web"}, nil, func(n systemd.UnitName) (systemd.UnitState, error) {
			if n.Prefix == "a" && hog.Load() {
				<-release // holds the only slot, ignoring its deadline
			}
			st := runningUnit(n.Prefix[0], 0)
			st.ID = n.String()
			return st, nil
		}, func(systemd.UnitName) (uint32, error) { return 9, nil })
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		hog.Store(true)
		tick(10 * time.Second)
		go func() { _, _ = actFor(in, actRestart, "past-bound") }()
		tick(4 * time.Minute)
		close(release)
		tick(10 * time.Second)
		if st := statusOf(t, in, "web"); st.BlockedReason == blockedOperator {
			t.Fatalf("web retired with no read outstanding: %+v", st)
		}
	})
}

// P3: an operator's restarts of a unit only observed use no budget.
func TestOperatorRestartObservedUnitHasNoBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fu := &fakeUnits{}
		f := newFakeSystemd(fu, dead)
		f.edit(func(f *fakeSystemd) { f.then = dead })
		in := configureRestart(t, armedUnits, "", fu)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		for i := range 7 {
			_, apiErr := actFor(in, actRestart, "again")
			if apiErr != nil && strings.Contains(apiErr.Message, "budget") {
				t.Fatalf("restart %d: %v", i+1, apiErr)
			}
			tick(5 * time.Second)
		}
		if s := statusOf(t, in, "web"); s.Attempts != 0 {
			t.Errorf("status %+v", s)
		}
	})
}

// P4: a sibling whose reads always fail does not hold restarts back: it
// cannot restart its unit either.
func TestUnreadableSiblingDoesNotBlockRestarts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var restarts atomic.Int32
		in := configureUnitsNamed(t, []string{"a", "web"}, nil, func(n systemd.UnitName) (systemd.UnitState, error) {
			if n.Prefix == "a" {
				return systemd.UnitState{}, systemd.ErrUnavailable
			}
			st := dead
			st.ID = n.String()
			return st, nil
		}, func(systemd.UnitName) (uint32, error) { restarts.Add(1); return 9, nil })
		r := start(t, in, "")
		defer r.stop(t)
		for range 3 {
			tick(10 * time.Second)
		}
		if restarts.Load() == 0 {
			t.Fatalf("status of web %+v: held back by a sibling that cannot read", statusOf(t, in, "web"))
		}
	})
}

// F3: automatic restarts seen only by the fresh read before sentineld's
// restart are counted, not reset away.
func TestPreflightCounterCounted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fu := &fakeUnits{}
		f := newFakeSystemd(fu, runningUnit('a', 0))
		f.edit(func(f *fakeSystemd) { f.then = runningUnit('c', 0) })
		in := configureRestart(t, armedUnits, "", fu)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		f.set(runningUnit('b', 5)) // five automatic restarts since the last check
		_, _ = actFor(in, actRestart, "preflight")
		tick(5 * time.Second)
		tick(10 * time.Second)
		if got := statusOf(t, in, "web").UnitRestarts; got != 5 {
			t.Fatalf("%d automatic restarts, want 5", got)
		}
	})
}

// F4: sentineld's restart starts stable_after again.
func TestOperatorRestartRestartsStableAfter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fu := &fakeUnits{}
		f := newFakeSystemd(fu, dead)
		in := configureRestart(t, armedUnits, "    recovery: {delay: 10m, stable_after: 1m}\n", fu)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait() // failed: an incident
		f.set(runningUnit('b', 0))
		for range 5 {
			tick(10 * time.Second) // 50 s of the minute
		}
		_, _ = actFor(in, actRestart, "mid-stable")
		tick(5 * time.Second)
		tick(10 * time.Second)
		tick(10 * time.Second)
		if s := statusOf(t, in, "web"); s.State != "failed" {
			t.Fatalf("recovered %s after the restart, before stable_after: %+v", "25s", s)
		}
	})
}

var _ = api.CodeConflict
