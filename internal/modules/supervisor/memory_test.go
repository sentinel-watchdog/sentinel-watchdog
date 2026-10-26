package supervisor

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/audit"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/module"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/platform/executor"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/api"
)

const memLimit = 64 << 20

// memFiles is worker with a 64 MiB limit, stopped with SIGTERM within 3s,
// restarted at most twice an hour after 1s.
func memFiles(extra string) map[string]string {
	return map[string]string{"supervisor/a.yaml": "version: 1\nservices:\n  - name: worker\n    command: /bin/sh\n" +
		"    startup_grace: 0s\n    stop_timeout: 3s\n    limits: {memory: 64MiB}\n" +
		"    recovery: {max_attempts: 2, window: 1h, delay: 1s, max_delay: 1m, stable_after: 1m, cooldown: 10m}\n" + extra}
}

// fixedRSS samples every process at rss.
func fixedRSS(rss uint64) func(int) (uint64, error) {
	return func(int) (uint64, error) { return rss, nil }
}

// D-090: a sample at or under the limit leaves the process alone; one
// over it stops the whole group with the service's signal and timeout,
// closes its output, records memory_limit with the sizes, and hands over
// to the recovery: attempts, then exhausted.
func TestMemoryLimitStops(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{partial: "last words"}
		in := mustConfigureProcs(t, memFiles(""), l)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		for _, rss := range []uint64{memLimit - 1, memLimit} {
			l.setRSS(fixedRSS(rss))
			tick(memorySampleInterval)
			if s := statusOf(t, in, "worker"); s.Phase != phaseRunning || s.MemoryRSS != rss || s.MemoryLimit != memLimit ||
				s.MemorySampledAt.IsZero() || len(l.proc(0).stopCalls()) != 0 {
				t.Fatalf("at %d bytes: %+v", rss, s)
			}
		}
		l.setRSS(fixedRSS(memLimit + 1))
		tick(memorySampleInterval)
		if stops := l.proc(0).stopCalls(); len(stops) != 1 || stops[0].sig != syscall.SIGTERM || stops[0].timeout != 3*time.Second {
			t.Fatalf("stop calls %+v", stops)
		}
		s := statusOf(t, in, "worker")
		if s.Phase != phaseRestarting || s.LastFailure != string(FailureMemory) || s.State != string(HealthFailed) ||
			s.Pid != 0 || s.MemoryRSS != 0 || !s.MemorySampledAt.IsZero() {
			t.Fatalf("after the stop: %+v", s)
		}
		if !strings.Contains(r.logs.String(), "last words") {
			t.Error("the output sink was not closed: its partial line is lost")
		}
		if f := readFileState(t, r.stateDir); len(f.Groups) != 0 || f.Services["worker"].LastFailure != FailureMemory {
			t.Errorf("state file: %+v", f)
		}
		evs := r.drain()
		if len(evs) != 1 || evs[0].Type != EventServiceFailed || evs[0].Attributes["failure"] != string(FailureMemory) ||
			evs[0].Attributes["rss_bytes"] != uint64(memLimit+1) || evs[0].Attributes["limit_bytes"] != uint64(memLimit) ||
			!strings.Contains(evs[0].Message, "over its limit of 64.0 MiB") {
			t.Fatalf("events %+v", evs)
		}
		stop := entries(r, actionStop)
		if len(stop) != 2 || stop[0].Result != audit.ResultIntent || stop[1].Result != audit.ResultOK {
			t.Fatalf("stop records %+v", stop)
		}
		for _, e := range stop {
			if e.Target != "worker" || e.CorrelationID != evs[0].ID || e.Detail["trigger"] != triggerMemory ||
				e.Detail["rss_bytes"] != "67108865" || e.Detail["limit_bytes"] != "67108864" {
				t.Errorf("stop record %+v", e)
			}
		}
		for i := 1; i <= 2; i++ { // two attempts, each over the limit at its first sample
			tick(time.Duration(i) * time.Second)
			if l.launches() != i+1 {
				t.Fatalf("attempt %d not launched", i)
			}
			tick(memorySampleInterval)
		}
		s = statusOf(t, in, "worker")
		if s.Phase != phaseExhausted || s.Failures != 3 || s.Restarts != 2 {
			t.Errorf("exhausted: %+v", s)
		}
		if n := len(entries(r, actionStop)); n != 6 {
			t.Errorf("%d stop records, want 6", n)
		}
		for _, e := range entries(r, actionStop) {
			if e.CorrelationID != evs[0].ID {
				t.Errorf("a later stop not in the incident: %+v", e)
			}
		}
	})
}

