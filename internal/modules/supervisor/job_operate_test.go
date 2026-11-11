package supervisor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/audit"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/module"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/platform/executor"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/api"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/model"
)

func actOK(t *testing.T, in *instance, name string, a action) api.SupervisorActionResult {
	t.Helper()
	out, apiErr := in.act(t.Context(), name, a)
	if apiErr != nil {
		t.Fatalf("%s %s: %+v", a, name, apiErr)
	}
	return out.(api.SupervisorActionResult)
}

// actionsAudited returns the results of the operator's action a in the
// audit log.
func actionsAudited(r *rig, a action) []audit.Result {
	var out []audit.Result
	for _, e := range r.audit.recorded() {
		if e.Action == moduleName+"."+string(a) {
			out = append(out, e.Result)
		}
	}
	return out
}

// B-045: an action gives its slot back before it answers, so the next
// command, sent as soon as the answer arrives, is never refused as "in
// progress". act is called directly: doCtx's synctest.Wait would let the
// action's goroutine finish first and hide the window. That window is a few
// instructions wide, so the test repeats the pair; the race detector's
// scheduling makes it fail reliably (task check's race step), a plain run
// only sometimes.
func TestActionFreesItsSlotBeforeItAnswers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := configureJobs(t, jobFile(jobDef("a", "0 * * * *", "a", dayTimeout)), &fakeLauncher{})
		r := start(t, in, "")
		defer r.stop(t)
		for i := range 200 {
			for _, a := range []action{actDisable, actEnable} {
				if _, apiErr := in.act(t.Context(), "a", a); apiErr != nil {
					t.Fatalf("pair %d, %s: %+v", i, a, apiErr)
				}
			}
		}
	})
}

// AC5 (Q10): disable lets the active run finish and starts nothing more;
// it is shown and audited; enable starts from now, no catch-up under
// run_once.
func TestJobDisableAndEnable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := configureJobs(t, jobFile(jobDef("a", "0 * * * *", "a", runOnceKey+dayTimeout)), l)
		r := start(t, in, "")
		defer r.stop(t)
		tick(time.Hour)
		out := actOK(t, in, "a", actDisable)
		if !out.Done || !out.Changed || out.Phase != phaseDisabled {
			t.Fatalf("disable = %+v", out)
		}
		if len(l.proc(0).stopCalls()) != 0 {
			t.Error("disable stopped the active run (3c-4 Q7: it finishes)")
		}
		if st, _ := in.status("a"); !st.Jobs[0].NextRun.IsZero() || st.Jobs[0].Phase != phaseDisabled {
			t.Errorf("status = %+v", st.Jobs[0])
		}
		js := readFileState(t, r.stateDir).Jobs["a"]
		if !js.Disabled || !js.Idle {
			t.Errorf("state = %+v", js)
		}
		if out := actOK(t, in, "a", actDisable); out.Changed {
			t.Errorf("second disable = %+v", out)
		}
		l.proc(0).end(executor.Exit{}, nil)
		tick(3 * time.Hour) // 04:00
		if n := l.launches(); n != 1 {
			t.Fatalf("%d launches while disabled", n)
		}
		tick(10 * time.Minute)
		out = actOK(t, in, "a", actEnable)
		if !out.Done || !out.Changed || out.Phase != phaseScheduled {
			t.Fatalf("enable = %+v", out)
		}
		if out := actOK(t, in, "a", actEnable); out.Changed {
			t.Errorf("second enable = %+v", out)
		}
		tick(time.Minute)
		js = readFileState(t, r.stateDir).Jobs["a"]
		if l.launches() != 1 || js.Missed != 0 || js.Disabled || js.Idle {
			t.Errorf("after enable: %d launches, state %+v", l.launches(), js)
		}
		tick(49 * time.Minute) // 05:00
		if n := l.launches(); n != 2 {
			t.Errorf("%d launches at the next slot", n)
		}
		if got := actionsAudited(r, actDisable); len(got) != 1 || got[0] != audit.ResultOK {
			t.Errorf("disable audited = %v", got)
		}
		if got := actionsAudited(r, actEnable); len(got) != 1 || got[0] != audit.ResultOK {
			t.Errorf("enable audited = %v", got)
		}
	})
}

