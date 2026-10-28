package supervisor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/audit"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/clock"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/module"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/state"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/platform/executor"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/model"
)

// loopService restarts at most three times an hour, after 1s, 2s, 4s.
const loopService = "version: 1\nservices:\n  - name: worker\n    command: /bin/sh\n    startup_grace: 0s\n" +
	"    recovery: {max_attempts: 3, window: 1h, delay: 1s, max_delay: 1m, stable_after: 1m, cooldown: 10m}\n" +
	"    notifications: {channels: [ops], events: [service_restarted, service_recovery_exhausted]}\n"

const testIncident = "0123456789abcdef0123456789abcdef"

var exit1 = executor.Exit{Code: 1}

func loopFiles() map[string]string { return map[string]string{"supervisor/a.yaml": loopService} }

// crash ends the i-th launched process with status 1.
func crash(l *fakeLauncher, i int) {
	l.proc(i).end(exit1, nil)
	synctest.Wait()
}

// countTypes counts events by type.
func countTypes(evs []model.Event) map[model.EventType]int {
	n := map[model.EventType]int{}
	for _, e := range evs {
		n[e.Type]++
	}
	return n
}

// writeFailedState writes a state file in which in's worker is failed with
// an open incident, the given budget and group records.
func writeFailedState(t *testing.T, stateDir string, in *instance, b budget, groups map[string]groupRecord) {
	t.Helper()
	f := fileState{Services: map[string]serviceState{"worker": {Digest: in.byName["worker"].digest, Health: HealthFailed,
		Since: time.Now(), IncidentID: testIncident, Failures: 1, LastFailure: FailureExited,
		Attempts: b.Attempts, ExhaustedAt: b.ExhaustedAt}}, Groups: groups}
	data, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	writeStateFile(t, stateDir, string(data))
}

// D-013, D-084 (5): one first launch, then exactly max_attempts attempts
// with growing delays, then exhausted with one event; after the cooldown
// the budget is renewed and attempt 1 starts at once.
func TestRecoveryBounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := mustConfigureProcs(t, loopFiles(), l)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		for i, d := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second} {
			crash(l, i)
			tick(d - time.Millisecond)
			if l.launches() != i+1 {
				t.Fatalf("attempt %d launched before its delay of %s", i+1, d)
			}
			tick(time.Millisecond)
			if l.launches() != i+2 {
				t.Fatalf("attempt %d not launched after %s", i+1, d)
			}
		}
		crash(l, 3)
		s := statusOf(t, in, "worker")
		if s.Phase != phaseExhausted || s.Attempts != 3 || s.MaxAttempts != 3 || s.Restarts != 3 ||
			s.ExhaustedAt.IsZero() || !s.ResumeAt.Equal(s.ExhaustedAt.Add(10*time.Minute)) {
			t.Fatalf("exhausted: %+v", s)
		}
		if f := readFileState(t, r.stateDir).Services["worker"]; f.ExhaustedAt.IsZero() || len(f.Attempts) != 3 {
			t.Errorf("state file: %+v", f)
		}
		tick(10*time.Minute - time.Second)
		if l.launches() != 4 {
			t.Fatal("launched during the cooldown")
		}
		tick(time.Second)
		if l.launches() != 5 {
			t.Fatal("not launched at the end of the cooldown")
		}
		evs := r.drain()
		n := countTypes(evs)
		if n[EventServiceFailed] != 1 || n[EventServiceRestarted] != 4 || n[EventServiceRecoveryExhausted] != 1 {
			t.Errorf("events: %v", n)
		}
		incident := evs[0].ID
		for _, e := range evs[1:] {
			if e.CorrelationID != incident {
				t.Errorf("%s not correlated with the incident: %+v", e.Type, e)
			}
		}
		if got := in.NotificationChannels(evs[len(evs)-1]); len(got) != 1 || got[0] != "ops" {
			t.Errorf("service_restarted not routed: %v", got)
		}
		last := r.audit.recorded()
		e := last[len(last)-1]
		if e.Action != actionRestart || e.Result != audit.ResultOK || e.Detail["trigger"] != triggerCooldown ||
			e.Detail["attempt"] != "1" || e.CorrelationID != incident {
			t.Errorf("last audit entry %+v", e)
		}
		if s := statusOf(t, in, "worker"); s.Attempts != 1 || !s.ExhaustedAt.IsZero() || !s.ResumeAt.IsZero() {
			t.Errorf("after the cooldown: %+v", s)
		}
	})
}

