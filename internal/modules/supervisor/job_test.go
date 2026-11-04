package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/audit"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/clock"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/module"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/platform/executor"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/api"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/model"
)

// jobsCentral arms programs and schedules in UTC: a synctest bubble
// starts at 2000-01-01T00:00:00Z.
const jobsCentral = "version: 1\ndaemon: {timezone: UTC}\n" + "notifications:\n  channels:\n" +
	"    - {name: ops, type: webhook, url: \"https://hooks.example.com/1\"}\n" +
	"modules:\n  supervisor:\n    enabled: true\n    processes: true\n"

const minuteJob = "version: 1\njobs:\n  - name: tick\n    schedule: \"* * * * *\"\n    command: /bin/sh\n" +
	"    environment: {API_KEY: \"literal-secret-42\"}\n    notifications: [ops]\n"

func configureJobs(t *testing.T, body string, l *fakeLauncher) *instance {
	t.Helper()
	return mustConfigureProcs(t, map[string]string{"sentinel.yaml": jobsCentral, "supervisor/a.yaml": body}, l)
}

// eventsOf returns the events of type typ among evs.
func eventsOf(evs []model.Event, typ model.EventType) []model.Event {
	var out []model.Event
	for _, e := range evs {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

// AC3, AC5, AC7, AC11: a job runs at each slot, never at Start; the
// intent and the reservation are recorded before the launch; the end is
// one job_succeeded with the fixed attributes, after the state is saved.
func TestJobRunsAtEachSlot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := configureJobs(t, minuteJob, l)
		var r *rig
		l.before = func(executor.Spec) {
			entries := r.audit.recorded()
			if len(entries) == 0 || entries[len(entries)-1].Result != audit.ResultIntent {
				t.Errorf("launch before its intent: %+v", entries)
			}
			runs := readFileState(t, r.stateDir).Runs
			if len(runs) != 1 {
				t.Errorf("launch before its reservation: %+v", runs)
			}
			for _, rec := range runs {
				if rec.Phase != runReserved || rec.Job != "tick" {
					t.Errorf("reservation = %+v", rec)
				}
			}
		}
		r = start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		if n := l.launches(); n != 0 {
			t.Fatalf("%d launches at Start: a job runs from its next slot (Q3)", n)
		}
		tick(time.Minute) // 00:01
		if n := l.launches(); n != 1 {
			t.Fatalf("%d launches at the first slot", n)
		}
		if spec := l.spec(0); !slices.Contains(spec.Env, "API_KEY=literal-secret-42") || spec.Arg0 != "/bin/sh" {
			t.Errorf("spec = %+v", spec)
		}
		entries := r.audit.recorded()
		if len(entries) != 2 || entries[0].Action != actionJobRun || entries[0].Target != "tick" ||
			entries[0].Result != audit.ResultIntent || entries[1].Result != audit.ResultOK ||
			entries[0].CorrelationID == "" || entries[1].CorrelationID != entries[0].CorrelationID {
			t.Fatalf("audit = %+v", entries)
		}
		id := entries[0].CorrelationID
		if rec := readFileState(t, r.stateDir).Runs[id]; rec.Phase != runRunning || rec.Group.Pgid != 1001 {
			t.Errorf("running record = %+v", rec)
		}
		tick(10 * time.Second)
		l.proc(0).end(executor.Exit{Code: 0}, nil)
		synctest.Wait()
		evs := eventsOf(r.drain(), EventJobSucceeded)
		if len(evs) != 1 {
			t.Fatalf("job_succeeded events: %+v", evs)
		}
		e := evs[0]
		if e.Source != "tick" || e.SourceType != typeCron || e.CorrelationID != id {
			t.Errorf("event = %+v", e)
		}
		wantKeys := []string{"duration_ms", "exit_code", "job", "run_id", "scheduled", "started"}
		var keys []string
		for k := range e.Attributes {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		if !slices.Equal(keys, wantKeys) || e.Attributes["scheduled"] != "2000-01-01T00:01:00Z" ||
			e.Attributes["duration_ms"] != int64(10000) || e.Attributes["exit_code"] != 0 {
			t.Errorf("attributes = %v", e.Attributes)
		}
		for _, v := range e.Attributes {
			if s, ok := v.(string); ok && strings.Contains(s, "literal-secret") {
				t.Errorf("an attribute holds the environment: %v", e.Attributes)
			}
		}
		st := readFileState(t, r.stateDir)
		js := st.Jobs["tick"]
		if len(st.Runs) != 0 || js.Runs != 1 || js.Succeeded != 1 || js.Last == nil || js.Last.Result != resultSucceeded ||
			!js.Through.Equal(time.Date(2000, 1, 1, 0, 1, 0, 0, time.UTC)) {
			t.Errorf("state = %+v %+v", st.Runs, js)
		}
		tick(50 * time.Second) // 00:02
		if n := l.launches(); n != 2 {
			t.Errorf("%d launches at the second slot", n)
		}
	})
}

// shiftedClock is the bubble's clock with its wall time moved by shift:
// timers still follow elapsed time, as the real monotonic timers do
// while an administrator or NTP sets the wall clock (decision 3c-4a).
type shiftedClock struct {
	mu    sync.Mutex
	shift time.Duration
}

func (c *shiftedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.shift)
}

func (c *shiftedClock) NewTimer(d time.Duration) clock.Timer { return clock.Real().NewTimer(d) }

func (c *shiftedClock) move(d time.Duration) {
	c.mu.Lock()
	c.shift += d
	c.mu.Unlock()
}

const hourlyJob = "version: 1\njobs:\n  - {name: hourly, schedule: \"0 * * * *\", command: /bin/sh}\n"