// AC5 (Q10): an operator's disable survives a restart, a reload and a
// change of the definition.
func TestJobDisablePersists(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		body := jobFile(jobDef("a", "* * * * *", "a", ""))
		in := configureJobs(t, body, l)
		r := start(t, in, "")
		actOK(t, in, "a", actDisable)
		if err := reloadJobs(t, r, l, jobsCentral, body); err != nil {
			t.Fatal(err)
		}
		if err := reloadJobs(t, r, l, jobsCentral, jobFile(jobDef("a", "* * * * *", "a2", ""))); err != nil {
			t.Fatal(err)
		}
		tick(5 * time.Minute)
		if n := l.launches(); n != 0 {
			t.Fatalf("%d launches after reloads", n)
		}
		r.stop(t)
		l2 := &fakeLauncher{}
		in2 := configureJobs(t, jobFile(jobDef("a", "* * * * *", "a2", "")), l2)
		r2 := start(t, in2, r.stateDir)
		defer r2.stop(t)
		tick(5 * time.Minute)
		if n := l2.launches(); n != 0 {
			t.Fatalf("%d launches after a restart", n)
		}
		actOK(t, in2, "a", actEnable)
		tick(time.Minute)
		if n := l2.launches(); n != 1 {
			t.Errorf("%d launches once enabled", n)
		}
	})
}

// AC5 (D-020), review C3: enabled: false wins over enable, also after a
// reload made it false under an operator's hold; the job then stays idle
// and starts from now once both are lifted.
func TestJobEnableRefusedByTheFile(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		on := jobFile(jobDef("a", "0 * * * *", "a", runOnceKey))
		off := jobFile(jobDef("a", "0 * * * *", "a", runOnceKey+", enabled: false"))
		in := configureJobs(t, on, l)
		r := start(t, in, "")
		defer r.stop(t)
		actOK(t, in, "a", actDisable)
		if err := reloadJobs(t, r, l, jobsCentral, off); err != nil {
			t.Fatal(err)
		}
		_, apiErr := in.act(t.Context(), "a", actEnable)
		if apiErr == nil || apiErr.Code != api.CodeConflict || !strings.Contains(apiErr.Message, "job a is disabled by its configuration") {
			t.Errorf("enable = %+v", apiErr)
		}
		if err := reloadJobs(t, r, l, jobsCentral, on); err != nil {
			t.Fatal(err)
		}
		tick(3 * time.Hour)
		if n := l.launches(); n != 0 {
			t.Fatalf("%d launches under the operator's hold", n)
		}
		actOK(t, in, "a", actEnable)
		tick(time.Minute)
		if js := readFileState(t, r.stateDir).Jobs["a"]; js.Missed != 0 || l.launches() != 0 {
			t.Errorf("state %+v, %d launches", js, l.launches())
		}
	})
}

// Review A2: a disable arriving during an admission keeps its launch from
// beginning, through the loop's context (the flag alone: TestJobLaunchLooksOnceMore).
func TestJobDisableDuringAdmission(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := configureJobs(t, hourlyJob, l)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		var acted bool
		r.audit.setFail(func(e module.AuditEntry) bool {
			if e.Action == actionJobRun && e.Result == audit.ResultIntent && !acted {
				acted = true
				if _, apiErr := in.act(t.Context(), "hourly", actDisable); apiErr != nil {
					t.Errorf("disable: %+v", apiErr)
				}
			}
			return false
		})
		tick(time.Hour)
		tick(time.Hour)
		if n := l.launches(); n != 0 {
			t.Errorf("%d launches after a disable", n)
		}
		if rs := readFileState(t, r.stateDir).Runs; len(rs) != 0 {
			t.Errorf("records left = %+v", rs)
		}
	})
}