// D-084 (6), ADR-0011: every launch is an intent, then an outcome, in the
// service's name and the incident's correlation; nothing of the program.
func TestRecoveryAuditEntries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := mustConfigureProcs(t, loopFiles(), l)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		crash(l, 0)
		l.mu.Lock()
		l.launchErr = errFakeExec
		l.mu.Unlock()
		tick(time.Second)
		got := r.audit.recorded()
		want := []struct {
			action  string
			result  audit.Result
			trigger string
			attempt string
			extra   string
		}{
			{actionStart, audit.ResultIntent, triggerStartup, "", ""},
			{actionStart, audit.ResultOK, triggerStartup, "", "pid=1001"},
			{actionRestart, audit.ResultIntent, triggerFailure, "1", ""},
			{actionRestart, audit.ResultFailed, triggerFailure, "1", "failure=exec_failed"},
		}
		if len(got) != len(want) {
			t.Fatalf("entries %+v", got)
		}
		incident := statusOf(t, in, "worker").IncidentID
		for i, w := range want {
			e := got[i]
			if e.Action != w.action || e.Result != w.result || e.Target != "worker" || e.Detail["trigger"] != w.trigger ||
				e.Detail["attempt"] != w.attempt {
				t.Errorf("entry %d = %+v", i, e)
			}
			if k, v, ok := strings.Cut(w.extra, "="); ok && e.Detail[k] != v {
				t.Errorf("entry %d detail %v, want %s", i, e.Detail, w.extra)
			}
			if i >= 2 && e.CorrelationID != incident {
				t.Errorf("entry %d not correlated with the incident", i)
			}
			for _, v := range e.Detail {
				if strings.Contains(v, "/bin/sh") {
					t.Errorf("entry %d names the program", i)
				}
			}
		}
		if got[0].CorrelationID == "" || got[0].CorrelationID != got[1].CorrelationID {
			t.Error("intent and outcome of the first launch are not correlated")
		}
	})
}

// D-014, C3: a full window in the state file without a recorded
// exhaustion (sentineld stopped during the last attempt) launches nothing
// until the cooldown ends.
func TestRecoveryFullBudgetAtStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := mustConfigureProcs(t, loopFiles(), l)
		stateDir := filepath.Join(t.TempDir(), "state")
		now := time.Now()
		writeFailedState(t, stateDir, in, budget{Attempts: []time.Time{now.Add(-3 * time.Minute), now.Add(-2 * time.Minute), now}}, nil)
		r := start(t, in, stateDir)
		defer r.stop(t)
		tick(10*time.Minute - time.Second)
		if l.launches() != 0 {
			t.Fatal("launched with a spent budget")
		}
		if n := countTypes(r.drain()); n[EventServiceRecoveryExhausted] != 1 {
			t.Errorf("events %v", n)
		}
		if f := readFileState(t, stateDir).Services["worker"]; !f.ExhaustedAt.Equal(now) {
			t.Errorf("exhaustion not saved: %+v", f)
		}
		tick(time.Second)
		if l.launches() != 1 {
			t.Error("not launched after the cooldown")
		}
	})
}

