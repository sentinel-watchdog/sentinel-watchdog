package supervisor

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/audit"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/module"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/platform/systemd"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/api"
)

// Regression tests of the restarts of systemd units: one per finding of
// the reviews of 3c-2 (.plans/supervisor-3c/findings-3c2.md and
// panel-3c2/findings.md), each shown to fail before its fix.

// Codex review of 3c-2 (R1–R7).

// R1: an operator's restart never reads beside a read of the loop that
// has not returned.
func TestOperatorRestartPreservesOutstandingRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fu := &fakeUnits{}
		fu.onRestart(func(context.Context) (uint32, error) { return 0, systemd.ErrUnavailable })
		in := configureRestart(t, armedUnits, "", fu)
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
		_, _ = actFor(in, actRestart, "stalled")
		synctest.Wait()
		if got := reads.Load(); got != 1 {
			t.Fatalf("%d reads before the outstanding read returned", got)
		}
		close(release)
		synctest.Wait()
		r.stop(t)
	})
}

// R2: restart_timeout bounds the wait for a slot too.
func TestRestartDeadlineBoundsSlotWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fu := &fakeUnits{}
		in := configureRestart(t, armedUnits, "    recovery: {restart_timeout: 1m}\n", fu)
		r := start(t, in, "")
		synctest.Wait()
		s := in.byName["web"]
		in.endLoop(context.Background(), s)
		synctest.Wait()
		fu.state(dead, nil)
		var hogs []struct{}
		fu.onRestart(func(context.Context) (uint32, error) {
			for range cap(in.slots) { // stalled checks of other services own every slot
				in.slots <- struct{}{}
				hogs = append(hogs, struct{}{})
			}
			return 9, nil
		})
		done := make(chan restartOutcome, 1)
		go func() {
			out, _ := in.restartUnit(context.Background(), s, attempt{trigger: triggerOperator}, true)
			done <- out
		}()
		synctest.Wait()
		tick(61 * time.Second)
		select {
		case out := <-done:
			if out.done || !strings.Contains(out.why, "restart_timeout") {
				t.Errorf("outcome %+v", out)
			}
		default:
			t.Fatal("still waiting after restart_timeout")
		}
		for range hogs {
			<-in.slots
		}
		r.stop(t)
	})
}

// R3: another unit behind the name resets the whole baseline, so the
// state file stays valid.
func TestCanonicalIDChangeKeepsValidState(t *testing.T) {
	old := runningUnit('a', 3)
	old.ID = "old.service"
	replacement := dead
	replacement.ID = "new.service"
	base := unitBase{Boot: testBoot, Invocation: strings.Repeat("a", 32), NRestarts: 3}
	_, _, after, _ := judgeUnit(unitMemo{last: old}, base, replacement, time.Now(), time.Minute, testBoot)
	st := serviceState{Digest: "sha256:" + strings.Repeat("0", 64), Health: HealthHealthy}
	st.setBase(after)
	f := fileState{Services: map[string]serviceState{"web": st}}
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
}

// R4: a crash loop caught in auto-restart at every check is counted and
// reported: the maintainer's goal for service_flapping.
func TestFlappingCountsRestartsObservedInTransition(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fu := &fakeUnits{}
		in := configureUnits(t, map[string]string{"supervisor/a.yaml": unitService +
			"    recovery: {max_attempts: 3, window: 2m}\n"}, fu)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		r.drain()
		for n := uint32(1); n <= 3; n++ {
			fu.state(unitAt(systemd.ActiveActivating, "auto-restart", 'a'+byte(n), n), nil)
			tick(10 * time.Second)
		}
		flaps := 0
		for _, e := range r.drain() {
			if e.Type == EventServiceFlapping {
				flaps++
			}
		}
		if flaps != 1 {
			t.Fatalf("%d flapping events, want 1", flaps)
		}
		if s := statusOf(t, in, "web"); s.UnitRestarts != 3 {
			t.Errorf("status %+v", s)
		}
	})
}

