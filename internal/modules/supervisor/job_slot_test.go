package supervisor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/audit"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/clock"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/cronexpr"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/module"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/platform/executor"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/model"
)

// withRuns is a supervisor file with settings.max_concurrent_runs: n.
func withRuns(n int, defs ...string) string {
	return fmt.Sprintf("version: 1\nsettings: {max_concurrent_runs: %d}\njobs:\n%s", n, strings.Join(defs, ""))
}

// recordsOf returns the run records of the job name in the state file.
func recordsOf(t *testing.T, r *rig, name string) []runRecord {
	t.Helper()
	var out []runRecord
	for _, rec := range readFileState(t, r.stateDir).Runs {
		if rec.Job == name {
			out = append(out, rec)
		}
	}
	return out
}

const allowKey = ", concurrency_policy: allow"

// AC2 (Q4, Q5): under allow runs overlap; at the cap the slot is skipped
// and counted, one warning per streak, no event; status says why, and a
// later launch clears it.
func TestJobAllowOverlapsUpToTheCap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := configureJobs(t, withRuns(2, jobDef("a", "* * * * *", "a", allowKey)), l)
		r := start(t, in, "")
		defer r.stop(t)
		tick(2 * time.Minute)
		if n := l.launches(); n != 2 {
			t.Fatalf("%d launches, want 2 overlapping", n)
		}
		for i := range 2 {
			if len(l.proc(i).stopCalls()) != 0 {
				t.Errorf("allow stopped run %d", i)
			}
		}
		for _, rec := range recordsOf(t, r, "a") {
			if !rec.Overlap {
				t.Errorf("record = %+v, want it counted against the cap", rec)
			}
		}
		tick(2 * time.Minute) // 00:03 and 00:04 at the cap
		if n := l.launches(); n != 2 {
			t.Fatalf("%d launches at the cap", n)
		}
		if js := readFileState(t, r.stateDir).Jobs["a"]; js.Skipped != 2 {
			t.Errorf("skipped = %d", js.Skipped)
		}
		if c := strings.Count(r.logs.String(), "max_concurrent_runs"); c != 1 {
			t.Errorf("the cap was logged %d times, want once per streak", c)
		}
		if evs := r.drain(); len(evs) != 0 {
			t.Errorf("events at the cap = %+v", evs)
		}
		st, _ := in.status("a")
		if js := st.Jobs[0]; js.BlockedReason != blockedRunsFull || js.Phase != phaseRunning {
			t.Errorf("status = %+v, want running with reason %s", js, blockedRunsFull)
		}
		if st.MaxConcurrentRuns != 2 {
			t.Errorf("max_concurrent_runs = %d", st.MaxConcurrentRuns)
		}
		l.proc(0).end(executor.Exit{}, nil)
		tick(time.Minute)
		if n := l.launches(); n != 3 {
			t.Fatalf("%d launches once a run ended", n)
		}
		if st, _ := in.status("a"); st.Jobs[0].BlockedReason != "" {
			t.Errorf("reason after a launch = %q", st.Jobs[0].BlockedReason)
		}
	})
}

// AC2 (Q4, Q19): the cap counts only runs launched under allow, in the
// whole module: one allow job fills it for another; forbid still launches.
func TestJobAllowCapIsModuleWideAndOnlyForAllow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		body := withRuns(1, jobDef("a", "1 * * * *", "a", allowKey), jobDef("b", "2 * * * *", "b", allowKey), jobDef("c", "3 * * * *", "c", ""))
		in := configureJobs(t, body, l)
		r := start(t, in, "")
		defer r.stop(t)
		tick(3 * time.Minute)
		if a, b, c := len(launchesOf(l, "a")), len(launchesOf(l, "b")), len(launchesOf(l, "c")); a != 1 || b != 0 || c != 1 {
			t.Errorf("launches a %d, b %d, c %d: want b skipped at the cap, c launched", a, b, c)
		}
		if st, _ := in.status("c"); st.Jobs[0].BlockedReason != "" {
			t.Errorf("forbid job reason = %q", st.Jobs[0].BlockedReason)
		}
	})
}

// OE-1: two allow loops deciding one minute cannot both pass the cap, and
// the loser writes nothing in the audit log.
func TestJobAllowTwoLoopsOneSlotLeft(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		body := withRuns(1, jobDef("a", "* * * * *", "x", allowKey), jobDef("b", "* * * * *", "x", allowKey))
		r := start(t, configureJobs(t, body, l), "")
		defer r.stop(t)
		tick(time.Minute)
		if n := l.launches(); n != 1 {
			t.Fatalf("%d launches with one slot left", n)
		}
		st := readFileState(t, r.stateDir)
		if st.Jobs["a"].Skipped+st.Jobs["b"].Skipped != 1 {
			t.Errorf("skipped a %d, b %d", st.Jobs["a"].Skipped, st.Jobs["b"].Skipped)
		}
		if entries := r.audit.recorded(); len(entries) != 2 || entries[0].Result != audit.ResultIntent || entries[1].Result != audit.ResultOK {
			t.Errorf("audit = %+v, want the winner's intent and outcome only", entries)
		}
	})
}

// AC2 (Q16): under allow a record whose group survives does not block the
// launch, counts against the cap, and is cleaned again at each slot.
func TestJobAllowOverItsOwnPendingRecord(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		r := start(t, configureJobs(t, withRuns(2, jobDef("a", "* * * * *", "a", allowKey)), l), "")
		defer r.stop(t)
		tick(time.Minute)
		l.mu.Lock()
		l.cleanupErr = executor.ErrCleanupIncomplete
		l.mu.Unlock()
		l.proc(0).end(executor.Exit{}, nil)
		synctest.Wait()
		cleanups := len(l.cleaned())
		tick(time.Minute) // launches beside the pending record
		if n := l.launches(); n != 2 {
			t.Fatalf("%d launches beside a pending record", n)
		}
		if len(l.cleaned()) <= cleanups {
			t.Error("the pending record was not cleaned again at the slot")
		}
		tick(time.Minute) // record + run: the cap
		if n := l.launches(); n != 2 {
			t.Errorf("%d launches: the pending record does not count against the cap", n)
		}
	})
}

// scanLast is lastSlot's oracle: every slot from first on, by Next.
func scanLast(t *testing.T, s *cronexpr.Schedule, loc *time.Location, first, target time.Time) time.Time {
	t.Helper()
	last := first
	for {
		n, err := s.Next(last, loc)
		if err != nil || n.After(target) {
			return last
		}
		last = n
	}
}