// D-014, C4: a restart of sentineld or a module replacement keeps the
// attempts of an open incident: the first launch is the next attempt,
// after its delay; a healthy service's relaunch is not an attempt.
func TestRecoveryAcrossRestarts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := mustConfigureProcs(t, loopFiles(), l)
		r := start(t, in, "")
		synctest.Wait()
		crash(l, 0)
		tick(time.Second)
		crash(l, 1)
		tick(2 * time.Second) // attempt 2 launched
		r.stop(t)
		if f := readFileState(t, r.stateDir).Services["worker"]; len(f.Attempts) != 2 || f.Restarts != 2 {
			t.Fatalf("state file %+v", f)
		}
		l2 := &fakeLauncher{}
		in2 := mustConfigureProcs(t, loopFiles(), l2)
		r2 := start(t, in2, r.stateDir)
		tick(4*time.Second - time.Millisecond)
		if l2.launches() != 0 {
			t.Fatal("attempt 3 launched before its delay")
		}
		tick(time.Millisecond)
		if l2.launches() != 1 || statusOf(t, in2, "worker").Attempts != 3 {
			t.Fatalf("attempt 3: %d launches, %+v", l2.launches(), statusOf(t, in2, "worker"))
		}
		crash(l2, 0)
		if s := statusOf(t, in2, "worker"); s.Phase != phaseExhausted {
			t.Errorf("not exhausted after the third attempt: %+v", s)
		}
		r2.stop(t)

		// A healthy service stopped by sentineld relaunches without using an attempt.
		l3 := &fakeLauncher{}
		in3 := mustConfigureProcs(t, map[string]string{"supervisor/a.yaml": workerService}, l3)
		r3 := start(t, in3, "")
		synctest.Wait()
		r3.stop(t)
		l4 := &fakeLauncher{}
		in4 := mustConfigureProcs(t, map[string]string{"supervisor/a.yaml": workerService}, l4)
		r4 := start(t, in4, r3.stateDir)
		defer r4.stop(t)
		synctest.Wait()
		if s := statusOf(t, in4, "worker"); l4.launches() != 1 || s.Attempts != 0 || s.Restarts != 0 {
			t.Errorf("relaunch counted: %+v", s)
		}
		if e := r4.audit.recorded(); len(e) < 1 || e[0].Action != actionStart || e[0].Detail["trigger"] != triggerStartup {
			t.Errorf("audit %+v", e)
		}
	})
}

// D-084 (6), C1: a recorded group is cleaned up before anything else,
// even with the budget exhausted and the audit log refusing everything;
// no launch follows.
func TestRecoveryCleanupComesFirst(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := mustConfigureProcs(t, loopFiles(), l)
		stateDir := filepath.Join(t.TempDir(), "state")
		writeFailedState(t, stateDir, in, budget{ExhaustedAt: time.Now()},
			map[string]groupRecord{"worker": {Pgid: 4242, Sid: 50, Start: 9, Boot: testBoot}})
		r := &rig{in: in, stateDir: stateDir, logs: &syncBuffer{}, audit: &fakeAuditor{}, faults: &faults{}}
		r.audit.setFail(func(module.AuditEntry) bool { return true })
		startRig(t, r)
		defer r.stop(t)
		tick(time.Minute)
		if c := l.cleaned(); len(c) != 1 || c[0].Pgid != 4242 {
			t.Errorf("cleanups %+v", c)
		}
		if l.launches() != 0 {
			t.Error("launched while exhausted")
		}
		if !strings.Contains(r.logs.String(), "could not record a cleanup") {
			t.Error("the audit failure of the cleanup is not logged")
		}
	})
}