// No limit, no sampling.
func TestMemoryNoLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := mustConfigureProcs(t, opFiles(""), l)
		r := start(t, in, "")
		defer r.stop(t)
		tick(time.Hour)
		if n := l.samples(); n != 0 {
			t.Errorf("%d samples without a limit", n)
		}
		if s := statusOf(t, in, "worker"); s.MemoryLimit != 0 || !s.MemorySampledAt.IsZero() {
			t.Errorf("status %+v", s)
		}
	})
}

// D-090 Q3: a sample that cannot be read is never zero and never a
// failure: the process keeps running, status says memory_unknown, one
// error is logged per streak, and a good sample clears it. ErrGone is the
// end of the process, seen on Done: nothing to report.
func TestMemoryUnknownFailsOpen(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := mustConfigureProcs(t, memFiles(""), l)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		l.setRSS(func(int) (uint64, error) { return 0, executor.ErrGone })
		tick(memorySampleInterval)
		if s := statusOf(t, in, "worker"); s.MemoryUnknown || s.Phase != phaseRunning {
			t.Fatalf("ErrGone: %+v", s)
		}
		l.setRSS(func(int) (uint64, error) { return 0, errors.New("statm: invalid resident size") })
		tick(3 * memorySampleInterval)
		s := statusOf(t, in, "worker")
		if !s.MemoryUnknown || s.Phase != phaseRunning || s.Failures != 0 || len(l.proc(0).stopCalls()) != 0 {
			t.Fatalf("unreadable: %+v", s)
		}
		if n := strings.Count(r.logs.String(), "cannot read the memory"); n != 1 {
			t.Errorf("%d errors logged for one streak", n)
		}
		l.setRSS(fixedRSS(1 << 20))
		tick(memorySampleInterval)
		if s := statusOf(t, in, "worker"); s.MemoryUnknown || s.MemoryRSS != 1<<20 {
			t.Errorf("after a good sample: %+v", s)
		}
	})
}

// D-021, D-090 Q6: where processes cannot be sampled, a service with a
// memory limit is not started (unsupported) instead of running unchecked;
// one without a limit starts.
func TestMemoryLimitUnsupported(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{noSampling: true}
		in := mustConfigureProcs(t, memFiles("  - name: other\n    command: /bin/sh\n"), l)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		if l.launches() != 1 || !strings.Contains(r.logs.String(), "cannot enforce limits.memory") {
			t.Fatalf("%d launches", l.launches())
		}
		s := statusOf(t, in, "worker")
		if s.Phase == phaseRunning || s.LastFailure != string(FailureUnsupported) {
			t.Errorf("worker: %+v", s)
		}
		if s := statusOf(t, in, "other"); s.Pid == 0 {
			t.Errorf("other: %+v", s)
		}
	})
}