// R5: a refused restart leaves no mark that would take systemd's next
// restart for sentineld's, or count its history again.
func TestRefusedRestartPreservesSystemdCounterDelta(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fu := &fakeUnits{}
		f := newFakeSystemd(fu, runningUnit('a', 10))
		f.edit(func(f *fakeSystemd) { f.err = &systemd.RefusedError{Code: systemd.RefusedConflict} })
		in := configureRestart(t, armedUnits, "    recovery: {delay: 10m}\n", fu)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		f.set(unitAt(systemd.ActiveFailed, "failed", 'a', 10))
		tick(10 * time.Second)
		_, _ = actFor(in, actRestart, "refused")
		synctest.Wait()
		if st := readFileState(t, r.stateDir).Services["web"]; st.UnitOwnRestart != "" {
			t.Errorf("the mark of a refused restart was kept: %+v", st)
		}
		f.set(runningUnit('b', 11))
		tick(10 * time.Second)
		if s := statusOf(t, in, "web"); s.UnitRestarts != 1 || s.LastFailure != string(FailureUnitRestarted) {
			t.Fatalf("status %+v, want 1 restart by systemd, seen as one", s)
		}
	})
}

// R6: an attempt the audit log refused waits max_delay before it is
// tried again, whatever the observations in between.
func TestFailedRestartAuditHonorsMaxDelay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fu := &fakeUnits{}
		newFakeSystemd(fu, dead)
		in := configureRestart(t, armedUnits, "    recovery: {delay: 1s, max_delay: 1m}\n", fu)
		r := start(t, in, "")
		defer r.stop(t)
		var intents atomic.Int32
		r.audit.setFail(func(e module.AuditEntry) bool {
			if e.Action == actionRestart && e.Result == audit.ResultIntent {
				intents.Add(1)
				return true
			}
			return false
		})
		synctest.Wait()
		tick(time.Second)
		for range 5 {
			tick(10 * time.Second)
		}
		if got := intents.Load(); got != 1 {
			t.Fatalf("%d intents before max_delay elapsed", got)
		}
		if s := statusOf(t, in, "web"); s.Phase != phaseBlocked || s.BlockedReason != blockedAudit {
			t.Errorf("status %+v", s)
		}
		tick(10 * time.Second)
		if got := intents.Load(); got != 2 {
			t.Fatalf("%d intents after max_delay, want 2", got)
		}
	})
}

// R7: the reads of attempts keep max_concurrent_checks: three units
// restarted at once, each polling a job that never ends, never read two
// at a time with a limit of 1 (review 3c-2 C9).
func TestUnitRestartReadsTakeSlots(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		var inFlight, most, polls int
		jobs := map[string]bool{}
		fu := &fakeUnits{}
		names := []string{"a", "b", "c"}
		in := configureUnitsNamed(t, names, fu, func(n systemd.UnitName) (systemd.UnitState, error) {
			mu.Lock()
			inFlight++
			most = max(most, inFlight)
			job := jobs[n.String()]
			if job {
				polls++
			}
			mu.Unlock()
			time.Sleep(200 * time.Millisecond)
			mu.Lock()
			inFlight--
			mu.Unlock()
			st := dead
			st.ID = n.String()
			if job {
				st.Job = 9
			}
			return st, nil
		}, func(n systemd.UnitName) (uint32, error) {
			mu.Lock()
			jobs[n.String()] = true
			mu.Unlock()
			return 9, nil
		})
		r := start(t, in, "")
		defer r.stop(t)
		for range 6 {
			tick(10 * time.Second)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(jobs) != 3 || polls < 6 {
			t.Fatalf("restarts %v, %d polls: the test exercised no restart", jobs, polls)
		}
		if most != 1 {
			t.Errorf("%d reads at once with max_concurrent_checks 1", most)
		}
	})
}

// namedUnits is a unitClient that answers by unit name.
type namedUnits struct {
	read    func(systemd.UnitName) (systemd.UnitState, error)
	restart func(systemd.UnitName) (uint32, error)
}

func (u namedUnits) Unit(_ context.Context, n systemd.UnitName) (systemd.UnitState, error) {
	return u.read(n)
}

func (u namedUnits) Restart(_ context.Context, n systemd.UnitName) (uint32, error) {
	return u.restart(n)
}

func (namedUnits) Close() error { return nil }