// A9: the latest slot equals a forward scan, across DST gaps and repeats
// in a zone with a half-hour rule too; first is the answer when nothing
// follows it.
func TestLastSlot(t *testing.T) {
	utc := func(y int, m time.Month, d, h, mi int) time.Time { return time.Date(y, m, d, h, mi, 0, 0, time.UTC) }
	tests := []struct {
		name, expr, zone string
		first, target    time.Time
		want             time.Time
	}{
		{"hourly", "0 * * * *", "UTC", utc(2026, 1, 1, 1, 0), utc(2026, 1, 1, 5, 30), utc(2026, 1, 1, 5, 0)},
		{"target on a slot", "0 * * * *", "UTC", utc(2026, 1, 1, 1, 0), utc(2026, 1, 1, 5, 0), utc(2026, 1, 1, 5, 0)},
		{"nothing after first", "0 * * * *", "UTC", utc(2026, 1, 1, 1, 0), utc(2026, 1, 1, 1, 59), utc(2026, 1, 1, 1, 0)},
		{"target is first", "0 * * * *", "UTC", utc(2026, 1, 1, 1, 0), utc(2026, 1, 1, 1, 0), utc(2026, 1, 1, 1, 0)},
		{"months of minutes", "* * * * *", "UTC", utc(2026, 1, 1, 0, 0), utc(2026, 7, 1, 0, 0), utc(2026, 7, 1, 0, 0)},
		{"sparse", "0 0 29 2 *", "UTC", utc(2024, 2, 29, 0, 0), utc(2031, 1, 1, 0, 0), utc(2028, 2, 29, 0, 0)},
		{"spring gap", "30 2 * * *", "Europe/Rome", utc(2026, 3, 27, 1, 30), utc(2026, 3, 29, 3, 0), utc(2026, 3, 29, 1, 0)},
		{"autumn repeat", "30 * * * *", "Europe/Rome", utc(2026, 10, 25, 0, 30), utc(2026, 10, 25, 1, 45), utc(2026, 10, 25, 1, 30)},
		{"half hour rule", "15 2 * * *", "Australia/Lord_Howe", utc(2026, 4, 1, 15, 15), utc(2026, 4, 6, 0, 0), utc(2026, 4, 5, 15, 45)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := cronexpr.Parse(tt.expr)
			if err != nil {
				t.Fatal(err)
			}
			loc, err := time.LoadLocation(tt.zone)
			if err != nil {
				t.Fatal(err)
			}
			if got := lastSlot(s, loc, tt.first, tt.target); !got.Equal(tt.want) {
				t.Errorf("lastSlot = %s, want %s", got, tt.want)
			}
		})
	}
}

// FuzzLastSlot: whatever the schedule, zone and interval, lastSlot equals
// the forward scan, and Next never goes back as its argument grows (what
// the bisection rests on, review A9).
func FuzzLastSlot(f *testing.F) {
	f.Add("0 * * * *", "UTC", int64(0), uint32(600))
	f.Add("30 2 * * *", "Europe/Rome", int64(7_500_000), uint32(200_000))
	f.Add("*/7 1-3 * * *", "America/New_York", int64(26_000_000), uint32(500_000))
	f.Add("15 2 * * sun", "Australia/Lord_Howe", int64(8_000_000), uint32(1_000_000))
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	f.Fuzz(func(t *testing.T, expr, zone string, shift int64, span uint32) {
		s, err := cronexpr.Parse(expr)
		if err != nil || zone == "" || zone == "Local" {
			return
		}
		loc, err := time.LoadLocation(zone)
		if err != nil {
			return
		}
		after := base.Add(time.Duration(shift%(1<<27)) * time.Second)
		first, err := s.Next(after, loc)
		if err != nil {
			return
		}
		target := first.Add(time.Duration(span%(7*24*3600)) * time.Second)
		got := lastSlot(s, loc, first, target)
		if want := scanLast(t, s, loc, first, target); !got.Equal(want) {
			t.Fatalf("lastSlot(%s, %s, %s, %s) = %s, scan says %s", expr, zone, first, target, got, want)
		}
		n1, err1 := s.Next(first, loc)
		n2, err2 := s.Next(target, loc)
		if err1 == nil && err2 == nil && n2.Before(n1) {
			t.Fatalf("Next(%s) = %s is after Next(%s) = %s", first, n1, target, n2)
		}
	})
}

const runOnceKey = ", missed_runs: run_once"

func hourlyOnce(extra string) string {
	return jobFile(jobDef("h", "0 * * * *", "h", runOnceKey+extra))
}

// intents returns the audited intents of the job launches.
func intents(r *rig) []module.AuditEntry {
	var out []module.AuditEntry
	for _, e := range r.audit.recorded() {
		if e.Action == actionJobRun && e.Result == audit.ResultIntent {
			out = append(out, e)
		}
	}
	return out
}

// oneRunRecord returns the only run record of the job name.
func oneRunRecord(t *testing.T, r *rig, name string) runRecord {
	t.Helper()
	recs := recordsOf(t, r, name)
	if len(recs) != 1 {
		t.Fatalf("records of %s = %+v", name, recs)
	}
	return recs[0]
}

func clockAt(h, m int) time.Time { return time.Date(2000, 1, 1, h, m, 0, 0, time.UTC) }

// AC4 (Q6): after a forward jump exactly one run, of the latest missed
// slot, audited as a catch-up; missed counts once with the interval.
func TestJobRunOnceAfterAJump(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		clk := &shiftedClock{}
		r := startOn(t, configureJobs(t, hourlyOnce(""), l), "", clk)
		defer r.stop(t)
		synctest.Wait()
		clk.move(5*time.Hour + 9*time.Minute) // the loop wakes at wall 05:10
		tick(jobWake)
		if n := l.launches(); n != 1 {
			t.Fatalf("%d launches, want one catch-up", n)
		}
		if rec := oneRunRecord(t, r, "h"); !rec.Scheduled.Equal(clockAt(5, 0)) {
			t.Errorf("slot = %s, want the latest missed", rec.Scheduled)
		}
		for _, e := range r.audit.recorded() {
			if e.Detail["trigger"] != triggerCatchUp || e.Detail["scheduled"] != "2000-01-01T05:00:00Z" {
				t.Errorf("audit = %+v", e)
			}
		}
		js := readFileState(t, r.stateDir).Jobs["h"]
		if js.Missed != 1 || js.Runs != 1 || !js.Through.Equal(clockAt(5, 0)) || js.LastMissed == nil ||
			!js.LastMissed.From.Equal(clockAt(1, 0)) || !js.LastMissed.To.Equal(clockAt(5, 0)) {
			t.Errorf("state = %+v %+v", js, js.LastMissed)
		}
		l.proc(0).end(executor.Exit{}, nil)
		tick(49 * time.Minute) // 05:59: nothing more
		if n := l.launches(); n != 1 {
			t.Errorf("%d launches before the next slot", n)
		}
		tick(time.Minute)
		if n := l.launches(); n != 2 || intents(r)[1].Detail["trigger"] != triggerSchedule {
			t.Errorf("%d launches at the next slot, intents %+v", n, intents(r))
		}
	})
}

// downtimeState is what a sentineld stopped at 1999-12-20 left for job h
// of body, with extra in the file beside it.
func downtimeState(t *testing.T, body, extra string) string {
	t.Helper()
	in := configureJobs(t, body, &fakeLauncher{})
	return `{"services": {}, "jobs": {"h": {"digest": "` + in.jobByName["h"].digest + `", "through": "1999-12-20T00:00:00Z"}}` + extra + `}`
}