// AC3 (Q4, Q5, review C1): a slot launched up to a minute late runs; one
// later is missed; a catch-up keeps the slot still on time; a clock set
// back never repeats a slot; a forward jump is a catch-up.
func TestJobSlotsAndClockChanges(t *testing.T) {
	t.Run("on time up to a minute late", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := &fakeLauncher{}
			clk := &shiftedClock{}
			r := startOn(t, configureJobs(t, hourlyJob, l), "", clk)
			defer r.stop(t)
			clk.move(time.Hour + time.Minute) // the 01:00 slot is seen at 01:01:00 exactly
			tick(jobWake)                     // the loop wakes within jobWake
			if n := l.launches(); n != 1 {
				t.Errorf("%d launches for a slot a minute late", n)
			}
		})
	})
	t.Run("missed after a minute", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := &fakeLauncher{}
			clk := &shiftedClock{}
			r := startOn(t, configureJobs(t, hourlyJob, l), "", clk)
			defer r.stop(t)
			synctest.Wait()
			clk.move(time.Hour + 2*time.Minute) // first seen at 01:02 or later
			tick(jobWake)
			if n := l.launches(); n != 0 {
				t.Fatalf("%d launches for a missed slot", n)
			}
			js := readFileState(t, r.stateDir).Jobs["hourly"]
			if js.Missed != 1 || js.LastMissed == nil || !js.LastMissed.From.Equal(time.Date(2000, 1, 1, 1, 0, 0, 0, time.UTC)) {
				t.Errorf("missed = %d %+v", js.Missed, js.LastMissed)
			}
			if !strings.Contains(r.logs.String(), "missed its slots") || len(r.drain()) != 0 {
				t.Errorf("a missed slot is logged, with no event")
			}
		})
	})
	t.Run("catch-up keeps the slot on time", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := &fakeLauncher{}
			clk := &shiftedClock{}
			r := startOn(t, configureJobs(t, minuteJob, l), "", clk)
			defer r.stop(t)
			synctest.Wait()
			clk.move(2*time.Minute + 30*time.Second) // the loop wakes at 00:01 real time: wall 00:03:30
			tick(time.Minute)                        // 00:01 and 00:02 missed, 00:03 (30 s late) on time
			if n := l.launches(); n != 1 {
				t.Fatalf("%d launches after the catch-up", n)
			}
			id := r.audit.recorded()[0].CorrelationID
			if rec := readFileState(t, r.stateDir).Runs[id]; !rec.Scheduled.Equal(time.Date(2000, 1, 1, 0, 3, 0, 0, time.UTC)) {
				t.Errorf("ran the slot %s, want 00:03", rec.Scheduled)
			}
		})
	})
	t.Run("clock set back", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := &fakeLauncher{}
			clk := &shiftedClock{}
			r := startOn(t, configureJobs(t, hourlyJob, l), "", clk)
			defer r.stop(t)
			tick(time.Hour) // 01:00 runs
			if l.launches() != 1 {
				t.Fatalf("%d launches at 01:00", l.launches())
			}
			l.proc(0).end(executor.Exit{}, nil)
			synctest.Wait()
			clk.move(-30 * time.Minute) // wall back to 00:30: 01:00 comes again
			tick(time.Hour)             // wall 01:30
			if n := l.launches(); n != 1 {
				t.Errorf("%d launches: a slot ran twice after the clock was set back", n)
			}
		})
	})
}

// AC4 (Q6): under forbid a slot while a run is active is skipped and
// counted, the run goes on, and the next slot after it ended runs.
func TestJobForbidSkipsAndResumes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		r := start(t, configureJobs(t, minuteJob, l), "")
		defer r.stop(t)
		tick(time.Minute)
		tick(2 * time.Minute) // 00:02 and 00:03 skipped
		if n := l.launches(); n != 1 {
			t.Fatalf("%d launches while a run is active", n)
		}
		if js := readFileState(t, r.stateDir).Jobs["tick"]; js.Skipped != 2 {
			t.Errorf("skipped = %d", js.Skipped)
		}
		if len(l.proc(0).stopCalls()) != 0 {
			t.Error("forbid stopped the active run")
		}
		l.proc(0).end(executor.Exit{}, nil)
		tick(time.Minute)
		if n := l.launches(); n != 2 {
			t.Errorf("%d launches after the run ended", n)
		}
	})
}

// AC5: the timeout stops the run with its stop signal and stop timeout;
// job_timeout once; exit codes and signals give job_failed.
func TestJobResults(t *testing.T) {
	body := "version: 1\njobs:\n  - {name: slow, schedule: \"0 * * * *\", command: /bin/sh, timeout: 10s, stop_signal: INT, stop_timeout: 3s}\n"
	t.Run("timeout", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := &fakeLauncher{}
			r := start(t, configureJobs(t, body, l), "")
			defer r.stop(t)
			tick(time.Hour)
			tick(10 * time.Second)
			if calls := l.proc(0).stopCalls(); len(calls) != 1 || calls[0].sig != syscall.SIGINT || calls[0].timeout != 3*time.Second {
				t.Fatalf("stops = %+v", calls)
			}
			evs := r.drain()
			if got := eventsOf(evs, EventJobTimeout); len(got) != 1 || len(evs) != 1 {
				t.Errorf("events = %+v", evs)
			}
			if js := readFileState(t, r.stateDir).Jobs["slow"]; js.TimedOut != 1 || js.Last.Result != resultTimedOut {
				t.Errorf("state = %+v", js)
			}
		})
	})
	for _, tt := range []struct {
		name    string
		exit    executor.Exit
		failure string
	}{
		{"exit code", executor.Exit{Code: 3}, failureExitCode},
		{"signal", executor.Exit{Code: -1, Signal: "SIGKILL"}, failureSignal},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				l := &fakeLauncher{}
				r := start(t, configureJobs(t, body, l), "")
				defer r.stop(t)
				tick(time.Hour)
				l.proc(0).end(tt.exit, nil)
				synctest.Wait()
				evs := eventsOf(r.drain(), EventJobFailed)
				if len(evs) != 1 || evs[0].Attributes["failure"] != tt.failure {
					t.Errorf("events = %+v", evs)
				}
			})
		})
	}
	t.Run("launch failure", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := &fakeLauncher{launchErr: errFakeExec}
			r := start(t, configureJobs(t, body, l), "")
			defer r.stop(t)
			tick(time.Hour)
			evs := eventsOf(r.drain(), EventJobFailed)
			if len(evs) != 1 || evs[0].Attributes["failure"] != failureLaunch {
				t.Fatalf("events = %+v", evs)
			}
			if _, ran := evs[0].Attributes["started"]; ran {
				t.Errorf("a run that never started has a start: %v", evs[0].Attributes)
			}
			entries := r.audit.recorded()
			if len(entries) != 2 || entries[1].Result != audit.ResultFailed {
				t.Errorf("audit = %+v", entries)
			}
			if st := readFileState(t, r.stateDir); len(st.Runs) != 0 || st.Jobs["slow"].Failed != 1 {
				t.Errorf("state = %+v", st)
			}
		})
	})
}