// D-090 Q4: the intent comes first and is waited for at most auditTimeout;
// a broken or stuck audit log never prevents the stop.
func TestMemoryStopDoesNotWaitForTheAuditLog(t *testing.T) {
	for _, stuck := range []bool{false, true} {
		t.Run(map[bool]string{false: "broken", true: "stuck"}[stuck], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				l := &fakeLauncher{}
				in := mustConfigureProcs(t, memFiles(""), l)
				r := start(t, in, "")
				defer r.stop(t)
				synctest.Wait()
				if stuck {
					r.audit.mu.Lock()
					r.audit.hold = make(chan struct{}) // never closed
					r.audit.mu.Unlock()
				} else {
					r.audit.setFail(func(e module.AuditEntry) bool { return e.Action == actionStop })
				}
				l.setRSS(fixedRSS(memLimit + 1))
				tick(memorySampleInterval)
				if stuck {
					if len(l.proc(0).stopCalls()) != 0 {
						t.Fatal("stopped before the intent's bound")
					}
					tick(auditTimeout)
				}
				if len(l.proc(0).stopCalls()) != 1 {
					t.Fatal("the audit log held the stop back")
				}
				if s := statusOf(t, in, "worker"); s.LastFailure != string(FailureMemory) {
					t.Errorf("status %+v", s)
				}
				if !strings.Contains(r.logs.String(), "the process is stopped anyway") {
					t.Error("the missing intent was not logged")
				}
			})
		})
	}
}

// operate runs cmd on worker as the transport does, from a goroutine of
// its own: without synctest.Wait, which only the test's goroutine calls.
func operate(cmd api.Command) func(t *testing.T, r *rig, _ *fakeLauncher) {
	return func(t *testing.T, r *rig, _ *fakeLauncher) {
		ctx := audit.WithCorrelation(context.Background(), testRequest)
		if _, apiErr := r.in.Handle(ctx, cmd.Name, jsontext.Value(`{"name":"worker"}`)); apiErr != nil {
			t.Errorf("%s: %v", cmd.Name, apiErr)
		}
	}
}

// Review 3b-6 C1: a memory stop in progress keeps its stop_timeout
// whatever ends the loop meanwhile; the end is recorded once, as
// memory_limit, and nothing is launched before the process has ended.
func TestMemoryStopKeepsItsTimeout(t *testing.T) {
	tests := []struct {
		name string
		// act ends the loop; it runs in its own goroutine.
		act func(t *testing.T, r *rig, l *fakeLauncher)
		// launches after the process ended and act is done.
		launches int
	}{
		{"reset", operate(api.SupervisorReset), 2},
		{"restart", operate(api.SupervisorRestart), 2},
		{"stop", operate(api.SupervisorStop), 1},
		{"disable", operate(api.SupervisorDisable), 1},
		{"reload", func(t *testing.T, r *rig, l *fakeLauncher) {
			if err := reconfigureTo(t, context.Background(), r, l, memFiles("    stop_signal: INT\n")); err != nil {
				t.Errorf("reload: %v", err)
			}
		}, 2},
		{"module stop", func(t *testing.T, r *rig, _ *fakeLauncher) {
			if err := r.in.Stop(context.Background()); err != nil {
				t.Errorf("Stop: %v", err)
			}
		}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				l := &fakeLauncher{}
				in := mustConfigureProcs(t, memFiles(""), l)
				r := start(t, in, "")
				synctest.Wait()
				p := l.proc(0)
				setStubborn(p)
				l.setRSS(fixedRSS(memLimit + 1))
				tick(memorySampleInterval)
				var acted atomic.Bool
				go func() { tt.act(t, r, l); acted.Store(true) }()
				tick(2*time.Second + time.Millisecond) // past an operator's wait, within stop_timeout
				if stops := p.stopCalls(); len(stops) != 1 {
					t.Fatalf("stop calls %+v: the stop was cut short or repeated", stops)
				}
				if l.launches() != 1 {
					t.Fatal("launched beside the process being stopped")
				}
				p.end(executor.Exit{Code: -1, Signal: "SIGTERM"}, nil)
				synctest.Wait()
				tick(2 * time.Second)
				if !acted.Load() {
					t.Fatal(tt.name + " did not end")
				}
				if l.launches() != tt.launches {
					t.Errorf("%d launches, want %d", l.launches(), tt.launches)
				}
				if tt.name != "reload" { // a reload starts the changed service from a fresh state
					if s := statusOf(t, in, "worker"); s.Failures != 1 || s.LastFailure != string(FailureMemory) {
						t.Errorf("the end was not recorded once as memory_limit: %+v", s)
					}
				}
				if n := countTypes(r.drain())[EventServiceFailed]; n != 1 {
					t.Errorf("%d service_failed", n)
				}
				if stop := entries(r, actionStop); len(stop) < 2 || stop[1].Result != audit.ResultOK ||
					stop[1].Detail["trigger"] != triggerMemory {
					t.Errorf("stop records %+v", stop)
				}
				if tt.name != "module stop" {
					r.stop(t)
				}
			})
		})
	}
}

