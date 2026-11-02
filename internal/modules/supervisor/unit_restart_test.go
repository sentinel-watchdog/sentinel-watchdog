package supervisor

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/audit"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/platform/systemd"
)

// fakeSystemd is a unit and the systemd that restarts it: Restart queues
// job 9 (or answers err), which ends after polls reads of the unit,
// leaving it as then says.
type fakeSystemd struct {
	mu    sync.Mutex
	st    systemd.UnitState
	left  int // reads before the queued job ends; 0: no job
	then  systemd.UnitState
	polls int
	err   error
	// queue: an unknown outcome still queues the job.
	queue bool
}

func newFakeSystemd(fu *fakeUnits, st systemd.UnitState) *fakeSystemd {
	f := &fakeSystemd{st: st, polls: 2, then: runningUnit('r', 0)}
	fu.set(func(context.Context) (systemd.UnitState, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.left > 0 {
			if f.left--; f.left == 0 {
				f.st = f.then
			}
		}
		st := f.st
		if f.left > 0 {
			st.Job = 9
		}
		return st, nil
	})
	fu.onRestart(func(context.Context) (uint32, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.err == nil || f.queue {
			f.left = f.polls
		}
		if f.err != nil {
			return 0, f.err
		}
		return 9, nil
	})
	return f
}

func (f *fakeSystemd) set(st systemd.UnitState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.st, f.left = st, 0
}

func (f *fakeSystemd) edit(edit func(*fakeSystemd)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	edit(f)
}

// armedUnits is the central file with systemd restarts armed.
const armedUnits = centralFile + "    systemd_restart: true\n"

// restartService fails at the first failed check, with recovery.
const restartService = "version: 1\nservices:\n  - name: web\n    unit: nginx.service\n    check_interval: 10s\n" +
	"    failure_policy: {consecutive_failures: 1, consecutive_successes: 1}\n"

func configureRestart(t *testing.T, central, recovery string, fu *fakeUnits) *instance {
	t.Helper()
	return configureUnits(t, map[string]string{"sentinel.yaml": central,
		"supervisor/a.yaml": restartService + recovery}, fu)
}

// restartRecords returns the audit records of restarts, as
// "result/failure".
func restartRecords(r *rig) []string {
	var out []string
	for _, e := range r.audit.recorded() {
		if e.Action == actionRestart {
			out = append(out, string(e.Result)+"/"+e.Detail["failure"])
		}
	}
	return out
}

// Restarts are asked twice: the central switch and the service's
// recovery (D-095, Q1); without either, a failed unit is only observed.
func TestUnitRestartNeedsBothSwitches(t *testing.T) {
	tests := []struct {
		name, central, recovery string
		notArmed                bool
	}{
		{"switch off", centralFile, "    recovery: {}\n", true},
		{"no recovery", armedUnits, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				fu := &fakeUnits{}
				newFakeSystemd(fu, dead)
				in := configureRestart(t, tt.central, tt.recovery, fu)
				r := start(t, in, "")
				defer r.stop(t)
				for range 10 {
					tick(10 * time.Second)
				}
				if n := fu.restartCount(); n != 0 {
					t.Fatalf("%d restarts", n)
				}
				if s := statusOf(t, in, "web"); s.State != "failed" || (s.BlockedReason == phaseNotArmed) != tt.notArmed {
					t.Errorf("status %+v", s)
				}
			})
		})
	}
}