// Review A5: a disable whose loop does not end is reported and audited;
// the loop's launch, once back, stops at once; enable is refused until the
// loop has ended, and is not wedged after it.
func TestJobDisableLoopStuck(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gate := make(chan struct{})
		l := &fakeLauncher{gate: gate}
		in := configureJobs(t, jobFile(jobDef("a", "0 * * * *", "a", "")), l)
		r := start(t, in, "")
		defer r.stop(t)
		tick(time.Hour) // the launch is held
		out, apiErr := in.act(t.Context(), "a", actDisable)
		if apiErr != nil || out.(api.SupervisorActionResult).Done {
			t.Fatalf("disable = %+v %+v, want it still in progress", out, apiErr)
		}
		tick(actionSlack + time.Second)
		if got := actionsAudited(r, actDisable); len(got) != 1 || got[0] != audit.ResultFailed {
			t.Errorf("audited = %v", got)
		}
		if !strings.Contains(r.logs.String(), "did not end for an operator's action") {
			t.Errorf("logs:\n%s", r.logs.String())
		}
		for _, a := range []action{actEnable, actDisable} {
			if _, apiErr := in.act(t.Context(), "a", a); apiErr == nil || apiErr.Code != api.CodeConflict {
				t.Errorf("%s while the loop is stuck = %+v", a, apiErr)
			}
		}
		close(gate)
		synctest.Wait()
		if c := l.proc(0).stopCalls(); len(c) != 1 {
			t.Errorf("the late launch was stopped %d times", len(c))
		}
		if out := actOK(t, in, "a", actEnable); !out.Changed {
			t.Errorf("enable after the loop ended = %+v", out)
		}
		tick(time.Hour)
		if n := l.launches(); n != 2 {
			t.Errorf("%d launches after enable", n)
		}
	})
}

// A failed save: enable changes nothing (it lets work run), disable applies
// and says it is not saved (it takes away, review 3b-5 C4).
func TestJobOperatorStateUnwritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes through a read-only directory")
	}
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := configureJobs(t, jobFile(jobDef("a", "* * * * *", "a", "")), l)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		dir := filepath.Join(r.stateDir, moduleName)
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Chmod(dir, 0o700) }()
		if out := actOK(t, in, "a", actDisable); !out.NotSaved || !out.Changed {
			t.Errorf("disable = %+v", out)
		}
		_, apiErr := in.act(t.Context(), "a", actEnable)
		if apiErr == nil || apiErr.Code != api.CodeUnavailable {
			t.Errorf("enable = %+v", apiErr)
		}
		if st, _ := in.status("a"); st.Jobs[0].Phase != phaseDisabled {
			t.Errorf("phase = %s", st.Jobs[0].Phase)
		}
		tick(2 * time.Minute)
		if n := l.launches(); n != 0 {
			t.Errorf("%d launches", n)
		}
	})
}

// AC4 (Q9, O1): a job that did not run for a reason of its own starts from
// now, under run_once too: new, changed, enabled again in the file or by an
// operator, armed again, also when the change came during a downtime.
func TestJobRunOnceNoCatchUpForIdleJobs(t *testing.T) {
	on := hourlyOnce("")
	off := hourlyOnce(", enabled: false")
	disarmed := strings.Replace(jobsCentral, "    processes: true\n", "", 1)
	tests := []struct {
		name            string
		central, body   string // the first life
		central2, body2 string // after three idle hours
		reload          bool   // a reload; otherwise a downtime and a restart
	}{
		{"enabled in a reload", jobsCentral, off, jobsCentral, on, true},
		{"armed in a reload", disarmed, on, jobsCentral, on, true},
		{"changed in a reload", jobsCentral, on, jobsCentral, hourlyOnce(", timeout: 2h"), true},
		{"enabled during a downtime", jobsCentral, off, jobsCentral, on, false},
		{"armed during a downtime", disarmed, on, jobsCentral, on, false},
		{"changed during a downtime", jobsCentral, on, jobsCentral, hourlyOnce(", timeout: 2h"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				clk := &shiftedClock{shift: 10 * time.Minute}
				l := &fakeLauncher{}
				files := func(central, body string) map[string]string {
					return map[string]string{"sentinel.yaml": central, "supervisor/a.yaml": body}
				}
				r := startOn(t, mustConfigureProcs(t, files(tt.central, tt.body), l), "", clk)
				synctest.Wait()
				check := func(st *rig) {
					t.Helper()
					tick(jobWake)
					if js := readFileState(t, st.stateDir).Jobs["h"]; js.Missed != 0 {
						t.Errorf("missed = %d", js.Missed)
					}
					if n := l.launches(); n != 0 {
						t.Errorf("%d launches: a catch-up", n)
					}
				}
				if tt.reload {
					defer r.stop(t)
					clk.move(3 * time.Hour)
					if err := reconfigureTo(t, t.Context(), r, l, files(tt.central2, tt.body2)); err != nil {
						t.Fatal(err)
					}
					check(r)
					return
				}
				r.stop(t)
				clk.move(3 * time.Hour)
				r2 := startOn(t, mustConfigureProcs(t, files(tt.central2, tt.body2), l), r.stateDir, clk)
				defer r2.stop(t)
				check(r2)
			})
		})
	}
}