// Review 3b-6 C1: when module Stop gives up waiting for a memory stop
// (the daemon's deadline), the process stays owned: Stop reports it, and
// once it ends the memory stop records it, once.
func TestMemoryStopBeyondTheDaemonsDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := mustConfigureProcs(t, memFiles(""), l)
		r := start(t, in, "")
		synctest.Wait()
		p := l.proc(0)
		setStubborn(p)
		l.setRSS(fixedRSS(memLimit + 1))
		tick(memorySampleInterval)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := in.Stop(ctx); err == nil || !strings.Contains(err.Error(), "1 processes did not end") {
			t.Fatalf("Stop: %v", err)
		}
		p.end(executor.Exit{Code: -1, Signal: "SIGKILL"}, nil)
		synctest.Wait()
		if s := statusOf(t, in, "worker"); s.Failures != 1 || s.LastFailure != string(FailureMemory) || s.Pid != 0 {
			t.Errorf("after the end: %+v", s)
		}
		if n := countTypes(r.drain())[EventServiceFailed]; n != 1 {
			t.Errorf("%d service_failed", n)
		}
	})
}

// A memory stop that gives up (the process does not end within
// stop_timeout and the cleanup bound) keeps the process owned and
// watched, samples no more, and its later end is still memory_limit.
func TestMemoryStopThatFails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := mustConfigureProcs(t, memFiles(""), l)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		p := l.proc(0)
		setStubborn(p)
		l.setRSS(fixedRSS(memLimit + 1))
		tick(memorySampleInterval)
		tick(3*time.Second + cleanupTimeout)
		if stop := entries(r, actionStop); len(stop) != 2 || stop[1].Result != audit.ResultFailed {
			t.Fatalf("stop records %+v", stop)
		}
		tick(time.Minute)
		if n := len(p.stopCalls()); n != 1 {
			t.Errorf("%d stops: a failed memory stop was repeated", n)
		}
		if l.launches() != 1 {
			t.Fatal("launched beside a process that did not stop")
		}
		p.end(executor.Exit{Code: -1, Signal: "SIGKILL"}, nil)
		synctest.Wait()
		if s := statusOf(t, in, "worker"); s.LastFailure != string(FailureMemory) || s.Failures != 1 {
			t.Errorf("after the end: %+v", s)
		}
	})
}

// An operator's reset ends the loop of a running process with a limit:
// the new loop adopts it and samples it again.
func TestMemorySampledAfterAdoption(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := mustConfigureProcs(t, memFiles(""), l)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		mustDo(t, in, api.SupervisorReset, "worker")
		l.setRSS(fixedRSS(memLimit + 1))
		tick(memorySampleInterval)
		if l.launches() != 1 || len(l.proc(0).stopCalls()) != 1 {
			t.Errorf("the adopted process was not sampled: %d launches, stops %+v", l.launches(), l.proc(0).stopCalls())
		}
	})
}

// Review 3b-6 C1: when module Stop gives up waiting for the loop and stops
// the process too, both stops may return; the end is recorded once, by
// the first to claim it (here the memory stop: a failure, not a stop).
func TestMemoryStopAndModuleStopRecordOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := mustConfigureProcs(t, memFiles(""), l)
		start(t, in, "")
		synctest.Wait()
		p := l.proc(0)
		p.mu.Lock()
		p.held = true
		p.mu.Unlock()
		l.setRSS(fixedRSS(memLimit + 1))
		tick(memorySampleInterval)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		go func() { _ = in.Stop(ctx) }()
		tick(time.Second) // module Stop gave up on the loop and stops the process itself
		p.mu.Lock()
		holds := slices.Clone(p.holds)
		p.mu.Unlock()
		if len(holds) != 2 {
			t.Fatalf("%d stops in flight, want the memory stop and module Stop's", len(holds))
		}
		close(holds[0])
		synctest.Wait()
		close(holds[1])
		synctest.Wait()
		s := statusOf(t, in, "worker")
		if s.Phase != phaseExited || s.LastFailure != string(FailureMemory) || s.Failures != 1 {
			t.Errorf("the end was recorded twice, or not as the memory stop's: %+v", s)
		}
	})
}