// AC4 (Q6): after a downtime of weeks, one run at Start; with no age limit.
func TestJobRunOnceAfterDowntime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stateDir := filepath.Join(t.TempDir(), "state")
		writeStateFile(t, stateDir, downtimeState(t, hourlyOnce(""), ""))
		l := &fakeLauncher{}
		r := startOn(t, configureJobs(t, hourlyOnce(""), l), stateDir, &shiftedClock{shift: 10 * time.Minute})
		defer r.stop(t)
		tick(time.Second) // wall 00:10: the slot of 00:00 is late
		if n := l.launches(); n != 1 {
			t.Fatalf("%d launches after the downtime", n)
		}
		if rec := oneRunRecord(t, r, "h"); !rec.Scheduled.Equal(clockAt(0, 0)) {
			t.Errorf("slot = %s", rec.Scheduled)
		}
		if got := intents(r)[0].Detail["trigger"]; got != triggerCatchUp {
			t.Errorf("trigger = %s", got)
		}
		js := readFileState(t, stateDir).Jobs["h"]
		if js.Missed != 1 || !js.LastMissed.From.Equal(time.Date(1999, 12, 20, 1, 0, 0, 0, time.UTC)) {
			t.Errorf("state = %+v", js)
		}
	})
}

// Review A1: with the clock at exactly a slot's minute, the slot a minute
// before is on time too: under every policy only the latest runs.
func TestJobRunOnceTwoSlotsOnTime(t *testing.T) {
	for _, policy := range []string{"", allowKey, ", concurrency_policy: replace"} {
		t.Run("policy"+policy, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				l := &fakeLauncher{}
				clk := &shiftedClock{}
				r := startOn(t, configureJobs(t, jobFile(jobDef("h", "* * * * *", "h", runOnceKey+policy)), l), "", clk)
				defer r.stop(t)
				synctest.Wait()
				clk.move(2 * time.Minute) // the loop wakes at wall 00:03:00 exactly
				tick(jobWake)
				if n := l.launches(); n != 1 {
					t.Fatalf("%d launches with two slots on time", n)
				}
				if rec := oneRunRecord(t, r, "h"); !rec.Scheduled.Equal(clockAt(0, 3)) {
					t.Errorf("slot = %s", rec.Scheduled)
				}
				if got := intents(r)[0].Detail["trigger"]; got != triggerSchedule {
					t.Errorf("trigger = %s: a slot on time is not a catch-up", got)
				}
				js := readFileState(t, r.stateDir).Jobs["h"]
				if js.Missed != 1 || !js.LastMissed.From.Equal(clockAt(0, 1)) || !js.LastMissed.To.Equal(clockAt(0, 3)) {
					t.Errorf("missed = %d %+v", js.Missed, js.LastMissed)
				}
			})
		})
	}
}

// AC4 (Q7): a catch-up blocked by a record of its name waits with the
// watermark in place and nothing counted, survives a restart without a
// second missed, and runs once the record is gone.
func TestJobRunOnceWaitSurvivesARestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stateDir := filepath.Join(t.TempDir(), "state")
		pending := `, "runs": {"` + runPendingID + `": {"job": "h", "phase": "cleanup_pending", "scheduled": "1999-12-19T00:00:00Z", ` +
			`"started": "1999-12-19T00:00:01Z", "group": {"pgid": 4343, "sid": 50, "start": 9, "boot": "` + testBoot + `"}}}`
		writeStateFile(t, stateDir, downtimeState(t, hourlyOnce(""), pending))
		l := &fakeLauncher{cleanupErr: executor.ErrCleanupIncomplete}
		in := configureJobs(t, hourlyOnce(""), l)
		r := startOn(t, in, stateDir, &shiftedClock{shift: 10 * time.Minute})
		tick(jobRetry + time.Second)
		if n := l.launches(); n != 0 {
			t.Fatalf("%d launches beside a surviving group", n)
		}
		js := readFileState(t, stateDir).Jobs["h"]
		if js.Missed != 0 || js.Skipped != 0 || !js.Through.Equal(time.Date(1999, 12, 20, 0, 0, 0, 0, time.UTC)) {
			t.Errorf("a waiting catch-up wrote %+v", js)
		}
		st, _ := in.status("h")
		if !st.Jobs[0].CatchUpPending {
			t.Errorf("status = %+v, want catch_up_pending", st.Jobs[0])
		}
		tick(3 * jobRetry)
		if c := strings.Count(r.logs.String(), "catch-up of the job waits"); c != 1 {
			t.Errorf("the wait was logged %d times, want once", c)
		}
		r.stop(t)

		l2 := &fakeLauncher{}
		r2 := startOn(t, configureJobs(t, hourlyOnce(""), l2), stateDir, &shiftedClock{shift: 10 * time.Minute})
		defer r2.stop(t)
		tick(time.Second)
		if n := l2.launches(); n != 1 {
			t.Fatalf("%d launches once the group is gone", n)
		}
		if js := readFileState(t, stateDir).Jobs["h"]; js.Missed != 1 {
			t.Errorf("missed = %d after the restart", js.Missed)
		}
		if st, _ := r2.in.status("h"); st.Jobs[0].CatchUpPending {
			t.Error("catch_up_pending after the catch-up ran")
		}
	})
}

// AC4 (Q7): under forbid a catch-up behind an active run waits; the next
// regular slot absorbs it (and is itself skipped while the run lasts).
func TestJobRunOnceAbsorbedByTheNextSlot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		clk := &shiftedClock{}
		r := startOn(t, configureJobs(t, hourlyOnce(""), l), "", clk)
		defer r.stop(t)
		tick(time.Hour) // 01:00 runs and stays
		clk.move(2*time.Hour + 30*time.Minute)
		tick(jobWake) // wall 03:31: 02:00 and 03:00 missed, the catch-up waits
		if n := l.launches(); n != 1 {
			t.Fatalf("%d launches beside the active run", n)
		}
		if js := readFileState(t, r.stateDir).Jobs["h"]; js.Missed != 0 {
			t.Fatalf("missed = %d while waiting", js.Missed)
		}
		tick(30 * time.Minute) // wall 04:01: 04:00 on time absorbs the catch-up
		tick(jobRetry)
		js := readFileState(t, r.stateDir).Jobs["h"]
		if js.Missed != 1 || js.Skipped != 1 || !js.Through.Equal(clockAt(4, 0)) || l.launches() != 1 {
			t.Errorf("state = %+v, %d launches", js, l.launches())
		}
		l.proc(0).end(executor.Exit{}, nil)
		tick(30 * time.Minute)
		if n := l.launches(); n != 1 {
			t.Errorf("%d launches: the absorbed catch-up ran late", n)
		}
	})
}

// AC4 (Q8): a catch-up the audit log refuses retries past a minute.
func TestJobRunOnceRetriesPastAMinute(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		clk := &shiftedClock{}
		r := startOn(t, configureJobs(t, hourlyOnce(""), l), "", clk)
		defer r.stop(t)
		synctest.Wait()
		r.audit.setFail(func(e module.AuditEntry) bool { return e.Result == audit.ResultIntent })
		clk.move(2*time.Hour + 9*time.Minute)
		tick(jobWake + 3*time.Minute)
		if n := l.launches(); n != 0 {
			t.Fatalf("%d launches without an intent", n)
		}
		if js := readFileState(t, r.stateDir).Jobs["h"]; js.Missed != 0 {
			t.Errorf("missed = %d while refused", js.Missed)
		}
		r.audit.setFail(nil)
		tick(jobRetry)
		if n := l.launches(); n != 1 {
			t.Errorf("%d launches once the audit log works", n)
		}
		if js := readFileState(t, r.stateDir).Jobs["h"]; js.Missed != 1 {
			t.Errorf("missed = %d", js.Missed)
		}
	})
}