const (
	runRunningID  = "0123456789abcdef0123456789abcdef"
	runReservedID = "fedcba9876543210fedcba9876543210"
	runPendingID  = "00112233445566778899aabbccddeeff"
)

// crashState is what a sentineld that died with runs in flight leaves: a
// running run of tick, a reserved run of a job since removed, and a run
// of tick whose group was not confirmed empty (reported already).
func crashState() string {
	g := func(pgid int) string {
		return fmt.Sprintf(`{"pgid": %d, "sid": 50, "start": 9, "boot": "%s"}`, pgid, testBoot)
	}
	return `{"services": {}, "runs": {` +
		`"` + runRunningID + `": {"job": "tick", "phase": "running", "scheduled": "1999-12-31T23:59:00Z", "started": "1999-12-31T23:59:01Z", "group": ` + g(4242) + `},` +
		`"` + runReservedID + `": {"job": "gone", "phase": "reserved", "scheduled": "1999-12-31T23:58:00Z"},` +
		`"` + runPendingID + `": {"job": "tick", "phase": "cleanup_pending", "scheduled": "1999-12-31T23:50:00Z", "started": "1999-12-31T23:50:01Z", "group": ` + g(4343) + `}}}`
}

// AC6 (Q14, review C3): runs a crash left are abandoned once, their
// groups killed; a run reported already is only cleaned up; records stay
// while their group survives, and keep the job's slots skipped.
func TestJobRunsLeftByACrash(t *testing.T) {
	t.Run("cleaned up", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			stateDir := filepath.Join(t.TempDir(), "state")
			writeStateFile(t, stateDir, crashState())
			l := &fakeLauncher{}
			r := start(t, configureJobs(t, minuteJob, l), stateDir)
			defer r.stop(t)
			synctest.Wait()
			if got := l.cleaned(); len(got) != 2 {
				t.Fatalf("cleanups = %+v", got)
			}
			evs := eventsOf(r.drain(), EventJobFailed)
			ids := map[string]bool{}
			for _, e := range evs {
				if e.Attributes["failure"] != failureAbandon {
					t.Errorf("event = %+v", e)
				}
				ids[e.CorrelationID] = true
			}
			if len(evs) != 2 || !ids[runRunningID] || !ids[runReservedID] {
				t.Errorf("abandoned events for %v, want the running and the reserved run", ids)
			}
			st := readFileState(t, stateDir)
			if len(st.Runs) != 0 || st.Jobs["tick"].Failed != 1 {
				t.Errorf("state = %+v", st)
			}
			if l.launches() != 0 {
				t.Error("a slot ran again after the crash")
			}
		})
	})
	t.Run("group survives", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			stateDir := filepath.Join(t.TempDir(), "state")
			writeStateFile(t, stateDir, crashState())
			l := &fakeLauncher{cleanupErr: executor.ErrCleanupIncomplete}
			r := start(t, configureJobs(t, minuteJob, l), stateDir)
			synctest.Wait()
			st := readFileState(t, stateDir)
			if rec := st.Runs[runRunningID]; rec.Phase != runCleanupPending {
				t.Errorf("running record = %+v, want it kept as cleanup_pending", rec)
			}
			if _, ok := st.Runs[runPendingID]; !ok {
				t.Error("the pending record was dropped although its group survives")
			}
			tick(time.Minute)
			if n := l.launches(); n != 0 {
				t.Errorf("%d launches beside a group that survives", n)
			}
			if js := readFileState(t, stateDir).Jobs["tick"]; js.Skipped != 1 {
				t.Errorf("skipped = %d", js.Skipped)
			}
			r.drain()
			r.stop(t)
			// Next start: the cleanup works, no second report.
			l2 := &fakeLauncher{}
			r2 := start(t, configureJobs(t, minuteJob, l2), stateDir)
			defer r2.stop(t)
			synctest.Wait()
			// Both records left were reported at the first start: they are
			// only cleaned up now.
			if evs := r2.drain(); len(evs) != 0 {
				t.Errorf("events after the second start = %+v", evs)
			}
			if st := readFileState(t, stateDir); len(st.Runs) != 0 {
				t.Errorf("runs left = %+v", st.Runs)
			}
			tick(time.Minute)
			if n := l2.launches(); n != 1 {
				t.Errorf("%d launches once the groups are gone", n)
			}
		})
	})
}

// AC9 (Q13): sentineld stopping stops the active runs: canceled.
func TestJobStopCancelsRuns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		r := start(t, configureJobs(t, minuteJob, l), "")
		tick(time.Minute)
		r.stop(t)
		if calls := l.proc(0).stopCalls(); len(calls) != 1 || calls[0].sig != syscall.SIGTERM {
			t.Errorf("stops = %+v", calls)
		}
		evs := eventsOf(r.drain(), EventJobFailed)
		if len(evs) != 1 || evs[0].Attributes["failure"] != failureCanceled {
			t.Errorf("events = %+v", evs)
		}
		if st := readFileState(t, r.stateDir); len(st.Runs) != 0 {
			t.Errorf("runs left = %+v", st.Runs)
		}
	})
}