// Q22: a job removed and added again starts enabled, from now, at zero.
func TestJobRemovedAndAddedAgain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		clk := &shiftedClock{shift: 10 * time.Minute}
		body := hourlyOnce("")
		in := configureJobs(t, body, l)
		r := startOn(t, in, "", clk)
		defer r.stop(t)
		actOK(t, in, "h", actDisable)
		if err := reloadJobs(t, r, l, jobsCentral, "version: 1\n"); err != nil {
			t.Fatal(err)
		}
		clk.move(3 * time.Hour)
		if err := reloadJobs(t, r, l, jobsCentral, body); err != nil {
			t.Fatal(err)
		}
		tick(jobWake)
		js := readFileState(t, r.stateDir).Jobs["h"]
		if js.Disabled || js.Idle || js.Missed != 0 || l.launches() != 0 {
			t.Errorf("state %+v, %d launches", js, l.launches())
		}
		if st, _ := in.status("h"); st.Jobs[0].Phase != phaseScheduled {
			t.Errorf("phase = %s", st.Jobs[0].Phase)
		}
	})
}

// The commands that do not apply stay refused on a disabled job too.
func TestJobOtherCommandsStayRefused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := configureJobs(t, minuteJob, l)
		r := start(t, in, "")
		defer r.stop(t)
		actOK(t, in, "tick", actDisable)
		for _, a := range []action{actStart, actStop, actRestart, actReset} {
			if _, apiErr := in.act(t.Context(), "tick", a); apiErr == nil || apiErr.Code != api.CodeUnsupported {
				t.Errorf("%s = %+v", a, apiErr)
			}
		}
		if _, apiErr := in.act(t.Context(), "nothing", actDisable); apiErr == nil || apiErr.Code != api.CodeBadRequest {
			t.Errorf("unknown name = %+v", apiErr)
		}
	})
}