// AC4 (Q8): under run_once a slot expiring during its admission becomes a
// catch-up: its intent is answered missed and one run follows, one missed.
func TestJobRunOnceExpiresDuringAdmission(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		clk := &shiftedClock{}
		r := startOn(t, configureJobs(t, hourlyOnce(""), l), "", clk)
		defer r.stop(t)
		synctest.Wait()
		var once sync.Once
		r.audit.setFail(func(e module.AuditEntry) bool {
			if e.Result == audit.ResultIntent {
				once.Do(func() { clk.move(2 * time.Minute) })
			}
			return false
		})
		tick(time.Hour)
		if n := l.launches(); n != 1 {
			t.Fatalf("%d launches", n)
		}
		entries := r.audit.recorded()
		if len(entries) != 4 || entries[1].Detail["failure"] != failureMissed || entries[2].Detail["trigger"] != triggerCatchUp {
			t.Errorf("audit = %+v", entries)
		}
		if js := readFileState(t, r.stateDir).Jobs["h"]; js.Missed != 1 || js.Runs != 1 {
			t.Errorf("state = %+v", js)
		}
	})
}

// Review A3: a catch-up whose next slot came due during its admission is
// absorbed by that slot; one run, one missed.
func TestJobRunOnceCatchUpCrossesTheNextSlot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		clk := &shiftedClock{}
		r := startOn(t, configureJobs(t, hourlyOnce(""), l), "", clk)
		defer r.stop(t)
		synctest.Wait()
		var once sync.Once
		r.audit.setFail(func(e module.AuditEntry) bool {
			if e.Result == audit.ResultIntent {
				once.Do(func() { clk.move(time.Minute) })
			}
			return false
		})
		clk.move(5*time.Hour + 58*time.Minute) // wakes at wall 05:59, 06:00 once audited
		tick(jobWake)
		if n := l.launches(); n != 1 {
			t.Fatalf("%d launches", n)
		}
		if rec := oneRunRecord(t, r, "h"); !rec.Scheduled.Equal(clockAt(6, 0)) {
			t.Errorf("slot = %s, want the regular one", rec.Scheduled)
		}
		if js := readFileState(t, r.stateDir).Jobs["h"]; js.Missed != 1 || js.Runs != 1 {
			t.Errorf("state = %+v", js)
		}
	})
}

// Decision question 5: an allow + run_once catch-up at a full cap waits;
// it is not lost, nothing is skipped.
func TestJobRunOnceCatchUpAtTheCap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		clk := &shiftedClock{}
		body := withRuns(1, jobDef("x", "0 1 * * *", "x", allowKey), jobDef("h", "30 * * * *", "h", allowKey+runOnceKey))
		r := startOn(t, configureJobs(t, body, l), "", clk)
		defer r.stop(t)
		tick(30*time.Minute + time.Second) // 00:30: h runs
		l.proc(0).end(executor.Exit{}, nil)
		tick(30 * time.Minute) // 01:00: x fills the cap
		clk.move(3 * time.Hour)
		tick(time.Minute) // wall 04:01: h's 01:30 to 03:30 missed
		if n := len(launchesOf(l, "h")); n != 1 {
			t.Fatalf("%d runs of h at a full cap", n)
		}
		js := readFileState(t, r.stateDir).Jobs["h"]
		if js.Missed != 0 || js.Skipped != 0 {
			t.Errorf("a waiting catch-up wrote %+v", js)
		}
		if st, _ := r.in.status("h"); !st.Jobs[0].CatchUpPending || st.Jobs[0].BlockedReason != blockedRunsFull {
			t.Errorf("status = %+v", st.Jobs[0])
		}
		l.proc(1).end(executor.Exit{}, nil)
		tick(jobRetry)
		if n := len(launchesOf(l, "h")); n != 2 {
			t.Errorf("%d runs of h once there is room", n)
		}
	})
}

// A run_once slot canceled before its launch stays due: a shutdown in that
// instant must not lose what run_once promises.
func TestJobRunOnceCanceledGivesTheSlotBack(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		clk := &shiftedClock{}
		in := configureJobs(t, hourlyOnce(""), l)
		r := startOn(t, in, "", clk)
		defer r.stop(t)
		synctest.Wait()
		var once sync.Once
		r.audit.setFail(func(e module.AuditEntry) bool {
			if e.Result == audit.ResultIntent {
				once.Do(func() {
					in.mu.Lock()
					in.runsCollected = true
					in.mu.Unlock()
				})
			}
			return false
		})
		clk.move(3*time.Hour + 9*time.Minute)
		tick(jobWake)
		if n := l.launches(); n != 0 {
			t.Fatalf("%d launches", n)
		}
		js := readFileState(t, r.stateDir).Jobs["h"]
		if js.Missed != 0 || js.Runs != 0 || js.Through.After(clockAt(0, 0)) {
			t.Errorf("state after a canceled catch-up = %+v", js)
		}
		if e := r.audit.recorded(); len(e) < 2 || e[1].Detail["failure"] != failureCanceled {
			t.Errorf("audit = %+v", e)
		}
	})
}

const (
	replaceKey = ", concurrency_policy: replace"
	dayTimeout = ", timeout: 24h" // an hourly run must not end at its timeout
)

// holdStops makes each Stop of p wait until releaseStops.
func holdStops(p *fakeProcess) {
	p.mu.Lock()
	p.held = true
	p.mu.Unlock()
}

func releaseStops(p *fakeProcess) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, h := range p.holds {
		close(h)
	}
	p.holds, p.held = nil, false
}

// failedOutcomes returns the audited outcomes of job launches that failed, by
// failure code.
func failedOutcomes(r *rig) []string {
	var out []string
	for _, e := range r.audit.recorded() {
		if e.Action == actionJobRun && e.Result == audit.ResultFailed {
			out = append(out, e.Detail["failure"])
		}
	}
	return out
}

// AC3 (Q1, Q3): replace goes intent with the old id, reservation saved,
// stop, end, launch, never two at once; the old run reports replaced.
func TestJobReplaceOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		r := start(t, configureJobs(t, jobFile(jobDef("a", "* * * * *", "a", replaceKey)), l), "")
		defer r.stop(t)
		tick(time.Minute)
		old := l.proc(0)
		oldID := intents(r)[0].CorrelationID
		holdStops(old)
		l.before = func(executor.Spec) {
			select {
			case <-old.Done():
			default:
				t.Error("the new run launched beside the old one")
			}
		}
		tick(time.Minute) // 00:02: the stop is held
		in := intents(r)
		if len(in) != 2 || in[1].Detail["replaces"] != oldID || in[1].Detail["replaces_count"] != "1" {
			t.Fatalf("intents = %+v", in)
		}
		if calls := old.stopCalls(); len(calls) != 1 {
			t.Fatalf("stops = %+v", calls)
		}
		if recs := recordsOf(t, r, "a"); len(recs) != 2 {
			t.Errorf("records during the stop = %+v, want the old run and the reservation", recs)
		}
		if n := l.launches(); n != 1 {
			t.Fatalf("%d launches during the stop", n)
		}
		releaseStops(old)
		synctest.Wait()
		if n := l.launches(); n != 2 {
			t.Fatalf("%d launches after the stop", n)
		}
		rec := oneRunRecord(t, r, "a")
		if !rec.Scheduled.Equal(clockAt(0, 2)) || rec.Phase != runRunning {
			t.Errorf("new run = %+v", rec)
		}
		evs := eventsOf(r.drain(), EventJobFailed)
		if len(evs) != 1 || evs[0].Attributes["failure"] != failureReplaced || evs[0].CorrelationID != oldID {
			t.Errorf("events = %+v", evs)
		}
	})
}