// An incident restarts the unit after the attempt's delay, in mode fail;
// the job is read until it ends; the new invocation is sentineld's, not a
// restart by systemd; the incident closes after stable_after (D-095).
func TestUnitRestarted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fu := &fakeUnits{}
		f := newFakeSystemd(fu, runningUnit('a', 0))
		in := configureRestart(t, armedUnits, "    recovery: {delay: 5s, stable_after: 1m}\n", fu)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		f.set(dead)
		tick(10 * time.Second) // failed: incident; the attempt waits its delay
		if n := fu.restartCount(); n != 0 {
			t.Fatalf("restarted before the delay: %d", n)
		}
		if s := statusOf(t, in, "web"); s.Phase != phaseRestarting || s.NextAttemptAt.IsZero() {
			t.Fatalf("status %+v", s)
		}
		tick(5 * time.Second) // the delay passes: a fresh read, then the restart
		tick(3 * time.Second) // two polls with the job, then it ends
		if n := fu.restartCount(); n != 1 {
			t.Fatalf("%d restarts, want 1", n)
		}
		if got := restartRecords(r); strings.Join(got, ",") != "intent/,ok/" {
			t.Fatalf("audit %v", got)
		}
		for _, e := range r.audit.recorded() {
			if e.Action == actionRestart && (e.Detail["unit"] != "nginx.service" || e.Detail["attempt"] != "1" ||
				e.Detail["trigger"] != triggerFailure || e.CorrelationID == "") {
				t.Errorf("record %+v", e)
			}
		}
		evs := r.drain()
		var restarted bool
		for _, e := range evs {
			if e.Type == EventServiceRestarted && e.Attributes["unit"] == "nginx.service" && e.Attributes["attempt"] == 1 {
				restarted = true
			}
		}
		if !restarted {
			t.Fatalf("no service_restarted: %+v", evs)
		}
		// Within stable_after the incident stays open, although the checks
		// succeed; the new invocation is not counted as systemd's.
		for range 4 {
			tick(10 * time.Second)
		}
		s := statusOf(t, in, "web")
		if s.State != "failed" || s.UnitRestarts != 0 || s.Starts != 1 || s.Restarts != 1 || s.Attempts != 1 {
			t.Fatalf("status within stable_after %+v", s)
		}
		for range 3 {
			tick(10 * time.Second)
		}
		if s := statusOf(t, in, "web"); s.State != "healthy" {
			t.Fatalf("not recovered after stable_after: %+v", s)
		}
		if n := fu.restartCount(); n != 1 {
			t.Errorf("%d restarts, want 1", n)
		}
	})
}

// How an attempt ends when systemd does not restart the unit (review
// 3c-2 C8): the attempt is spent, the outcome audited with a code.
func TestUnitRestartOutcomes(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(f *fakeSystemd)
		want    string // the outcome's record
		running bool   // the unit runs again afterwards
	}{
		{"job failed", func(f *fakeSystemd) { f.then = unitAt(systemd.ActiveFailed, "failed", 'r', 0) }, "failed/job_failed", false},
		{"job timeout", func(f *fakeSystemd) { f.polls = 1000 }, "failed/job_timeout", false},
		{"refused", func(f *fakeSystemd) { f.err = &systemd.RefusedError{Code: systemd.RefusedConflict} }, "failed/conflict", false},
		{"systemd unreachable", func(f *fakeSystemd) { f.err = fmt.Errorf("%w: dial", systemd.ErrUnavailable) }, "failed/systemd_unavailable", false},
		{"unknown, then a job: waited for", func(f *fakeSystemd) { f.err, f.queue = systemd.ErrUnknownOutcome, true }, "ok/", true},
		{"unknown, no job, same invocation", func(f *fakeSystemd) { f.err = systemd.ErrUnknownOutcome }, "failed/outcome_unknown", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				fu := &fakeUnits{}
				f := newFakeSystemd(fu, dead)
				f.edit(tt.setup)
				in := configureRestart(t, armedUnits, "    recovery: {delay: 1s, restart_timeout: 1m, max_attempts: 1}\n", fu)
				r := start(t, in, "")
				defer r.stop(t)
				synctest.Wait()
				tick(time.Second)
				for range 7 {
					tick(10 * time.Second)
				}
				if got := restartRecords(r); len(got) < 2 || got[1] != tt.want {
					t.Fatalf("audit %v, want intent then %s", got, tt.want)
				}
				// One attempt only: max_attempts 1, and never a second call
				// within an attempt (an unknown outcome is read, not asked
				// again).
				if n := fu.restartCount(); n != 1 {
					t.Errorf("%d restarts, want 1", n)
				}
				if s := statusOf(t, in, "web"); s.Attempts != 1 || (s.ActiveState == "active") != tt.running {
					t.Errorf("status %+v", s)
				}
			})
		})
	}
}

// A unit that comes back during the delay, or that has a job queued when
// the attempt is due, is not restarted, and no attempt is spent.
func TestUnitRestartNotNeeded(t *testing.T) {
	tests := []struct {
		name string
		then systemd.UnitState
	}{
		{"back by itself", runningUnit('b', 0)},
		{"a job queued", func() systemd.UnitState { st := dead; st.Job = 4; return st }()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				fu := &fakeUnits{}
				f := newFakeSystemd(fu, dead)
				in := configureRestart(t, armedUnits, "    recovery: {delay: 30s}\n", fu)
				r := start(t, in, "")
				defer r.stop(t)
				synctest.Wait()
				tick(10 * time.Second)
				f.set(tt.then)
				for range 5 {
					tick(10 * time.Second)
				}
				if n := fu.restartCount(); n != 0 {
					t.Fatalf("%d restarts", n)
				}
				if s := statusOf(t, in, "web"); s.Attempts != 0 {
					t.Errorf("an attempt was spent: %+v", s)
				}
				if got := restartRecords(r); len(got) != 0 {
					t.Errorf("audit %v", got)
				}
			})
		})
	}
}