// D-086 (6): while the cleanup of a recorded group fails, nothing is
// launched; it is retried, and the service starts once it succeeds.
func TestRecoveryNoLaunchWhileCleanupFails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stateDir := filepath.Join(t.TempDir(), "state")
		writeStateFile(t, stateDir, `{"services": {}, "groups": {"worker": {"pgid": 4242, "sid": 50, "start": 9, "boot": "`+testBoot+`"}}}`)
		l := &fakeLauncher{cleanupErr: executor.ErrCleanupIncomplete}
		in := mustConfigureProcs(t, loopFiles(), l)
		r := start(t, in, stateDir)
		defer r.stop(t)
		tick(5 * time.Minute)
		if l.launches() != 0 || len(l.cleaned()) < 3 {
			t.Fatalf("%d launches, %d cleanups", l.launches(), len(l.cleaned()))
		}
		if n := countTypes(r.drain()); n[EventServiceFailed] != 1 {
			t.Errorf("one failure per streak, not per retry: %v", n)
		}
		l.mu.Lock()
		l.cleanupErr = nil
		l.mu.Unlock()
		tick(time.Minute + time.Second) // the next retry, then the delay of attempt 1 (the failure opened an incident)
		if l.launches() != 1 {
			t.Errorf("%d launches after the cleanup succeeded", l.launches())
		}
	})
}

// ADR-0011: an intent that cannot be recorded stops the launch; no attempt
// is used; the launch happens once the audit log takes records again.
func TestRecoveryBlockedByAudit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := mustConfigureProcs(t, loopFiles(), l)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		r.audit.setFail(func(e module.AuditEntry) bool { return e.Result == audit.ResultIntent })
		crash(l, 0)
		tick(10 * time.Minute)
		s := statusOf(t, in, "worker")
		if l.launches() != 1 || s.Phase != phaseBlocked || s.BlockedReason != blockedAudit || s.Attempts != 0 {
			t.Fatalf("%d launches, %+v", l.launches(), s)
		}
		if f := readFileState(t, r.stateDir).Services["worker"]; len(f.Attempts) != 0 {
			t.Errorf("a refused intent used an attempt: %+v", f.Attempts)
		}
		r.audit.setFail(nil)
		tick(time.Minute + time.Second)
		if l.launches() != 2 || statusOf(t, in, "worker").Attempts != 1 {
			t.Errorf("%d launches after the audit log came back", l.launches())
		}
	})
}

// D-014, C2: an attempt that cannot be saved is not launched; its intent
// gets a failed outcome; the launch happens once the state can be saved.
func TestRecoveryBlockedByState(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := mustConfigureProcs(t, loopFiles(), l)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		dir := filepath.Join(r.stateDir, moduleName)
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		crash(l, 0)
		tick(time.Second)
		s := statusOf(t, in, "worker")
		if l.launches() != 1 || s.Phase != phaseBlocked || s.BlockedReason != blockedState || s.Attempts != 0 {
			t.Fatalf("%d launches, %+v", l.launches(), s)
		}
		e := r.audit.recorded()
		if last := e[len(e)-1]; last.Result != audit.ResultFailed || last.Detail["failure"] != blockedState {
			t.Errorf("intent left without its outcome: %+v", last)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		tick(time.Minute + time.Second) // the retry, then the attempt's delay again
		if l.launches() != 2 {
			t.Errorf("%d launches once the state could be saved", l.launches())
		}
	})
}

// C5: a launched process whose outcome cannot be recorded stays owned and
// watched; it is never launched again to repair the record.
func TestRecoveryOutcomeFailureKeepsTheProcess(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := mustConfigureProcs(t, loopFiles(), l)
		r := &rig{in: in, stateDir: filepath.Join(t.TempDir(), "state"), logs: &syncBuffer{}, audit: &fakeAuditor{}, faults: &faults{}}
		r.audit.setFail(func(e module.AuditEntry) bool { return e.Result == audit.ResultOK })
		startRig(t, r)
		tick(time.Hour)
		if s := statusOf(t, in, "worker"); l.launches() != 1 || s.Pid == 0 || s.Phase != phaseRunning {
			t.Fatalf("%d launches, %+v", l.launches(), s)
		}
		r.stop(t)
		if len(l.proc(0).stopCalls()) != 1 {
			t.Error("the process was not stopped with the module")
		}
	})
}