// Review 3b-6 R3: a process whose memory stop gave up is condemned: its
// startup grace and stable_after no longer change its health, so no
// service_recovered closes the incident its end will report.
func TestFailedMemoryStopDoesNotRecover(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := mustConfigureProcs(t, memFiles(""), l)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		crash(l, 0)
		tick(time.Second) // attempt 1, with the incident open
		p := l.proc(1)
		setStubborn(p)
		l.setRSS(fixedRSS(memLimit + 1))
		tick(memorySampleInterval)
		tick(3*time.Second + cleanupTimeout) // the stop gives up
		tick(time.Minute)                    // past stable_after
		if n := countTypes(r.drain())[EventServiceRecovered]; n != 0 {
			t.Errorf("%d service_recovered while the memory stop is unfinished", n)
		}
		if s := statusOf(t, in, "worker"); s.State != string(HealthFailed) {
			t.Errorf("status %+v", s)
		}
		p.end(executor.Exit{Code: -1, Signal: "SIGKILL"}, nil)
		synctest.Wait()
	})
}

// Review 3b-6 R3: nor does a startup grace that ends after the memory
// stop gave up make the service healthy.
func TestFailedMemoryStopDuringStartup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		files := map[string]string{"supervisor/a.yaml": "version: 1\nservices:\n  - name: worker\n    command: /bin/sh\n" +
			"    startup_grace: 30s\n    stop_timeout: 3s\n    limits: {memory: 64MiB}\n"}
		in := mustConfigureProcs(t, files, l)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		p := l.proc(0)
		setStubborn(p)
		l.setRSS(fixedRSS(memLimit + 1))
		tick(memorySampleInterval)
		tick(time.Minute) // the stop gave up, then the grace ended
		if s := statusOf(t, in, "worker"); s.State == string(HealthHealthy) || s.Phase != phaseStopping {
			t.Errorf("a condemned process became healthy: %+v", s)
		}
		p.end(executor.Exit{Code: -1, Signal: "SIGKILL"}, nil)
		synctest.Wait()
		if s := statusOf(t, in, "worker"); s.LastFailure != string(FailureMemory) {
			t.Errorf("after the end: %+v", s)
		}
	})
}

// D-090 (1): limits.memory is part of how a process runs, so a reload
// that changes it restarts the process, but not of its digest: the
// incident and the budget are kept, as for recovery.
func TestMemoryLimitChangeOnReload(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := mustConfigureProcs(t, memFiles(""), l)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		crash(l, 0)
		tick(time.Second) // attempt 1
		before := statusOf(t, in, "worker")
		files := memFiles("")
		files["supervisor/a.yaml"] = strings.Replace(files["supervisor/a.yaml"], "memory: 64MiB", "memory: 128MiB", 1)
		if err := reconfigureTo(t, context.Background(), r, l, files); err != nil {
			t.Fatal(err)
		}
		tick(2 * time.Second) // with the incident open, the launch is attempt 2, after its delay
		after := statusOf(t, in, "worker")
		if l.launches() != 3 || after.MemoryLimit != 128<<20 {
			t.Fatalf("%d launches, limit %d: the change did not restart the process", l.launches(), after.MemoryLimit)
		}
		if after.IncidentID != before.IncidentID || after.Attempts != 2 || after.Failures != before.Failures {
			t.Errorf("state not kept: before %+v\nafter %+v", before, after)
		}
	})
}