// AC7 (Q-B): an intent the audit log refuses launches nothing; the slot
// is tried again while it is on time, then missed.
func TestJobAuditRefused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := configureJobs(t, hourlyJob, l)
		r := start(t, in, "")
		defer r.stop(t)
		r.audit.setFail(func(e module.AuditEntry) bool { return e.Result == audit.ResultIntent })
		tick(time.Hour)
		tick(20 * time.Second)
		if n := l.launches(); n != 0 {
			t.Fatalf("%d launches without an intent", n)
		}
		r.audit.setFail(nil)
		tick(jobRetry)
		if n := l.launches(); n != 1 {
			t.Fatalf("%d launches once the audit log works again within the minute", n)
		}
		l.proc(0).end(executor.Exit{}, nil)
		r.audit.setFail(func(e module.AuditEntry) bool { return e.Result == audit.ResultIntent })
		tick(time.Hour)
		tick(2 * time.Minute)
		r.audit.setFail(nil)
		tick(time.Minute)
		if n := l.launches(); n != 1 {
			t.Errorf("%d launches: a slot blocked past its minute ran late", n)
		}
		if js := readFileState(t, r.stateDir).Jobs["hourly"]; js.Missed != 1 {
			t.Errorf("missed = %d", js.Missed)
		}
		if len(eventsOf(r.drain(), EventJobFailed)) != 0 {
			t.Error("a blocked launch published an event (Q-B: none)")
		}
	})
}

// AC2: without the central switch, or with enabled: false, nothing runs.
func TestJobNotRunWhenNotArmedOrDisabled(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"not armed": {"sentinel.yaml": centralFile, "supervisor/a.yaml": minuteJob},
		"disabled":  {"sentinel.yaml": jobsCentral, "supervisor/a.yaml": strings.Replace(minuteJob, "    command:", "    enabled: false\n    command:", 1)},
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				l := &fakeLauncher{}
				r := start(t, mustConfigureProcs(t, files, l), "")
				defer r.stop(t)
				tick(10 * time.Minute)
				if n := l.launches(); n != 0 || len(r.audit.recorded()) != 0 {
					t.Errorf("%d launches, audit %+v", n, r.audit.recorded())
				}
			})
		})
	}
}

// jobFile is a supervisor file with one job per definition.
func jobFile(defs ...string) string {
	return "version: 1\njobs:\n" + strings.Join(defs, "")
}

func jobDef(name, schedule, arg, extra string) string {
	return fmt.Sprintf("  - {name: %s, schedule: %q, command: /bin/sh, args: [%s]%s}\n", name, schedule, arg, extra)
}

func reloadJobs(t *testing.T, r *rig, l *fakeLauncher, central, body string) error {
	t.Helper()
	return reconfigureTo(t, t.Context(), r, l, map[string]string{"sentinel.yaml": central, "supervisor/a.yaml": body})
}

// AC8 (Q7, Q12, Q13, Q16): a reload keeps unchanged jobs; a changed job's
// run goes on and blocks its next slot by name, then the new definition
// runs; a removed or disabled job schedules nothing more and its run
// ends by itself; disarming programs stops the runs; a time-zone change
// moves only the jobs that inherit it.
func TestJobReload(t *testing.T) {
	t.Run("unchanged and changed", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := &fakeLauncher{}
			in := configureJobs(t, jobFile(jobDef("a", "* * * * *", "a1", ""), jobDef("b", "0 * * * *", "b1", "")), l)
			r := start(t, in, "")
			defer r.stop(t)
			tick(time.Minute) // a1 runs
			keptB := in.jobByName["b"]
			if err := reloadJobs(t, r, l, jobsCentral, jobFile(jobDef("a", "* * * * *", "a2", ""), jobDef("b", "0 * * * *", "b1", ""))); err != nil {
				t.Fatal(err)
			}
			if in.jobByName["b"] != keptB {
				t.Error("an unchanged job was replaced")
			}
			tick(time.Minute) // 00:02: a1 still runs, the new a is skipped
			if n := len(launchesOf(l, "a2")); n != 0 || len(l.proc(0).stopCalls()) != 0 {
				t.Fatalf("a2 launched %d times beside a1, or a1 stopped", n)
			}
			l.proc(0).end(executor.Exit{}, nil)
			tick(time.Minute)
			if n := len(launchesOf(l, "a2")); n != 1 {
				t.Errorf("a2 launched %d times after a1 ended", n)
			}
			if evs := eventsOf(r.drain(), EventJobSucceeded); len(evs) != 1 || evs[0].Source != "a" {
				t.Errorf("events = %+v", evs)
			}
		})
	})
	t.Run("removed and disabled", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := &fakeLauncher{}
			in := configureJobs(t, jobFile(jobDef("a", "* * * * *", "a1", ""), jobDef("b", "* * * * *", "b1", "")), l)
			r := start(t, in, "")
			defer r.stop(t)
			tick(time.Minute)
			if err := reloadJobs(t, r, l, jobsCentral, jobFile(jobDef("b", "* * * * *", "b1", ", enabled: false"))); err != nil {
				t.Fatal(err)
			}
			for i := range 2 {
				l.proc(i).end(executor.Exit{}, nil)
			}
			tick(3 * time.Minute)
			if n := l.launches(); n != 2 {
				t.Errorf("%d launches after removal and disable", n)
			}
			for i := range 2 {
				if len(l.proc(i).stopCalls()) != 0 {
					t.Errorf("run %d was stopped by the reload (Q7: it finishes)", i)
				}
			}
			if evs := eventsOf(r.drain(), EventJobSucceeded); len(evs) != 2 {
				t.Errorf("a removed job's run did not report its end: %+v", evs)
			}
			if st := readFileState(t, r.stateDir); len(st.Runs) != 0 {
				t.Errorf("runs left = %+v", st.Runs)
			}
		})
	})
	t.Run("disarmed", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := &fakeLauncher{}
			r := start(t, configureJobs(t, minuteJob, l), "")
			defer r.stop(t)
			tick(time.Minute)
			unarmed := strings.Replace(jobsCentral, "    processes: true\n", "", 1)
			if err := reloadJobs(t, r, l, unarmed, minuteJob); err != nil {
				t.Fatal(err)
			}
			if len(l.proc(0).stopCalls()) != 1 {
				t.Error("disarming programs did not stop the run (Q13)")
			}
			tick(5 * time.Minute)
			if l.launches() != 1 {
				t.Errorf("%d launches while disarmed", l.launches())
			}
			if evs := eventsOf(r.drain(), EventJobFailed); len(evs) != 1 || evs[0].Attributes["failure"] != failureCanceled {
				t.Errorf("events = %+v", evs)
			}
		})
	})
	t.Run("time zone", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := &fakeLauncher{}
			body := jobFile(jobDef("own", "0 3 * * *", "o", ", timezone: Asia/Tokyo"), jobDef("inherits", "0 3 * * *", "i", ""))
			in := configureJobs(t, body, l)
			r := start(t, in, "")
			defer r.stop(t)
			own, inherits := in.jobByName["own"], in.jobByName["inherits"]
			rome := strings.Replace(jobsCentral, "timezone: UTC", "timezone: Europe/Rome", 1)
			if err := reloadJobs(t, r, l, rome, body); err != nil {
				t.Fatal(err)
			}
			if in.jobByName["own"] != own || in.jobByName["inherits"] == inherits {
				t.Error("a daemon time-zone change must move only the jobs that inherit it")
			}
			if loc := in.jobByName["inherits"].def.Location.String(); loc != "Europe/Rome" {
				t.Errorf("inherited zone %s", loc)
			}
			tick(2 * time.Hour) // 03:00 in Rome is 02:00 UTC (January)
			if n := len(launchesOf(l, "i")); n != 1 {
				t.Errorf("the inheriting job ran %d times by 02:00 UTC", n)
			}
		})
	})
}

