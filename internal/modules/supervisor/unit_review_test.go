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
)

// Regression tests of the Codex review of 3c-2 (R1–R7).

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

// namedUnits is a unitReader that answers by unit name.
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
	c, err := New(Options{NewUnitReader: func() unitReader { return u },
		BootID: func() (string, error) { return testBoot, nil }}).Configure(mc)
	if err != nil {
		t.Fatal(err)
	}
	return c.(*instance)
}