// AC3 (Q2, C1): nothing stops the old run for a launch that could not start:
// not an audit failure, not a state failure, not a cancellation or an
// expiry between the reservation and the first stop.
func TestJobReplaceNoStopWithoutALaunch(t *testing.T) {
	setup := func(t *testing.T) (*fakeLauncher, *shiftedClock, *rig) {
		t.Helper()
		l := &fakeLauncher{}
		clk := &shiftedClock{}
		r := startOn(t, configureJobs(t, jobFile(jobDef("a", "0 * * * *", "a", replaceKey+dayTimeout)), l), "", clk)
		tick(time.Hour)
		return l, clk, r
	}
	t.Run("audit refused", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l, _, r := setup(t)
			defer r.stop(t)
			r.audit.setFail(func(e module.AuditEntry) bool { return e.Result == audit.ResultIntent })
			tick(time.Hour)
			if c := l.proc(0).stopCalls(); len(c) != 0 || l.launches() != 1 {
				t.Errorf("stops %+v, %d launches", c, l.launches())
			}
		})
	})
	t.Run("state refused", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root writes through a read-only directory")
		}
		synctest.Test(t, func(t *testing.T) {
			l, _, r := setup(t)
			defer r.stop(t)
			dir := filepath.Join(r.stateDir, moduleName)
			if err := os.Chmod(dir, 0o500); err != nil {
				t.Fatal(err)
			}
			tick(time.Hour)
			if c := l.proc(0).stopCalls(); len(c) != 0 {
				t.Errorf("stops = %+v", c)
			}
			if err := os.Chmod(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			tick(jobRetry) // the state can be written again, within the minute
			if n := l.launches(); n != 2 {
				t.Errorf("%d launches", n)
			}
		})
	})
	t.Run("expired during the admission", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l, clk, r := setup(t)
			defer r.stop(t)
			var once sync.Once
			r.audit.setFail(func(e module.AuditEntry) bool {
				if e.Result == audit.ResultIntent {
					once.Do(func() { clk.move(2 * time.Minute) })
				}
				return false
			})
			tick(time.Hour)
			if c := l.proc(0).stopCalls(); len(c) != 0 {
				t.Errorf("stops = %+v", c)
			}
			if got := failedOutcomes(r); len(got) != 1 || got[0] != failureMissed {
				t.Errorf("outcomes = %v", got)
			}
		})
	})
	t.Run("canceled during the admission", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l, _, r := setup(t)
			defer r.stop(t)
			var once sync.Once
			r.audit.setFail(func(e module.AuditEntry) bool {
				if e.Result == audit.ResultIntent {
					once.Do(func() {
						r.in.mu.Lock()
						r.in.runsCollected = true
						r.in.mu.Unlock()
					})
				}
				return false
			})
			tick(time.Hour)
			if c := l.proc(0).stopCalls(); len(c) != 0 {
				t.Errorf("stops = %+v", c)
			}
		})
	})
}

// AC3 (Q1, Q15): a stop that takes minutes still launches the slot it
// began for; the slots passed meanwhile count once in skipped, none in
// missed, under both missed_runs; the loop goes on from the first after it.
func TestJobReplaceLongStop(t *testing.T) {
	for _, missed := range []string{"", runOnceKey} {
		t.Run("missed"+missed, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				l := &fakeLauncher{}
				r := start(t, configureJobs(t, jobFile(jobDef("a", "* * * * *", "a", replaceKey+missed+", stop_timeout: 5m")), l), "")
				defer r.stop(t)
				tick(time.Minute)
				old := l.proc(0)
				holdStops(old)
				tick(time.Minute)                    // 00:02: the stop begins
				tick(3*time.Minute + 30*time.Second) // 00:05:30
				releaseStops(old)
				synctest.Wait()
				if n := l.launches(); n != 2 {
					t.Fatalf("%d launches after a stop of 3 minutes", n)
				}
				if rec := oneRunRecord(t, r, "a"); !rec.Scheduled.Equal(clockAt(0, 2)) {
					t.Errorf("new run's slot = %s", rec.Scheduled)
				}
				js := readFileState(t, r.stateDir).Jobs["a"]
				if js.Skipped != 1 || js.Missed != 0 || !js.Through.Equal(clockAt(0, 5)) {
					t.Errorf("state = %+v", js)
				}
				if got := failedOutcomes(r); len(got) != 0 {
					t.Errorf("failed outcomes = %v", got)
				}
			})
		})
	}
}

// AC3 (Q1, Q17): an old group not confirmed empty launches nothing: the
// slot counts as skipped, the intent is answered, and no reservation stays.
func TestJobReplaceGroupSurvives(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		r := start(t, configureJobs(t, jobFile(jobDef("a", "0 * * * *", "a", replaceKey+dayTimeout)), l), "")
		defer r.stop(t)
		tick(time.Hour)
		l.mu.Lock()
		l.cleanupErr = executor.ErrCleanupIncomplete
		l.mu.Unlock()
		tick(time.Hour)
		if n := l.launches(); n != 1 {
			t.Fatalf("%d launches beside a surviving group", n)
		}
		if got := failedOutcomes(r); len(got) != 1 || got[0] != failureStopFailed {
			t.Errorf("outcomes = %v", got)
		}
		js := readFileState(t, r.stateDir).Jobs["a"]
		if js.Skipped != 1 || js.Runs != 1 || !js.Through.Equal(clockAt(2, 0)) {
			t.Errorf("state = %+v", js)
		}
		if rec := oneRunRecord(t, r, "a"); rec.Phase != runCleanupPending {
			t.Errorf("record = %+v", rec)
		}
		tick(time.Hour) // the record is held: no signal to anything, the slot skipped
		if js := readFileState(t, r.stateDir).Jobs["a"]; js.Skipped != 2 {
			t.Errorf("skipped = %d", js.Skipped)
		}
		l.mu.Lock()
		l.cleanupErr = nil
		l.mu.Unlock()
		tick(time.Hour)
		if n := l.launches(); n != 2 {
			t.Errorf("%d launches once the group is gone", n)
		}
	})
}