// C5: Stop while an intent is in flight ends the loop without a launch.
func TestRecoveryStopDuringIntent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := mustConfigureProcs(t, loopFiles(), l)
		r := &rig{in: in, stateDir: filepath.Join(t.TempDir(), "state"), logs: &syncBuffer{}, audit: &fakeAuditor{hold: make(chan struct{})}, faults: &faults{}}
		startRig(t, r)
		synctest.Wait()
		r.stop(t)
		if l.launches() != 0 {
			t.Error("launched after Stop")
		}
	})
}

// Shutdown during a delay, an exhaustion or a block ends the loop at
// once, with no launch and no failure.
func TestRecoveryStopWhileWaiting(t *testing.T) {
	tests := []struct {
		name  string
		setup func(r *rig, l *fakeLauncher)
		phase string
	}{
		{"restarting", func(_ *rig, l *fakeLauncher) { crash(l, 0) }, phaseRestarting},
		{"blocked", func(r *rig, l *fakeLauncher) {
			r.audit.setFail(func(e module.AuditEntry) bool { return e.Result == audit.ResultIntent })
			crash(l, 0)
			tick(time.Second)
		}, phaseBlocked},
		{"exhausted", func(_ *rig, l *fakeLauncher) {
			for i, d := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 0} {
				crash(l, i)
				tick(d)
			}
		}, phaseExhausted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				l := &fakeLauncher{}
				in := mustConfigureProcs(t, loopFiles(), l)
				r := start(t, in, "")
				synctest.Wait()
				tt.setup(r, l)
				if s := statusOf(t, in, "worker"); s.Phase != tt.phase {
					t.Fatalf("phase %s, want %s", s.Phase, tt.phase)
				}
				before := l.launches()
				r.stop(t)
				tick(time.Hour)
				if l.launches() != before {
					t.Error("launched after Stop")
				}
			})
		})
	}
}

// D-084 (3): a corrupt state file starts the processes with their budgets
// spent: the first crash exhausts; the daemon is told once; status says so.
func TestRecoveryCorruptState(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stateDir := filepath.Join(t.TempDir(), "state")
		writeStateFile(t, stateDir, `{"services": {"BAD NAME": {}}}`)
		l := &fakeLauncher{}
		in := mustConfigureProcs(t, loopFiles(), l)
		r := start(t, in, stateDir)
		defer r.stop(t)
		synctest.Wait()
		if l.launches() != 1 {
			t.Fatal("the processes did not start")
		}
		if f := r.faults.list(); len(f) != 1 || f[0] != "state_load/state_corrupt" {
			t.Errorf("faults %v", f)
		}
		if st, _ := in.status(""); !st.StateRecovered {
			t.Error("status does not say the state was recovered")
		}
		crash(l, 0)
		tick(time.Minute)
		if s := statusOf(t, in, "worker"); l.launches() != 1 || s.Phase != phaseExhausted {
			t.Errorf("%d launches, %+v", l.launches(), s)
		}
	})
}

// D-087 Q3: stability closes each incident but does not free the budget:
// a process that runs for stable_after between crashes is still exhausted
// after max_attempts attempts in the window.
func TestRecoveryStabilityKeepsAttempts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := mustConfigureProcs(t, loopFiles(), l)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		for i, d := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second} {
			crash(l, i)
			tick(d + time.Minute) // launched, then stable
			if s := statusOf(t, in, "worker"); s.State != "healthy" {
				t.Fatalf("round %d: %+v", i, s)
			}
		}
		crash(l, 3)
		if s := statusOf(t, in, "worker"); s.Phase != phaseExhausted {
			t.Errorf("a slow flapper is not bounded: %+v", s)
		}
		n := countTypes(r.drain())
		if n[EventServiceFailed] != 4 || n[EventServiceRecovered] != 3 || n[EventServiceRecoveryExhausted] != 1 {
			t.Errorf("events %v", n)
		}
	})
}