// configureUnitsNamed configures armed systemd services names, with
// recovery and a limit of one check at a time, read by u.
func configureUnitsNamed(t *testing.T, names []string, _ *fakeUnits, read func(systemd.UnitName) (systemd.UnitState, error),
	restart func(systemd.UnitName) (uint32, error)) *instance {
	t.Helper()
	body := "version: 1\nsettings: {max_concurrent_checks: 1}\nservices:\n"
	for _, n := range names {
		body += "  - name: " + n + "\n    unit: " + n + ".service\n    check_interval: 10s\n" +
			"    failure_policy: {consecutive_failures: 1}\n    recovery: {delay: 1s}\n"
	}
	mc, err := moduleConfig(t, map[string]string{"sentinel.yaml": armedUnits, "supervisor/a.yaml": body}, nil)
	if err != nil {
		t.Fatal(err)
	}
	u := namedUnits{read: read, restart: restart}
	c, err := New(Options{NewUnitClient: func() unitClient { return u },
		BootID: func() (string, error) { return testBoot, nil }}).Configure(mc)
	if err != nil {
		t.Fatal(err)
	}
	return c.(*instance)
}

// Regression tests of the Codex re-review of the 3c-2 fixes (R6 again,
// R11–R16).

// R6: the max_delay of a blocked attempt also holds through checks that
// decide nothing.
func TestFailedRestartAuditHonorsMaxDelayThroughUndecided(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fu := &fakeUnits{}
		newFakeSystemd(fu, dead)
		in := configureRestart(t, armedUnits, "    recovery: {delay: 1s, max_delay: 1m}\n", fu)
		r := start(t, in, "")
		defer r.stop(t)
		var intents atomic.Int32
		r.audit.setFail(func(e module.AuditEntry) bool {
			if e.Action == actionRestart && e.Result == audit.ResultIntent {
				intents.Add(1)
				return true
			}
			return false
		})
		synctest.Wait()
		tick(time.Second)
		fu.state(systemd.UnitState{}, systemd.ErrUnavailable)
		tick(10 * time.Second)   // undecided
		newFakeSystemd(fu, dead) // readable and failed again
		tick(10 * time.Second)
		tick(time.Second)
		if got := intents.Load(); got != 1 {
			t.Fatalf("%d intents before max_delay elapsed", got)
		}
		// The attempt survived: it is tried again once max_delay has
		// passed (lens-tests T2).
		for range 5 {
			tick(10 * time.Second)
		}
		if got := intents.Load(); got != 2 {
			t.Fatalf("%d intents after max_delay, want 2", got)
		}
	})
}

// R11: automatic restarts after sentineld's own restart are all counted,
// whatever the count was before.
func TestAutomaticRestartsAfterOwnCounterReset(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fu := &fakeUnits{}
		f := newFakeSystemd(fu, runningUnit('a', 4))
		f.edit(func(f *fakeSystemd) { f.then = runningUnit('b', 6) })
		in := configureRestart(t, armedUnits, "", fu)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		_, _ = actFor(in, actRestart, "counter-reset")
		tick(5 * time.Second)
		tick(10 * time.Second)
		if got := statusOf(t, in, "web").UnitRestarts; got != 6 {
			t.Fatalf("%d automatic restarts, want 6", got)
		}
	})
}

// R12: starts of sentineld in a new boot count its restarts once, even
// while the unit is never active.
func TestCounterBootAdvancesWithoutActiveObservation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fu := &fakeUnits{}
		files := map[string]string{"supervisor/a.yaml": unitService}
		first := start(t, configureUnits(t, files, fu), "")
		synctest.Wait()
		dir := first.stateDir
		first.stop(t)
		const bootB = "11111111-2222-3333-4444-555555555555"
		fu.state(unitAt(systemd.ActiveFailed, "failed", 'b', 2), nil)
		for i := range 2 {
			in := configureUnits(t, files, fu)
			in.bootID = func() (string, error) { return bootB, nil }
			r := start(t, in, dir)
			synctest.Wait()
			got := statusOf(t, in, "web").UnitRestarts
			r.stop(t)
			if got != 2 {
				t.Fatalf("start %d: %d restarts, want 2", i, got)
			}
		}
	})
}