// AC10 (Q9): list and status show jobs beside services; the operator's
// commands that do not apply to a job are refused and change nothing.
func TestJobCommands(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		body := "version: 1\nservices:\n  - {name: worker, command: /bin/sh}\n" + strings.TrimPrefix(minuteJob, "version: 1\n")
		in := configureJobs(t, body, l)
		r := start(t, in, "")
		defer r.stop(t)
		tick(time.Minute)
		lst := in.list()
		if len(lst.Services) != 1 || len(lst.Jobs) != 1 || lst.Jobs[0].Name != "tick" || lst.Jobs[0].Phase != phaseRunning ||
			lst.Jobs[0].Schedule != "* * * * *" || lst.Jobs[0].Timezone != "UTC" {
			t.Fatalf("list = %+v", lst)
		}
		st, apiErr := in.status("tick")
		if apiErr != nil || len(st.Services) != 0 || len(st.Jobs) != 1 {
			t.Fatalf("status tick = %+v %v", st, apiErr)
		}
		js := st.Jobs[0]
		if len(js.ActiveRuns) != 1 || js.ActiveRuns[0].Phase != runRunning || js.ActiveRuns[0].Pid == 0 || js.Runs != 1 ||
			!js.NextRun.Equal(time.Date(2000, 1, 1, 0, 2, 0, 0, time.UTC)) {
			t.Errorf("job status = %+v", js)
		}
		launches, entries := l.launches(), len(r.audit.recorded())
		for _, a := range []action{actStart, actStop, actRestart, actReset} {
			_, apiErr := in.act(t.Context(), "tick", a)
			if apiErr == nil || apiErr.Code != api.CodeUnsupported || !strings.Contains(apiErr.Message, "is a job") {
				t.Errorf("%s on a job: %+v", a, apiErr)
			}
		}
		if l.launches() != launches || len(r.audit.recorded()) != entries || len(l.proc(launches-1).stopCalls()) != 0 {
			t.Error("a refused command on a job had an effect")
		}
	})
}

// AC7 (Q-B): a reservation the state file refuses launches nothing; the
// intent is answered; the slot is tried again while on time.
func TestJobStateUnwritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes through a read-only directory")
	}
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := configureJobs(t, hourlyJob, l)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		dir := filepath.Join(r.stateDir, moduleName)
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		tick(time.Hour)
		if n := l.launches(); n != 0 {
			t.Fatalf("%d launches without a saved reservation", n)
		}
		if st, _ := in.status("hourly"); st.Jobs[0].Phase != phaseBlocked || st.Jobs[0].BlockedReason != blockedState {
			t.Errorf("status = %+v", st.Jobs[0])
		}
		e := r.audit.recorded()
		if last := e[len(e)-1]; last.Result != audit.ResultFailed || last.Detail["failure"] != blockedState {
			t.Errorf("intent left without its outcome: %+v", last)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		tick(jobRetry)
		if n := l.launches(); n != 1 {
			t.Errorf("%d launches once the state file can be written within the minute", n)
		}
	})
}

// C8: process group and run records share one cap; at the cap a slot
// launches nothing and the job shows blocked.
func TestJobRecordCap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := configureJobs(t, hourlyJob, l)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		in.mu.Lock()
		for i := range maxGroups {
			in.groups[fmt.Sprintf("gone%d", i)] = groupRecord{Pgid: 5000 + i, Sid: 50, Start: 9, Boot: testBoot}
		}
		in.mu.Unlock()
		l.cleanupGate = make(chan struct{}) // keep the stale records while the slot comes
		tick(time.Hour)
		if n := l.launches(); n != 0 {
			t.Errorf("%d launches at the record cap", n)
		}
		if st, _ := in.status("hourly"); st.Jobs[0].BlockedReason != blockedRecords {
			t.Errorf("status = %+v", st.Jobs[0])
		}
		close(l.cleanupGate)
	})
}

// The largest state file the jobs can make stays far below the state
// store's limit (16 MiB): every job with its last result and missed
// interval, and every record slot a run.
func TestJobStateSizeBound(t *testing.T) {
	at := time.Date(2026, 11, 3, 2, 30, 0, 0, time.UTC)
	f := fileState{Services: map[string]serviceState{}, Jobs: map[string]jobState{}, Runs: map[string]runRecord{}}
	name := func(prefix string, i int) string { return fmt.Sprintf("%s%062d", prefix, i) }
	for i := range maxServices {
		f.Jobs[name("j", i)] = jobState{Digest: "sha256:" + strings.Repeat("a", 64), Through: at, Runs: 1 << 62,
			LastMissed: &missedRecord{From: at, To: at}, Last: &runResult{RunID: strings.Repeat("b", 32), Scheduled: at,
				Started: at, Ended: at, Result: resultFailed, Failure: failureAbandon, Code: -1, Signal: "SIGKILL"}, Disabled: true, Idle: true}
	}
	for i := range maxGroups {
		f.Runs[fmt.Sprintf("%032x", i)] = runRecord{Job: name("j", i%maxServices), Phase: runCleanupPending, Scheduled: at, Started: at,
			Group: groupRecord{Pgid: maxPgid, Sid: maxPgid, Start: 1 << 62, Boot: testBoot}, Overlap: true}
	}
	raw, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > 2<<20 {
		t.Errorf("a full job state is %d bytes", len(raw))
	}
}