// D-086 (1), C9: a program made untrusted between two attempts is not
// launched again.
func TestRecoveryUntrustedReplacement(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "bin")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		prog := filepath.Join(dir, "worker")
		if err := os.WriteFile(prog, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		body := strings.Replace(loopService, "command: /bin/sh", "command: "+prog, 1)
		l := &fakeLauncher{}
		in, err := configureProcs(t, map[string]string{"supervisor/a.yaml": body}, l)
		if err != nil {
			t.Skipf("the temporary directory is not trusted here: %v", err)
		}
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		if err := os.Chmod(dir, 0o777); err != nil {
			t.Fatal(err)
		}
		crash(l, 0)
		tick(time.Second)
		if s := statusOf(t, in, "worker"); l.launches() != 1 || s.LastFailure != string(FailureUntrusted) {
			t.Errorf("%d launches, %+v", l.launches(), s)
		}
	})
}

// C6: a save serialises a copy of the attempts, never the slice the loop
// keeps changing (run with -race).
func TestSaveSnapshotsAttempts(t *testing.T) {
	in := mustConfigureProcs(t, map[string]string{"sentinel.yaml": centralFile, "supervisor/a.yaml": loopService}, &fakeLauncher{})
	r := start(t, in, "")
	defer r.stop(t)
	s := in.byName["worker"]
	in.mu.Lock()
	s.st.Health, s.st.IncidentID = HealthFailed, testIncident
	s.st.Attempts = make([]time.Time, 3, 8)
	in.mu.Unlock()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := range 200 {
			in.mu.Lock()
			s.st.Attempts[i%3] = time.Now()
			in.mu.Unlock()
		}
	}()
	go func() {
		defer wg.Done()
		for range 50 {
			in.mu.Lock()
			in.gen++
			gen := in.gen
			in.mu.Unlock()
			in.save(gen)
		}
	}()
	wg.Wait()
}

// startRig starts r.in with r's prepared runtime.
func startRig(t *testing.T, r *rig) {
	t.Helper()
	if r.stateDir == "" {
		r.stateDir = filepath.Join(t.TempDir(), "state")
	}
	dir, err := state.OpenDir(r.stateDir, clock.Real())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dir.Close() })
	r.dir = dir
	if err := r.in.Start(context.Background(), runtimeFor(r, dir)); err != nil {
		t.Fatalf("Start: %v", err)
	}
}

// Review 3b-3 R1: a recovery key set to null is a default or a problem,
// never a panic.
func TestRecoveryNullFields(t *testing.T) {
	for _, key := range []string{"action", "max_attempts", "window", "delay", "backoff", "max_delay", "stable_after", "cooldown"} {
		t.Run(key, func(t *testing.T) {
			body := "version: 1\nservices:\n  - name: worker\n    command: /bin/sh\n    recovery: {" + key + ": null}\n"
			defer func() {
				if p := recover(); p != nil {
					t.Fatalf("Configure panicked on %s: null", key)
				}
			}()
			in, err := configureProcs(t, map[string]string{"supervisor/a.yaml": body}, &fakeLauncher{})
			if err == nil && in.byName["worker"].def.Process.Recovery.MaxAttempts != defaultMaxAttempts {
				t.Errorf("null %s changed the policy: %+v", key, in.byName["worker"].def.Process.Recovery)
			}
		})
	}
}

// Review 3b-3 R3: once the cooldown is over, a launch blocked by the
// audit log shows as blocked, not as exhausted.
func TestCooldownThenBlockedClearsResumeAt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := mustConfigureProcs(t, loopFiles(), l)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		for i, d := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 0} {
			crash(l, i)
			tick(d)
		}
		r.audit.setFail(func(e module.AuditEntry) bool { return e.Result == audit.ResultIntent })
		tick(10 * time.Minute)
		s := statusOf(t, in, "worker")
		if s.Phase != phaseBlocked || s.BlockedReason != blockedAudit || !s.ResumeAt.IsZero() {
			t.Fatalf("status %+v", s)
		}
	})
}