// A process that will not end: the wait is bounded, nothing launches, the
// run stays owned; a stop the module began ends the wait for nothing else.
func TestJobReplaceStuckProcess(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := configureJobs(t, jobFile(jobDef("a", "0 * * * *", "a", replaceKey)), l)
		r := start(t, in, "")
		tick(time.Hour)
		old := l.proc(0)
		holdStops(old)
		tick(time.Hour)
		tick(time.Minute) // past stop_timeout + margins
		if got := failedOutcomes(r); len(got) != 1 || got[0] != failureStopFailed {
			t.Errorf("outcomes = %v", got)
		}
		if n := l.launches(); n != 1 {
			t.Errorf("%d launches beside a process that did not end", n)
		}
		in.mu.Lock()
		owned := len(in.runs)
		in.mu.Unlock()
		if owned != 1 {
			t.Errorf("%d runs owned, want the stuck one", owned)
		}
		if rec := oneRunRecord(t, r, "a"); rec.Phase != runRunning {
			t.Errorf("record = %+v", rec)
		}
		releaseStops(old)
		synctest.Wait()
		r.stop(t)
	})
}

// A run that ended by itself before the slot is not replaced: nothing is
// stopped, nothing reported.
func TestJobReplaceRunEndedByItself(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		r := start(t, configureJobs(t, jobFile(jobDef("a", "0 * * * *", "a", replaceKey)), l), "")
		defer r.stop(t)
		tick(time.Hour)
		l.proc(0).end(executor.Exit{Code: 0}, nil)
		tick(time.Hour)
		if len(l.proc(0).stopCalls()) != 0 || l.launches() != 2 {
			t.Errorf("stops %+v, %d launches", l.proc(0).stopCalls(), l.launches())
		}
		if evs := eventsOf(r.drain(), EventJobFailed); len(evs) != 0 {
			t.Errorf("events = %+v", evs)
		}
	})
}

// Review C6: whichever select case wins, a process already ended keeps its
// result, for a stop and for a timeout alike.
func TestJobEndedProcessKeepsItsResult(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := configureJobs(t, hourlyJob, l)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		for range 40 {
			p := &fakeProcess{done: make(chan struct{})}
			p.end(executor.Exit{Code: 3}, nil)
			id := model.NewEventID()
			run := &jobRun{id: id, def: in.jobByName["hourly"].def, proc: p, stop: make(chan struct{}), done: make(chan struct{})}
			run.requestStopFor(failureReplaced)
			in.mu.Lock()
			in.runRecs[id] = runRecord{Job: "hourly", Phase: runRunning, Scheduled: clockAt(0, 0), Started: clockAt(0, 0)}
			in.runs[id] = run
			in.mu.Unlock()
			timer := clock.Real().NewTimer(0)
			time.Sleep(time.Millisecond) // the timer has fired too
			in.supervise(run, timer)
			if len(p.stopCalls()) != 0 {
				t.Fatalf("an ended process was stopped: %+v", p.stopCalls())
			}
			evs := r.drain()
			if len(evs) != 1 || evs[0].Attributes["failure"] != failureExitCode {
				t.Fatalf("events = %+v", evs)
			}
		}
	})
}

// Q11: a policy change to replace stops the old runs each with its own
// stop_signal; O2: more than 15 are listed within the audit's bound.
func TestJobReplaceAfterAllow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		body := func(key, signal string) string {
			return withRuns(32, jobDef("a", "* * * * *", "a", key+", stop_signal: "+signal))
		}
		r := start(t, configureJobs(t, body(allowKey, "INT"), l), "")
		defer r.stop(t)
		tick(20 * time.Minute)
		if n := l.launches(); n != 20 {
			t.Fatalf("%d runs", n)
		}
		if err := reloadJobs(t, r, l, jobsCentral, body(replaceKey, "HUP")); err != nil {
			t.Fatal(err)
		}
		tick(time.Minute)
		if n := l.launches(); n != 21 {
			t.Fatalf("%d launches after the stops", n)
		}
		for i := range 20 {
			if c := l.proc(i).stopCalls(); len(c) != 1 || c[0].sig != syscall.SIGINT {
				t.Errorf("run %d: stops = %+v, want its own stop_signal", i, c)
			}
		}
		in := intents(r)
		last := in[len(in)-1].Detail
		if last["replaces_count"] != "20" || strings.Count(last["replaces"], ",") != maxReplacesListed-1 || len(last["replaces"]) > 512 /* the audit log's bound on a value */ {
			t.Errorf("detail = %v", last)
		}
	})
}

// Q20, C4: a reload ending the loop during the stop launches nothing; one
// outcome, canceled over a stop failure; no stranded reservation; the old
// run ends replaced.
func TestJobReplaceCanceledDuringTheStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		r := start(t, configureJobs(t, jobFile(jobDef("a", "* * * * *", "a", replaceKey)), l), "")
		defer r.stop(t)
		tick(time.Minute)
		old := l.proc(0)
		holdStops(old)
		tick(time.Minute)
		if err := reloadJobs(t, r, l, jobsCentral, jobFile(jobDef("a", "0 * * * *", "b", replaceKey))); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if got := failedOutcomes(r); len(got) != 1 || got[0] != failureCanceled {
			t.Errorf("outcomes = %v", got)
		}
		if recs := recordsOf(t, r, "a"); len(recs) != 1 {
			t.Errorf("records = %+v: a reservation stayed", recs)
		}
		releaseStops(old)
		synctest.Wait()
		if n := l.launches(); n != 1 {
			t.Errorf("%d launches", n)
		}
		evs := eventsOf(r.drain(), EventJobFailed)
		if len(evs) != 1 || evs[0].Attributes["failure"] != failureReplaced {
			t.Errorf("events = %+v", evs)
		}
	})
}

// Q7: under replace a catch-up stops the active run too, then runs.
func TestJobReplaceCatchUp(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		clk := &shiftedClock{}
		r := startOn(t, configureJobs(t, hourlyOnce(replaceKey), l), "", clk)
		defer r.stop(t)
		tick(time.Hour)
		clk.move(3*time.Hour + 9*time.Minute)
		tick(jobWake)
		if n := l.launches(); n != 2 {
			t.Fatalf("%d launches", n)
		}
		if got := intents(r)[1].Detail; got["trigger"] != triggerCatchUp || got["replaces_count"] != "1" {
			t.Errorf("intent = %v", got)
		}
		if js := readFileState(t, r.stateDir).Jobs["h"]; js.Missed != 1 || js.Skipped != 0 {
			t.Errorf("state = %+v", js)
		}
	})
}