// R13: another unit behind the name starts its count over.
func TestCounterDoesNotCrossCanonicalID(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fu := &fakeUnits{}
		in := configureUnits(t, map[string]string{"supervisor/a.yaml": unitService}, fu)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		replacement := runningUnit('b', 50)
		replacement.ID = "replacement.service"
		fu.state(replacement, nil)
		tick(10 * time.Second)
		if got := statusOf(t, in, "web").UnitRestarts; got != 0 {
			t.Fatalf("%d restarts attributed across canonical IDs", got)
		}
	})
}

// R14: a name blocked by a stuck action keeps its unit's Id and never
// holds the restarts of other services back.
func TestStuckUnitKeepsIdentity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := configureUnitsNamed(t, []string{"a", "b"}, nil, func(n systemd.UnitName) (systemd.UnitState, error) {
			st := runningUnit('a', 0)
			st.ID = n.String()
			return st, nil
		}, func(systemd.UnitName) (uint32, error) { return 9, nil })
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		in.endLoop(context.Background(), in.byName["a"])
		in.stuck(in.byName["a"], actRestart, "")
		in.mu.Lock()
		id := in.byName["a"].unit.last.ID
		unresolved := in.unresolvedLocked(in.byName["b"])
		in.mu.Unlock()
		if id != "a.service" || unresolved {
			t.Errorf("blocked a: Id %q, b held back %v", id, unresolved)
		}
	})
}

// R15: a count moved in a transition is saved before service_flapping is
// published.
func TestFlappingSavedBeforePublished(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fu := &fakeUnits{}
		in := configureUnits(t, map[string]string{"supervisor/a.yaml": unitService +
			"    recovery: {max_attempts: 3, window: 2m}\n"}, fu)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		for n := uint32(1); n <= 3; n++ {
			fu.state(unitAt(systemd.ActiveActivating, "auto-restart", 'a'+byte(n), n), nil)
			tick(10 * time.Second)
		}
		saved := readFileState(t, r.stateDir).Services["web"]
		if saved.UnitCounter.Count != 3 || saved.FlappingAt.IsZero() {
			t.Fatalf("flapping published before persistence: %+v", saved)
		}
	})
}

// R16: a cancellation during a poll is a cancellation, not a timeout.
func TestCancellationDuringPollIsCanceled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fu := &fakeUnits{}
		fu.state(dead, nil)
		entered := make(chan struct{})
		fu.onRestart(func(context.Context) (uint32, error) {
			fu.set(func(ctx context.Context) (systemd.UnitState, error) {
				select {
				case <-entered:
				default:
					close(entered)
				}
				<-ctx.Done()
				return systemd.UnitState{}, ctx.Err()
			})
			return 9, nil
		})
		in := configureRestart(t, armedUnits, "", fu)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		in.endLoop(context.Background(), in.byName["web"])
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan struct{})
		go func() {
			in.restartUnit(ctx, in.byName["web"], attempt{trigger: triggerOperator}, true)
			close(done)
		}()
		<-entered
		cancel()
		<-done
		if got := strings.Join(restartRecords(r), ","); !strings.Contains(got, "failed/canceled") {
			t.Fatalf("audit %s, want a cancellation", got)
		}
	})
}

// The depth-L review panel of 3c-2.

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

// Tests asked for by the tests lens of the panel (T1, T3, T4, T5) and the
// recover added to recoverUnit.

// T1: an operator's restart of an alias is refused and reaches nothing.
func TestOperatorRestartOfAliasRefused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		au := &aliasUnits{}
		in := configureAlias(t, aliasServices, au)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		tick(10 * time.Second)
		_, apiErr := in.act(context.Background(), "a", actRestart)
		if apiErr == nil || apiErr.Code != api.CodeConflict || !strings.Contains(apiErr.Message, "same unit") {
			t.Fatalf("error %+v", apiErr)
		}
		tick(5 * time.Second)
		if n := au.count(); n != 0 {
			t.Errorf("%d restarts of an alias", n)
		}
	})
}