// Q11: the timeout's bounds are accepted.
func TestJobTimeoutBounds(t *testing.T) {
	for _, d := range []string{"1s", "24h"} {
		body := fmt.Sprintf("version: 1\njobs:\n  - {name: a, schedule: \"@daily\", timeout: %s, command: /bin/sh}\n", d)
		if _, err := configureProcs(t, map[string]string{"supervisor/a.yaml": body}, &fakeLauncher{}); err != nil {
			t.Errorf("timeout %s: %v", d, err)
		}
	}
}

// G7: a job replaced by a process service of the same name keeps its
// run; the service is not launched beside it, and starts once it ended.
func TestJobNameTakenByAService(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		r := start(t, configureJobs(t, jobFile(jobDef("x", "* * * * *", "job", "")), l), "")
		defer r.stop(t)
		tick(time.Minute)
		service := "version: 1\nservices:\n  - {name: x, command: /bin/sh, args: [svc]}\n"
		if err := reloadJobs(t, r, l, jobsCentral, service); err != nil {
			t.Fatal(err)
		}
		tick(2 * time.Second)
		if n := len(launchesOf(l, "svc")); n != 0 {
			t.Fatalf("the service was launched %d times beside the job's run", n)
		}
		l.proc(0).end(executor.Exit{}, nil)
		tick(2 * time.Minute) // the service's next attempt
		if n := len(launchesOf(l, "svc")); n != 1 {
			t.Errorf("the service was launched %d times after the run ended", n)
		}
	})
}

// Review 3c-4a R1: a run of a changed job's old definition ends into the
// new one's state without making the file invalid; counters carry over.
func TestJobResultAfterChangeKeepsStateValid(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		r := start(t, configureJobs(t, jobFile(jobDef("a", "* * * * *", "a1", "")), l), "")
		defer r.stop(t)
		tick(time.Minute)
		if err := reloadJobs(t, r, l, jobsCentral, jobFile(jobDef("a", "0 * * * *", "a2", ", timeout: 2h"))); err != nil {
			t.Fatal(err)
		}
		l.proc(0).end(executor.Exit{}, nil)
		synctest.Wait()
		if js := readFileState(t, r.stateDir).Jobs["a"]; js.Runs != 1 || js.Succeeded != 1 {
			t.Errorf("counters after the change = %+v", js)
		}
	})
}

// Review 3c-4a R2, R3: a removed job whose loop is held in a launch past
// the reload's deadline keeps its name; the launch, once back, is stopped
// at once; every reload says so until the loop has ended.
func TestJobLoopStuckInAReload(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gate := make(chan struct{})
		l := &fakeLauncher{gate: gate}
		r := start(t, configureJobs(t, jobFile(jobDef("a", "* * * * *", "a1", "")), l), "")
		defer r.stop(t)
		tick(time.Minute) // the launch is held
		next := map[string]string{"sentinel.yaml": jobsCentral, "supervisor/a.yaml": jobFile(jobDef("a", "* * * * *", "a2", ""))}
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		first := reconfigureTo(t, ctx, r, l, next)
		cancel()
		second := reconfigureTo(t, t.Context(), r, l, next)
		if first == nil || second == nil {
			t.Fatalf("reload errors = %v, %v: both must name the job still held", first, second)
		}
		close(gate)
		synctest.Wait()
		if calls := l.proc(0).stopCalls(); len(calls) != 1 {
			t.Errorf("the late launch of the old definition was stopped %d times, want 1", len(calls))
		}
		if err := reconfigureTo(t, t.Context(), r, l, next); err != nil {
			t.Fatalf("reload once the loop ended: %v", err)
		}
		tick(time.Minute)
		if n := len(launchesOf(l, "a2")); n != 1 {
			t.Errorf("the new definition launched %d times once the old loop ended", n)
		}
	})
}

// Review 3c-4a R5: disarming programs while a run ignores its stop
// reports it.
func TestJobDisarmReportsUnfinishedRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		r := start(t, configureJobs(t, minuteJob, l), "")
		defer r.stop(t)
		tick(time.Minute)
		p := l.proc(0)
		p.mu.Lock()
		p.stubborn = true
		p.mu.Unlock()
		unarmed := strings.Replace(jobsCentral, "    processes: true\n", "", 1)
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		err := reconfigureTo(t, ctx, r, l, map[string]string{"sentinel.yaml": unarmed, "supervisor/a.yaml": minuteJob})
		cancel()
		ctx2, cancel2 := context.WithTimeout(t.Context(), time.Second)
		second := reconfigureTo(t, ctx2, r, l, map[string]string{"sentinel.yaml": unarmed, "supervisor/a.yaml": minuteJob})
		cancel2()
		p.end(executor.Exit{}, nil)
		synctest.Wait()
		if err == nil || !strings.Contains(err.Error(), "did not stop") {
			t.Errorf("err = %v, want the unfinished run reported", err)
		}
		if second == nil {
			t.Error("a second disarmed reload succeeded while the run was alive (re-review R2)")
		}
	})
}

// Review 3c-4a R7 (risk): a job's route works from the first start.
func TestJobRouteAtStart(t *testing.T) {
	in := configureJobs(t, minuteJob, &fakeLauncher{})
	if got := in.NotificationChannels(model.Event{Source: "tick", Type: EventJobFailed}); !slices.Equal(got, []string{"ops"}) {
		t.Errorf("channels = %v, want [ops]", got)
	}
}