// C8, O3: replace touches only runs of its own name in this instance:
// another job's run stays, and a record with no process here (a crash's
// orphan) is not signalled; the slot is skipped.
func TestJobReplaceTouchesOnlyItsOwnRuns(t *testing.T) {
	t.Run("another job", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := &fakeLauncher{}
			body := jobFile(jobDef("a", "0 * * * *", "a", replaceKey), jobDef("b", "30 * * * *", "b", dayTimeout))
			r := start(t, configureJobs(t, body, l), "")
			defer r.stop(t)
			tick(time.Hour)
			tick(time.Hour)
			b := launchesOf(l, "b")[0]
			if len(l.proc(b).stopCalls()) != 0 {
				t.Errorf("a's replace stopped b: %+v", l.proc(b).stopCalls())
			}
			if n := len(launchesOf(l, "a")); n != 2 {
				t.Errorf("a ran %d times", n)
			}
		})
	})
	t.Run("an orphan", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			stateDir := filepath.Join(t.TempDir(), "state")
			body := jobFile(jobDef("h", "0 * * * *", "h", replaceKey))
			orphan := `, "runs": {"` + runRunningID + `": {"job": "h", "phase": "running", "scheduled": "1999-12-19T00:00:00Z", ` +
				`"started": "1999-12-19T00:00:01Z", "group": {"pgid": 4343, "sid": 50, "start": 9, "boot": "` + testBoot + `"}}}`
			writeStateFile(t, stateDir, downtimeState(t, body, orphan))
			l := &fakeLauncher{cleanupGate: make(chan struct{})} // the orphan's cleanup does not end
			in := configureJobs(t, body, l)
			r := start(t, in, stateDir)
			defer r.stop(t)
			tick(time.Hour)
			if n := l.launches(); n != 0 {
				t.Errorf("%d launches beside an orphan", n)
			}
			if entries := intents(r); len(entries) != 0 {
				t.Errorf("intents beside an orphan = %+v", entries)
			}
			close(l.cleanupGate)
		})
	})
}

// AC7 (Q21): a crash during a replace leaves both records: both end
// abandoned, the slot stays consumed, and nothing runs for it, even under
// run_once.
func TestJobCrashDuringReplace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stateDir := filepath.Join(t.TempDir(), "state")
		body := hourlyOnce(replaceKey)
		in := configureJobs(t, body, &fakeLauncher{})
		writeStateFile(t, stateDir, `{"services": {}, "jobs": {"h": {"digest": "`+in.jobByName["h"].digest+`", "through": "2000-01-01T00:00:00Z", "runs": 2}}, "runs": {`+
			`"`+runRunningID+`": {"job": "h", "phase": "running", "scheduled": "1999-12-31T23:00:00Z", "started": "1999-12-31T23:00:01Z", `+
			`"group": {"pgid": 4343, "sid": 50, "start": 9, "boot": "`+testBoot+`"}},`+
			`"`+runReservedID+`": {"job": "h", "phase": "reserved", "scheduled": "2000-01-01T00:00:00Z"}}}`)
		l := &fakeLauncher{}
		r := start(t, configureJobs(t, body, l), stateDir)
		defer r.stop(t)
		tick(59 * time.Minute)
		if n := l.launches(); n != 0 {
			t.Errorf("%d launches: the crashed slot ran again", n)
		}
		evs := eventsOf(r.drain(), EventJobFailed)
		if len(evs) != 2 || evs[0].Attributes["failure"] != failureAbandon || evs[1].Attributes["failure"] != failureAbandon {
			t.Errorf("events = %+v", evs)
		}
		if st := readFileState(t, stateDir); len(st.Runs) != 0 || st.Jobs["h"].Missed != 0 {
			t.Errorf("state = %+v", st)
		}
	})
}

// Panel F1: a run_once launch canceled before it is queued stays due: its
// slot comes back, across a restart too.
func TestJobRunOnceCanceledLaunchStaysDue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{gate: make(chan struct{}), honorCtx: true}
		clk := &shiftedClock{}
		r := startOn(t, configureJobs(t, hourlyOnce(""), l), "", clk)
		synctest.Wait()
		clk.move(3*time.Hour + 9*time.Minute)
		tick(jobWake) // the catch-up is reserved, its launch held
		r.stop(t)
		l2 := &fakeLauncher{}
		r2 := startOn(t, configureJobs(t, hourlyOnce(""), l2), r.stateDir, clk)
		defer r2.stop(t)
		synctest.Wait()
		if n := l2.launches(); n != 1 {
			t.Errorf("%d launches after the restart, want the catch-up", n)
		}
		if js := readFileState(t, r.stateDir).Jobs["h"]; js.Missed != 1 {
			t.Errorf("missed = %d", js.Missed)
		}
	})
}

// Panel F3: an admission of a loop a reload has ended reserves nothing: its
// increment would reach the old job while the undo reaches the new one.
func TestJobEndedLoopReservesNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := configureJobs(t, jobFile(jobDef("a", "0 * * * *", "a", "")), l)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		old := in.jobByName["a"]
		in.mu.Lock()
		old.st.Runs = 1
		in.mu.Unlock()
		if err := reloadJobs(t, r, l, jobsCentral, jobFile(jobDef("a", "0 * * * *", "a2", ""))); err != nil {
			t.Fatal(err)
		}
		a := &runAdmission{def: old.def, id: model.NewEventID(), slot: clockAt(1, 0), trigger: triggerSchedule, action: slotLaunch}
		if in.reserveRun(t.Context(), old, a) {
			t.Fatal("a loop already ended reserved a run")
		}
		st := readFileState(t, r.stateDir)
		if st.Jobs["a"].Runs != 1 || len(st.Runs) != 0 {
			t.Errorf("state = %+v", st)
		}
		if got := failedOutcomes(r); len(got) != 1 || got[0] != failureCanceled {
			t.Errorf("outcomes = %v", got)
		}
	})
}

// Panel F4: run times are not always a minute apart: an offset change with
// seconds brings two within 56 seconds.
func TestLastSlotAcrossASecondsOffset(t *testing.T) {
	s, err := cronexpr.Parse("* * * * *")
	if err != nil {
		t.Fatal(err)
	}
	loc, err := time.LoadLocation("Europe/Rome")
	if err != nil {
		t.Fatal(err)
	}
	first := time.Date(1893, 10, 31, 22, 59, 4, 0, time.UTC)
	want := time.Date(1893, 10, 31, 23, 0, 0, 0, time.UTC)
	if got := lastSlot(s, loc, first, want); !got.Equal(want) {
		t.Errorf("lastSlot = %s, want %s", got, want)
	}
}

// Panel F5: the last look before a launch: a loop's job ended since recheck
// (a disable or a reload saving meanwhile) launches nothing.
func TestJobLaunchLooksOnceMore(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := configureJobs(t, hourlyJob, l)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		j := in.jobByName["hourly"]
		a := &runAdmission{def: j.def, id: model.NewEventID(), slot: clockAt(0, 0), trigger: triggerSchedule, action: slotLaunch}
		in.mu.Lock()
		in.runRecs[a.id] = runRecord{Job: "hourly", Phase: runReserved, Scheduled: a.slot}
		j.ending = true
		in.mu.Unlock()
		in.launchRun(t.Context(), j, a)
		if n := l.launches(); n != 0 {
			t.Errorf("%d launches after the job was ended", n)
		}
		if got := failedOutcomes(r); len(got) != 1 || got[0] != failureCanceled {
			t.Errorf("outcomes = %v", got)
		}
		if st := readFileState(t, r.stateDir); len(st.Runs) != 0 {
			t.Errorf("records = %+v", st.Runs)
		}
	})
}