// AC6: the reload table. An active run is never stopped by a reload that
// keeps programs armed; afterwards the slot follows the job's new definition,
// counting that run by name (Q11).
func TestJobReloadTable(t *testing.T) {
	def := func(arg, key string) string { return jobFile(jobDef("a", "* * * * *", arg, key)) }
	tests := []struct {
		name      string
		from      string
		to        string
		wantArg   string // launched at the slot after the reload, "" nothing
		oldStop   int    // stops of the first run after that slot
		wantSkips uint64
	}{
		{"forbid unchanged", "", "", "", 0, 1},
		{"forbid changed", "", ", timeout: 2h", "", 0, 1},
		{"forbid to allow, the old run is not counted", "", allowKey, "a", 0, 0},
		{"forbid to replace", "", replaceKey, "a", 1, 0},
		{"allow unchanged", allowKey, allowKey, "a", 0, 0},
		{"allow to forbid", allowKey, "", "", 0, 1},
		{"allow to replace", allowKey, replaceKey, "a", 1, 0},
		{"replace unchanged", replaceKey, replaceKey, "a", 1, 0},
		{"replace to forbid", replaceKey, "", "", 0, 1},
		{"replace to allow", replaceKey, allowKey, "a", 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				l := &fakeLauncher{}
				r := start(t, configureJobs(t, def("a", tt.from), l), "")
				defer r.stop(t)
				tick(time.Minute)
				if err := reloadJobs(t, r, l, jobsCentral, def("a", tt.to)); err != nil {
					t.Fatal(err)
				}
				if c := l.proc(0).stopCalls(); len(c) != 0 {
					t.Fatalf("the reload stopped the run: %+v", c)
				}
				tick(time.Minute)
				want := 1
				if tt.wantArg != "" {
					want = 2
				}
				if n := l.launches(); n != want {
					t.Errorf("%d launches, want %d", n, want)
				}
				if c := l.proc(0).stopCalls(); len(c) != tt.oldStop {
					t.Errorf("stops of the old run = %+v", c)
				}
				if js := readFileState(t, r.stateDir).Jobs["a"]; js.Skipped != tt.wantSkips {
					t.Errorf("skipped = %d", js.Skipped)
				}
			})
		})
	}
	t.Run("removed and disabled: runs go on, slots end", func(t *testing.T) {
		for _, key := range []string{"", allowKey, replaceKey} {
			for _, to := range []string{"version: 1\n", def("a", key+", enabled: false")} {
				synctest.Test(t, func(t *testing.T) {
					l := &fakeLauncher{}
					r := start(t, configureJobs(t, def("a", key), l), "")
					defer r.stop(t)
					tick(time.Minute)
					if err := reloadJobs(t, r, l, jobsCentral, to); err != nil {
						t.Fatal(err)
					}
					tick(3 * time.Minute)
					if n, c := l.launches(), l.proc(0).stopCalls(); n != 1 || len(c) != 0 {
						t.Errorf("%q: %d launches, stops %+v", key, n, c)
					}
				})
			}
		}
	})
	t.Run("missed_runs changed: the new policy applies, from now", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := &fakeLauncher{}
			clk := &shiftedClock{}
			r := startOn(t, configureJobs(t, def("a", ""), l), "", clk)
			defer r.stop(t)
			synctest.Wait()
			if err := reloadJobs(t, r, l, jobsCentral, def("a", runOnceKey)); err != nil {
				t.Fatal(err)
			}
			clk.move(10 * time.Minute)
			tick(jobWake) // a jump after the change: a catch-up of the new policy
			if n := l.launches(); n != 1 {
				t.Errorf("%d launches", n)
			}
			if js := readFileState(t, r.stateDir).Jobs["a"]; js.Missed != 1 {
				t.Errorf("missed = %d", js.Missed)
			}
		})
	})
	t.Run("disarmed: every policy stops its runs", func(t *testing.T) {
		disarmed := strings.Replace(jobsCentral, "    processes: true\n", "", 1)
		for _, key := range []string{"", allowKey, replaceKey} {
			synctest.Test(t, func(t *testing.T) {
				l := &fakeLauncher{}
				r := start(t, configureJobs(t, def("a", key), l), "")
				defer r.stop(t)
				tick(time.Minute)
				if err := reloadJobs(t, r, l, disarmed, def("a", key)); err != nil {
					t.Fatal(err)
				}
				if c := l.proc(0).stopCalls(); len(c) != 1 {
					t.Errorf("stops = %+v", c)
				}
				tick(2 * time.Minute)
				if n := l.launches(); n != 1 {
					t.Errorf("%d launches while disarmed", n)
				}
			})
		}
	})
	t.Run("the cap applies at once, without a restart", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := &fakeLauncher{}
			in := configureJobs(t, withRuns(1, jobDef("a", "* * * * *", "a", allowKey)), l)
			r := start(t, in, "")
			defer r.stop(t)
			kept := in.jobByName["a"]
			tick(2 * time.Minute) // 1 run, 00:02 skipped
			if err := reloadJobs(t, r, l, jobsCentral, withRuns(3, jobDef("a", "* * * * *", "a", allowKey))); err != nil {
				t.Fatal(err)
			}
			if in.jobByName["a"] != kept {
				t.Error("a change of the cap replaced the job")
			}
			tick(2 * time.Minute)
			if n := l.launches(); n != 3 {
				t.Fatalf("%d runs under a cap of 3", n)
			}
			if err := reloadJobs(t, r, l, jobsCentral, withRuns(1, jobDef("a", "* * * * *", "a", allowKey))); err != nil {
				t.Fatal(err)
			}
			tick(time.Minute)
			if n := l.launches(); n != 3 {
				t.Errorf("%d runs under a lower cap", n)
			}
			for i := range 3 {
				if c := l.proc(i).stopCalls(); len(c) != 0 {
					t.Errorf("a lower cap stopped run %d", i)
				}
			}
		})
	})
}