// Review 3c-4a R7 (code review): a clock set back while the launch is
// audited does not launch a slot that is no longer due.
func TestJobAdmissionWaitsForAFutureSlot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		clk := &shiftedClock{}
		r := startOn(t, configureJobs(t, hourlyJob, l), "", clk)
		defer r.stop(t)
		synctest.Wait()
		var once sync.Once
		r.audit.setFail(func(e module.AuditEntry) bool {
			if e.Action == actionJobRun && e.Result == audit.ResultIntent {
				once.Do(func() { clk.move(-30 * time.Minute) })
			}
			return false
		})
		tick(time.Hour) // wall 01:00, then 00:30 during the intent
		if n := l.launches(); n != 0 {
			t.Fatalf("%d launches of a slot 30 minutes ahead", n)
		}
		tick(30 * time.Minute) // wall 01:00 again
		if n := l.launches(); n != 1 {
			t.Errorf("%d launches once the slot is due", n)
		}
	})
}

// Review 3c-4a S7, T-01: a job enabled again starts from now, without a
// false catch-up; a clock set back and then an edit never repeat a slot.
func TestJobWatermarkAcrossEdits(t *testing.T) {
	t.Run("enabled again", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := &fakeLauncher{}
			off := jobFile(jobDef("a", "0 * * * *", "a", ", enabled: false"))
			r := start(t, configureJobs(t, off, l), "")
			defer r.stop(t)
			tick(3 * time.Hour)
			if err := reloadJobs(t, r, l, jobsCentral, jobFile(jobDef("a", "0 * * * *", "a", ""))); err != nil {
				t.Fatal(err)
			}
			tick(jobWake)
			if js := readFileState(t, r.stateDir).Jobs["a"]; js.Missed != 0 {
				t.Errorf("enabling a job counted %d catch-ups", js.Missed)
			}
		})
	})
	t.Run("clock back then edit", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := &fakeLauncher{}
			clk := &shiftedClock{}
			r := startOn(t, configureJobs(t, jobFile(jobDef("a", "0 * * * *", "a1", "")), l), "", clk)
			tick(time.Hour) // 01:00 runs
			l.proc(0).end(executor.Exit{}, nil)
			synctest.Wait()
			clk.move(-30 * time.Minute) // wall 00:30
			if err := reloadJobs(t, r, l, jobsCentral, jobFile(jobDef("a", "0 * * * *", "a1", ", timeout: 2h"))); err != nil {
				t.Fatal(err)
			}
			tick(time.Hour) // wall 01:30: 01:00 must not run again
			if n := l.launches(); n != 1 {
				t.Errorf("%d launches: the edit made a consumed slot run again", n)
			}
			r.stop(t)
		})
	})
}

// Review 3c-4a T-02, R6: a run whose group survives its end stays
// recorded, its job's slots skipped, until a later slot's cleanup works.
func TestJobCleanupFailsAtTheEnd(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		r := start(t, configureJobs(t, minuteJob, l), "")
		defer r.stop(t)
		tick(time.Minute)
		l.mu.Lock()
		l.cleanupErr = executor.ErrCleanupIncomplete
		l.mu.Unlock()
		l.proc(0).end(executor.Exit{}, nil)
		synctest.Wait()
		st := readFileState(t, r.stateDir)
		if len(st.Runs) != 1 {
			t.Fatalf("records = %+v, want the run kept", st.Runs)
		}
		for _, rec := range st.Runs {
			if rec.Phase != runCleanupPending {
				t.Errorf("record = %+v", rec)
			}
		}
		if evs := eventsOf(r.drain(), EventJobSucceeded); len(evs) != 1 {
			t.Errorf("the run's end was not reported once: %+v", evs)
		}
		tick(2 * time.Minute)
		if n := l.launches(); n != 1 {
			t.Fatalf("%d launches beside a surviving group", n)
		}
		l.mu.Lock()
		l.cleanupErr = nil
		l.mu.Unlock()
		tick(time.Minute)
		if n := l.launches(); n != 2 {
			t.Errorf("%d launches once the group is gone", n)
		}
		if c := strings.Count(r.logs.String(), "skips the slots of the job"); c != 1 {
			t.Errorf("the skips were logged %d times, want once per streak", c)
		}
	})
}

// Review 3c-4a T-03: 61 seconds late is missed.
func TestJobSixtyOneSecondsIsMissed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		clk := &shiftedClock{}
		r := startOn(t, configureJobs(t, hourlyJob, l), "", clk)
		defer r.stop(t)
		synctest.Wait()                   // the loop sleeps until the bubble's 00:01
		clk.move(time.Hour + time.Second) // it wakes at wall 01:01:01: the 01:00 slot is 61 s late
		tick(time.Minute)
		if n := l.launches(); n != 0 {
			t.Errorf("%d launches of a slot 61 s late", n)
		}
		if js := readFileState(t, r.stateDir).Jobs["hourly"]; js.Missed != 1 {
			t.Errorf("missed = %d", js.Missed)
		}
	})
}

// Review 3c-4a T-04: the values of a job's environment are masked in its
// output.
func TestJobOutputMasksItsEnvironment(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{output: []string{"key is literal-secret-42"}}
		r := start(t, configureJobs(t, minuteJob, l), "")
		defer r.stop(t)
		tick(time.Minute)
		l.proc(0).end(executor.Exit{}, nil)
		synctest.Wait()
		logs := r.logs.String()
		if strings.Contains(logs, "literal-secret-42") || !strings.Contains(logs, "job output") {
			t.Errorf("logs:\n%s", logs)
		}
	})
}

// Review 3c-4a S3: a fixed hour in the job's zone across Rome's spring gap
// runs once, at 03:00 CEST (D-096).
func TestJobDaylightSavingInItsZone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		clk := &shiftedClock{}
		body := jobFile(jobDef("a", "30 2 * * *", "a", ", timezone: Europe/Rome"))
		r := startOn(t, configureJobs(t, body, l), "", clk)
		defer r.stop(t)
		synctest.Wait()
		// 2000-03-26: Rome's clocks go from 02:00 to 03:00 at 01:00Z.
		clk.move(time.Date(2000, 3, 26, 0, 30, 0, 0, time.UTC).Sub(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)))
		tick(jobWake) // catch-up: the slots before are missed
		tick(30 * time.Minute)
		if n := l.launches(); n != 1 {
			t.Fatalf("%d launches across the gap", n)
		}
		id := r.audit.recorded()[0].CorrelationID
		if rec := readFileState(t, r.stateDir).Runs[id]; !rec.Scheduled.Equal(time.Date(2000, 3, 26, 1, 0, 0, 0, time.UTC)) {
			t.Errorf("slot %s, want 2000-03-26T01:00:00Z (03:00 CEST)", rec.Scheduled)
		}
	})
}