// Panel F6, T-04: a place claimed under the cap comes back when the
// admission fails, by an error or a panic: it is not lost until a restart.
func TestJobAllowClaimIsReleased(t *testing.T) {
	body := withRuns(1, jobDef("a", "0 * * * *", "a", allowKey), jobDef("b", "30 * * * *", "b", allowKey))
	t.Run("audit refused", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := &fakeLauncher{}
			r := start(t, configureJobs(t, jobFile(jobDef("a", "0 * * * *", "a", allowKey)), l), "")
			defer r.stop(t)
			synctest.Wait()
			r.audit.setFail(func(e module.AuditEntry) bool { return e.Result == audit.ResultIntent })
			tick(time.Hour)
			r.audit.setFail(nil)
			r.in.mu.Lock()
			reserved := r.in.overlapReserved
			r.in.mu.Unlock()
			if reserved != 0 {
				t.Errorf("%d places left claimed", reserved)
			}
			tick(jobRetry)
			if n := l.launches(); n != 1 {
				t.Errorf("%d launches once the audit log works", n)
			}
		})
	})
	t.Run("panic", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := &fakeLauncher{}
			r := start(t, configureJobs(t, body, l), "")
			defer r.stop(t)
			synctest.Wait()
			var once sync.Once
			r.audit.setFail(func(e module.AuditEntry) bool {
				if e.Target == "a" && e.Result == audit.ResultIntent {
					once.Do(func() { panic("audit") })
				}
				return false
			})
			tick(30 * time.Minute) // 00:30: b runs
			l.proc(0).end(executor.Exit{}, nil)
			tick(30 * time.Minute) // 01:00: a's loop panics
			if !strings.Contains(r.logs.String(), "job loop failed") {
				t.Fatalf("no panic in the loop:\n%s", r.logs.String())
			}
			tick(30 * time.Minute) // 01:30: b again
			if n := len(launchesOf(l, "b")); n != 2 {
				t.Errorf("b ran %d times: the panic kept a place under the cap", n)
			}
		})
	})
}

// Panel F9 (C8, Q18): under every policy a name held by a service's process
// or a stuck loop skips the slot; nothing is launched or signalled.
func TestJobNameHeld(t *testing.T) {
	holds := map[string]func(in *instance){
		"a service's process": func(in *instance) {
			in.groups["a"] = groupRecord{Pgid: 4242, Sid: 50, Start: 9, Boot: testBoot}
		},
		"a stuck loop": func(in *instance) {
			in.retiredJobs["a"] = &job{done: make(chan struct{})}
		},
	}
	for name, hold := range holds {
		for _, key := range []string{"", allowKey, replaceKey} {
			t.Run(name+key, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					l := &fakeLauncher{}
					in := configureJobs(t, jobFile(jobDef("a", "0 * * * *", "a", key+dayTimeout)), l)
					r := start(t, in, "")
					defer r.stop(t)
					tick(time.Hour)
					in.mu.Lock()
					hold(in)
					in.mu.Unlock()
					tick(time.Hour)
					if n, c := l.launches(), l.proc(0).stopCalls(); n != 1 || len(c) != 0 {
						t.Errorf("%d launches, stops %+v", n, c)
					}
					if js := readFileState(t, r.stateDir).Jobs["a"]; js.Skipped != 1 {
						t.Errorf("skipped = %d", js.Skipped)
					}
					if c := len(intents(r)); c != 1 {
						t.Errorf("%d intents", c)
					}
					in.mu.Lock()
					delete(in.groups, "a")
					delete(in.retiredJobs, "a")
					in.mu.Unlock()
				})
			})
		}
	}
}

// Panel F15 (C7): under run_once, a slot exactly a minute late is on time; a
// second later it is a catch-up.
func TestJobRunOnceOnTimeBoundary(t *testing.T) {
	for _, tt := range []struct {
		name    string
		late    time.Duration
		trigger string
	}{
		{"exactly a minute", 0, triggerSchedule},
		{"a second past", time.Second, triggerCatchUp},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				l := &fakeLauncher{}
				clk := &shiftedClock{}
				r := startOn(t, configureJobs(t, hourlyOnce(""), l), "", clk)
				defer r.stop(t)
				synctest.Wait()               // the loop sleeps until the bubble's 00:01
				clk.move(time.Hour + tt.late) // it wakes at wall 01:01 + late
				tick(jobWake)
				if n := l.launches(); n != 1 {
					t.Fatalf("%d launches", n)
				}
				if got := intents(r)[0].Detail["trigger"]; got != tt.trigger {
					t.Errorf("trigger = %s", got)
				}
			})
		})
	}
}

// Q15, Q20: once a stop began the slot stays consumed, under run_once too:
// canceled there, it is not caught up later.
func TestJobRunOnceCanceledAfterTheStopBegan(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		r := start(t, configureJobs(t, jobFile(jobDef("a", "* * * * *", "a", replaceKey+runOnceKey)), l), "")
		defer r.stop(t)
		tick(time.Minute)
		old := l.proc(0)
		holdStops(old)
		tick(time.Minute) // 00:02: the stop is held
		if err := reloadJobs(t, r, l, jobsCentral, jobFile(jobDef("a", "* * * * *", "a", replaceKey+runOnceKey+", enabled: false"))); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if got := failedOutcomes(r); len(got) != 1 || got[0] != failureCanceled {
			t.Errorf("outcomes = %v", got)
		}
		if js := readFileState(t, r.stateDir).Jobs["a"]; !js.Through.Equal(clockAt(0, 2)) || js.Runs != 1 {
			t.Errorf("state = %+v: a slot whose stop began went back", js)
		}
		releaseStops(old)
		synctest.Wait()
	})
}

// Review C7, A1: under run_once with older slots missed, a latest slot
// exactly a minute late is still on time and stands for them.
func TestJobRunOnceLatestExactlyAMinuteLate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		clk := &shiftedClock{}
		r := startOn(t, configureJobs(t, jobFile(jobDef("h", "*/5 * * * *", "h", runOnceKey)), l), "", clk)
		defer r.stop(t)
		synctest.Wait()
		clk.move(10 * time.Minute) // wakes at wall 00:11:00: 00:05 late, 00:10 exactly a minute late
		tick(jobWake)
		if n := l.launches(); n != 1 {
			t.Fatalf("%d launches", n)
		}
		if got := intents(r)[0].Detail; got["trigger"] != triggerSchedule || got["scheduled"] != "2000-01-01T00:10:00Z" {
			t.Errorf("intent = %v", got)
		}
	})
}

// Re-review R1: only an error of the context itself shows the program never
// ran. Another error racing with a cancellation may follow a start (the
// executor kills a child whose group it cannot read): the slot stays
// consumed and the failure is reported.
func TestJobCanceledLaunchErrorKeepsTheSlot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{launchErr: fmt.Errorf("read the new process: %w", errFakeExec)}
		clk := &shiftedClock{}
		in := configureJobs(t, hourlyOnce(""), l)
		l.before = func(executor.Spec) { in.cancel() }
		r := startOn(t, in, "", clk)
		synctest.Wait()
		clk.move(3*time.Hour + 9*time.Minute)
		tick(jobWake)
		r.stop(t)
		if js := readFileState(t, r.stateDir).Jobs["h"]; !js.Through.Equal(clockAt(3, 0)) || js.Failed != 1 {
			t.Errorf("state = %+v", js)
		}
		evs := eventsOf(r.drain(), EventJobFailed)
		if len(evs) != 1 || evs[0].Attributes["failure"] != failureLaunch {
			t.Errorf("events = %+v", evs)
		}
	})
}