// Review C2: the launch runs under the loop's context: a disable while it
// is not queued yet cancels it, the run ends canceled and nothing starts.
func TestJobLaunchCanceledWithTheLoop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{gate: make(chan struct{}), honorCtx: true}
		in := configureJobs(t, jobFile(jobDef("a", "0 * * * *", "a", "")), l)
		r := start(t, in, "")
		defer r.stop(t)
		tick(time.Hour)
		actOK(t, in, "a", actDisable)
		synctest.Wait()
		if n := l.launches(); n != 0 {
			t.Errorf("%d launches", n)
		}
		evs := eventsOf(r.drain(), EventJobFailed)
		if len(evs) != 1 || evs[0].Attributes["failure"] != failureCanceled {
			t.Errorf("events = %+v", evs)
		}
		if rs := readFileState(t, r.stateDir).Runs; len(rs) != 0 {
			t.Errorf("records = %+v", rs)
		}
	})
}

// Review A5: a reservation undone after a reload replaced the job corrects
// the state of the job now under that name, not of the one replaced, and
// never takes its watermark back past a later one.
func TestJobUndoReachesTheCurrentJob(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := configureJobs(t, jobFile(jobDef("a", "0 * * * *", "a", "")), l)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		old := in.jobByName["a"]
		slot := clockAt(1, 0)
		in.mu.Lock()
		old.st.Runs = 1
		old.st.Through = slot
		in.mu.Unlock()
		if err := reloadJobs(t, r, l, jobsCentral, jobFile(jobDef("a", "0 * * * *", "a2", ""))); err != nil {
			t.Fatal(err)
		}
		cur := in.jobByName["a"]
		if cur == old {
			t.Fatal("the job was not replaced")
		}
		id := model.NewEventID()
		in.mu.Lock()
		in.runRecs[id] = runRecord{Job: "a", Phase: runReserved, Scheduled: slot}
		in.mu.Unlock()
		in.undoReservation(&runAdmission{def: old.def, id: id, slot: slot, through: clockAt(0, 0)}, true)
		in.mu.Lock()
		defer in.mu.Unlock()
		if cur.st.Runs != 0 || !cur.st.Through.Equal(clockAt(0, 0)) {
			t.Errorf("current job = %+v", cur.st)
		}
		if _, ok := in.runRecs[id]; ok {
			t.Error("the record stayed")
		}
		cur.st.Through = clockAt(3, 0)
		in.runRecs[id] = runRecord{Job: "a", Phase: runReserved, Scheduled: slot}
		in.mu.Unlock()
		in.undoReservation(&runAdmission{def: old.def, id: id, slot: slot, through: clockAt(0, 0)}, true)
		in.mu.Lock()
		if !cur.st.Through.Equal(clockAt(3, 0)) {
			t.Errorf("a later watermark went back to %s", cur.st.Through)
		}
	})
}

// Review A6: a job's status does not keep a reason from before its disable.
func TestJobDisabledShowsNoStaleReason(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := configureJobs(t, withRuns(1, jobDef("a", "* * * * *", "a", allowKey)), l)
		r := start(t, in, "")
		defer r.stop(t)
		tick(2 * time.Minute)
		if st, _ := in.status("a"); st.Jobs[0].BlockedReason != blockedRunsFull {
			t.Fatalf("reason = %q", st.Jobs[0].BlockedReason)
		}
		in.mu.Lock()
		in.jobByName["a"].catchUp = true // as a loop setting it just after the disable's lock
		in.mu.Unlock()
		actOK(t, in, "a", actDisable)
		in.mu.Lock()
		j := in.jobByName["a"]
		j.catchUp, j.runsFull = true, true
		in.mu.Unlock()
		if st, _ := in.status("a"); st.Jobs[0].BlockedReason != "" || st.Jobs[0].CatchUpPending {
			t.Errorf("status = %+v", st.Jobs[0])
		}
	})
}