// Attempts that fail exhaust the budget: service_recovery_exhausted, no
// more restarts until the cooldown (D-013).
func TestUnitRestartExhausted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fu := &fakeUnits{}
		f := newFakeSystemd(fu, dead)
		f.edit(func(f *fakeSystemd) { f.then = dead })
		in := configureRestart(t, armedUnits, "    recovery: {max_attempts: 2, delay: 1s, backoff: fixed, cooldown: 1h}\n", fu)
		r := start(t, in, "")
		defer r.stop(t)
		for range 20 {
			tick(10 * time.Second)
		}
		if n := fu.restartCount(); n != 2 {
			t.Fatalf("%d restarts, want 2", n)
		}
		var exhausted int
		for _, e := range r.drain() {
			if e.Type == EventServiceRecoveryExhausted {
				exhausted++
				if e.Attributes["unit"] != "nginx.service" {
					t.Errorf("event %+v", e)
				}
			}
		}
		if exhausted != 1 {
			t.Errorf("%d exhausted events", exhausted)
		}
		if s := statusOf(t, in, "web"); s.Phase != phaseExhausted || s.ResumeAt.IsZero() {
			t.Errorf("status %+v", s)
		}
		// A unit brought back by someone else ends the exhaustion (C2).
		f.set(runningUnit('z', 0))
		for range 8 {
			tick(10 * time.Second)
		}
		s := statusOf(t, in, "web")
		if s.State != "healthy" || !s.ExhaustedAt.IsZero() || s.Attempts != 2 {
			t.Errorf("after a recovery by someone else: %+v", s)
		}
		r.stop(t)
		if f := readFileState(t, r.stateDir).Services["web"]; !f.ExhaustedAt.IsZero() || len(f.Attempts) != 2 {
			t.Errorf("saved %+v", f)
		}
	})
}

// sentineld's own restart is never counted as systemd's, also when it
// stopped before it saw the result; systemd's own restarts since are
// (review 3c-2 C7).
func TestUnitOwnRestartAttribution(t *testing.T) {
	inv := func(c byte) string { return strings.Repeat(string(c), 32) }
	t0 := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	base := unitBase{Boot: testBoot, Invocation: inv('a'), NRestarts: 4, Own: inv('a')}
	tests := []struct {
		name     string
		st       systemd.UnitState
		restarts uint64
	}{
		{"ours alone", runningUnit('b', 0), 0},
		{"ours, then systemd's twice", runningUnit('c', 2), 2},
		{"ours did not happen", runningUnit('a', 4), 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, b, n := judgeUnit(unitMemo{}, base, tt.st, t0, time.Minute, testBoot)
			if n != tt.restarts || b.Own != "" || b.Invocation != tt.st.InvocationID {
				t.Errorf("restarts %d, baseline %+v; want %d", n, b, tt.restarts)
			}
		})
	}
}

// The attempt and the invocation it replaces are saved together before
// systemd is asked: a restart of sentineld after it still knows the
// restart was its own.
func TestUnitOwnRestartSavedWithAttempt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fu := &fakeUnits{}
		f := newFakeSystemd(fu, dead)
		f.edit(func(f *fakeSystemd) { f.polls = 1000 }) // the job never ends while sentineld runs
		in := configureRestart(t, armedUnits, "    recovery: {delay: 1s}\n", fu)
		r := start(t, in, "")
		synctest.Wait()
		tick(2 * time.Second)
		if n := fu.restartCount(); n != 1 {
			t.Fatalf("%d restarts", n)
		}
		if st := readFileState(t, r.stateDir).Services["web"]; st.UnitOwnRestart != dead.InvocationID || len(st.Attempts) != 1 {
			t.Fatalf("saved %+v", st)
		}
		r.stop(t)
		var outcome string
		for _, e := range r.audit.recorded() {
			if e.Action == actionRestart && e.Result != audit.ResultIntent {
				outcome = string(e.Result) + "/" + e.Detail["failure"]
			}
		}
		if outcome != "failed/"+failureCanceled {
			t.Errorf("outcome at stop %q", outcome)
		}
	})
}