// T3: service_flapping also for a unit only observed, with the defaults
// (5 restarts in 10 minutes).
func TestFlappingOfObservedUnitUsesDefaults(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fu := &fakeUnits{}
		in := configureUnits(t, map[string]string{"supervisor/a.yaml": unitService}, fu)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		r.drain()
		for n := uint32(1); n <= 5; n++ {
			fu.state(runningUnit('a'+byte(n), n), nil)
			tick(10 * time.Second)
		}
		var flaps int
		for _, e := range r.drain() {
			if e.Type == EventServiceFlapping {
				flaps++
				if e.Attributes["window_seconds"] != int64(600) || e.Attributes["restarts"] != 5 {
					t.Errorf("event %+v", e)
				}
			}
		}
		if flaps != 1 {
			t.Fatalf("%d service_flapping, want 1", flaps)
		}
	})
}

// T4: a unit recovered by someone else while exhausted with cooldown 0
// ends the exhaustion, but the window's attempts still count: a new
// failure exhausts at once, with no restart beyond the budget.
func TestExternalRecoveryKeepsWindowAttempts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fu := &fakeUnits{}
		f := newFakeSystemd(fu, dead)
		f.edit(func(f *fakeSystemd) { f.then = dead })
		in := configureRestart(t, armedUnits, "    recovery: {max_attempts: 2, delay: 1s, backoff: fixed, cooldown: \"0s\", window: 1h}\n", fu)
		r := start(t, in, "")
		for range 6 {
			tick(10 * time.Second)
		}
		if s := statusOf(t, in, "web"); s.Phase != phaseExhausted || fu.restartCount() != 2 {
			t.Fatalf("status %+v, %d restarts", s, fu.restartCount())
		}
		f.set(runningUnit('z', 0)) // back, by someone else
		for range 8 {
			tick(10 * time.Second)
		}
		r.stop(t)
		if st := readFileState(t, r.stateDir).Services["web"]; !st.ExhaustedAt.IsZero() || len(st.Attempts) != 2 {
			t.Fatalf("saved %+v", st)
		}
		in2 := configureRestart(t, armedUnits, "    recovery: {max_attempts: 2, delay: 1s, backoff: fixed, cooldown: \"0s\", window: 1h}\n", fu)
		r2 := start(t, in2, r.stateDir)
		defer r2.stop(t)
		f.set(dead)
		for range 4 {
			tick(10 * time.Second)
		}
		if s := statusOf(t, in2, "web"); s.Phase != phaseExhausted || fu.restartCount() != 2 {
			t.Fatalf("a new failure got restarts beyond the window's budget: %+v, %d restarts", s, fu.restartCount())
		}
	})
}

// T5: a name blocked by a stuck action does not hold the restart of
// another service back, observed through the restart itself.
func TestStuckUnitDoesNotBlockOthers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var bDead atomic.Bool
		var restarts atomic.Int32
		in := configureUnitsNamed(t, []string{"a", "b"}, nil, func(n systemd.UnitName) (systemd.UnitState, error) {
			st := runningUnit('a', 0)
			if n.Prefix == "b" && bDead.Load() {
				st = dead
			}
			st.ID = n.String()
			return st, nil
		}, func(systemd.UnitName) (uint32, error) { restarts.Add(1); return 9, nil })
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		in.endLoop(context.Background(), in.byName["a"])
		in.stuck(in.byName["a"], actRestart, "")
		bDead.Store(true)
		for range 3 {
			tick(10 * time.Second)
		}
		if restarts.Load() == 0 {
			t.Fatalf("b was not restarted: %+v", statusOf(t, in, "b"))
		}
	})
}

// A panic in the restart step is the module's own: it is logged and the
// loop goes on reading.
func TestRecoverUnitPanicKeepsLoop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fu := &fakeUnits{}
		newFakeSystemd(fu, dead)
		fu.onRestart(func(context.Context) (uint32, error) { panic("boom") })
		in := configureRestart(t, armedUnits, "    recovery: {delay: 1s}\n", fu)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		tick(time.Second)
		before := fu.count()
		tick(10 * time.Second)
		tick(10 * time.Second)
		if fu.count() <= before {
			t.Fatal("the loop stopped after a panic")
		}
		if logs := r.logs.String(); !strings.Contains(logs, "recovery panicked") || strings.Contains(logs, "boom") {
			t.Errorf("logs: %s", logs)
		}
	})
}