// Review 3c-4a T-08: a service replaced by a job of the same name: the
// job does not run while the service's process is still owned.
func TestJobNameTakenFromAService(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		service := "version: 1\nservices:\n  - {name: x, command: /bin/sh, args: [svc]}\n"
		r := start(t, mustConfigureProcs(t, map[string]string{"sentinel.yaml": jobsCentral, "supervisor/a.yaml": service}, l), "")
		defer r.stop(t)
		synctest.Wait()
		r.in.mu.Lock()
		r.in.groups["x"] = groupRecord{Pgid: 4242, Sid: 50, Start: 9, Boot: testBoot} // as if its stop left the group
		r.in.mu.Unlock()
		l.cleanupGate = make(chan struct{})
		if err := reloadJobs(t, r, l, jobsCentral, jobFile(jobDef("x", "* * * * *", "job", ""))); err != nil {
			t.Fatal(err)
		}
		tick(2 * time.Minute)
		if n := len(launchesOf(l, "job")); n != 0 {
			t.Errorf("the job launched %d times while the service's group is recorded", n)
		}
		close(l.cleanupGate)
	})
}

// Re-review 3c-4a R1: a run masks its own environment in its output
// whatever redaction is in use.
func TestJobOutputMasksItsOwnEnvironment(t *testing.T) {
	in := &instance{redact: func(s string) string { return s }}
	mask := withEnvMasked(in.redactor(), []string{"OLD_TOKEN=old-secret-value", "PATH=/bin"})
	if got := mask("print old-secret-value from /bin"); strings.Contains(got, "old-secret-value") || !strings.Contains(got, "/bin") {
		t.Errorf("masked = %q", got)
	}
}

// Re-review 3c-4a R3: an output error is logged also when the run was
// stopped (timeout).
func TestJobOutputErrorAtTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		body := "version: 1\njobs:\n  - {name: slow, schedule: \"0 * * * *\", command: /bin/sh, timeout: 10s}\n"
		r := start(t, configureJobs(t, body, l), "")
		defer r.stop(t)
		tick(time.Hour)
		p := l.proc(0)
		p.mu.Lock()
		p.outErr = io.ErrShortWrite
		p.mu.Unlock()
		tick(10 * time.Second)
		if !strings.Contains(r.logs.String(), "lost output of the job's run") {
			t.Errorf("logs:\n%s", r.logs.String())
		}
	})
}

// F9 (maintainer's option b): a watermark more than a day ahead of the
// clock (the clock was wrong forward) goes back to now at Start and at a
// reload; one less ahead is kept (a slot never runs twice), shown and
// logged once while the clock catches up.
func TestJobWatermarkAheadOfTheClock(t *testing.T) {
	stateWith := func(t *testing.T, through string) string {
		t.Helper()
		in := configureJobs(t, hourlyJob, &fakeLauncher{})
		return `{"services": {}, "jobs": {"hourly": {"digest": "` + in.jobByName["hourly"].digest + `", "through": "` + through + `"}}}`
	}
	t.Run("reset at start", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			stateDir := filepath.Join(t.TempDir(), "state")
			writeStateFile(t, stateDir, stateWith(t, "2031-01-01T00:00:00Z"))
			l := &fakeLauncher{}
			r := start(t, configureJobs(t, hourlyJob, l), stateDir)
			defer r.stop(t)
			tick(time.Hour)
			if n := l.launches(); n != 1 {
				t.Errorf("%d launches after a watermark years ahead", n)
			}
			if !strings.Contains(r.logs.String(), "reset the watermark") {
				t.Error("the reset is not logged")
			}
		})
	})
	t.Run("kept within a day", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			stateDir := filepath.Join(t.TempDir(), "state")
			writeStateFile(t, stateDir, stateWith(t, "2000-01-01T02:00:00Z"))
			l := &fakeLauncher{}
			in := configureJobs(t, hourlyJob, l)
			r := start(t, in, stateDir)
			defer r.stop(t)
			tick(time.Hour + 30*time.Minute) // 01:00 was consumed (watermark 02:00)
			if n := l.launches(); n != 0 {
				t.Fatalf("%d launches of a slot already consumed", n)
			}
			st, _ := in.status("hourly")
			if js := st.Jobs[0]; js.BlockedReason != blockedClockBehind || !js.Watermark.Equal(time.Date(2000, 1, 1, 2, 0, 0, 0, time.UTC)) {
				t.Errorf("status = %+v", js)
			}
			if c := strings.Count(r.logs.String(), "the clock is behind the slots"); c != 1 {
				t.Errorf("logged %d times, want once", c)
			}
			tick(time.Hour + 30*time.Minute) // 03:00 runs
			if n := l.launches(); n != 1 {
				t.Errorf("%d launches once the clock caught up", n)
			}
		})
	})
	t.Run("reset at reload", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := &fakeLauncher{}
			clk := &shiftedClock{}
			in := configureJobs(t, hourlyJob, l)
			r := startOn(t, in, "", clk)
			defer r.stop(t)
			synctest.Wait()
			clk.move(3 * 24 * time.Hour) // wrong forward: the catch-up moves the watermark
			tick(jobWake)                // (and the slot then exactly a minute late runs)
			if l.launches() > 0 {
				l.proc(0).end(executor.Exit{}, nil)
				synctest.Wait()
			}
			before := l.launches()
			clk.move(-3 * 24 * time.Hour) // NTP
			if err := reloadJobs(t, r, l, jobsCentral, hourlyJob); err != nil {
				t.Fatal(err)
			}
			tick(time.Hour)
			if n := l.launches() - before; n != 1 {
				t.Errorf("%d launches after the reload reset the watermark", n)
			}
		})
	})
}
