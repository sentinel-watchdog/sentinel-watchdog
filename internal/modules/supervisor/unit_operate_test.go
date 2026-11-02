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

// actFor runs a for an operator's request with correlation id request.
func actFor(in *instance, a action, request string) (any, *api.Error) {
	return in.act(audit.WithCorrelation(context.Background(), request), "web", a)
}

// An operator's restart restarts a running unit too; without an incident
// it is no attempt, and it is audited with the request (D-089, D-095).
func TestUnitOperatorRestartHealthy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fu := &fakeUnits{}
		newFakeSystemd(fu, runningUnit('a', 0))
		in := configureRestart(t, armedUnits, "", fu) // no recovery: an operator may still restart
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		out, apiErr := actFor(in, actRestart, "req-1")
		if apiErr != nil {
			t.Fatal(apiErr)
		}
		if res := out.(api.SupervisorActionResult); res.Done {
			t.Errorf("done before systemd's job ended: %+v", res)
		}
		tick(5 * time.Second)
		if n := fu.restartCount(); n != 1 {
			t.Fatalf("%d restarts", n)
		}
		var got []string
		for _, e := range r.audit.recorded() {
			if e.Action == actionRestart {
				got = append(got, string(e.Result))
				if e.CorrelationID != "req-1" || e.Detail["trigger"] != triggerOperator || e.Detail["attempt"] != "" {
					t.Errorf("record %+v", e)
				}
			}
		}
		if strings.Join(got, ",") != "intent,ok" {
			t.Errorf("audit %v", got)
		}
		for range 3 {
			tick(10 * time.Second)
		}
		if s := statusOf(t, in, "web"); s.State != "healthy" || s.UnitRestarts != 0 || s.ConsecutiveFailures != 0 {
			t.Errorf("the operator's restart counted as systemd's: %+v", s)
		}
	})
}

// With an open incident an operator's restart is an attempt of it.
func TestUnitOperatorRestartAttempt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fu := &fakeUnits{}
		newFakeSystemd(fu, dead)
		in := configureRestart(t, armedUnits, "    recovery: {delay: 10m}\n", fu)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait() // failed; the automatic attempt waits ten minutes
		if _, apiErr := actFor(in, actRestart, "req-2"); apiErr != nil {
			t.Fatal(apiErr)
		}
		tick(5 * time.Second)
		if s := statusOf(t, in, "web"); s.Attempts != 1 || s.Restarts != 1 {
			t.Errorf("status %+v", s)
		}
		for _, e := range r.audit.recorded() {
			if e.Action == actionRestart && (e.Detail["attempt"] != "1" || e.Detail["trigger"] != triggerOperator) {
				t.Errorf("record %+v", e)
			}
		}
	})
}

// What refuses an operator's restart, before anything is asked of
// systemd.
func TestUnitOperatorRestartRefused(t *testing.T) {
	queued := dead
	queued.Job = 3
	tests := []struct {
		name, central, recovery string
		st                      systemd.UnitState
		exhaust                 bool
		why                     string
	}{
		{"not armed", centralFile, "    recovery: {}\n", dead, false, "systemd_restart is false"},
		{"a job queued", armedUnits, "    recovery: {delay: 10m}\n", queued, false, "job queued"},
		{"budget spent", armedUnits, "    recovery: {max_attempts: 1, delay: 1s, cooldown: 1h}\n", dead, true, "budget"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				fu := &fakeUnits{}
				f := newFakeSystemd(fu, tt.st)
				f.edit(func(f *fakeSystemd) { f.then = dead })
				in := configureRestart(t, tt.central, tt.recovery, fu)
				r := start(t, in, "")
				defer r.stop(t)
				synctest.Wait()
				if tt.exhaust {
					for range 3 {
						tick(10 * time.Second)
					}
				}
				before := fu.restartCount()
				_, apiErr := actFor(in, actRestart, "req")
				if apiErr == nil || apiErr.Code != api.CodeConflict || !strings.Contains(apiErr.Message, tt.why) {
					t.Fatalf("error %+v, want %q", apiErr, tt.why)
				}
				tick(5 * time.Second)
				if n := fu.restartCount(); n != before {
					t.Errorf("%d restarts after a refusal", n-before)
				}
			})
		})
	}
}

// One action at a time: a second one during a slow restart is refused,
// and a reload waits for it (review 3c-2 C6).
func TestUnitOperatorRestartHoldsOps(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fu := &fakeUnits{}
		f := newFakeSystemd(fu, runningUnit('a', 0))
		f.edit(func(f *fakeSystemd) { f.polls = 30 })
		in := configureRestart(t, armedUnits, "", fu)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		if _, apiErr := actFor(in, actRestart, "req-1"); apiErr != nil {
			t.Fatal(apiErr)
		}
		_, apiErr := actFor(in, actDisable, "req-2")
		if apiErr == nil || apiErr.Code != api.CodeUnavailable {
			t.Fatalf("a second action during a restart: %+v", apiErr)
		}
		// A reload waits for the restart (lens-tests T7).
		var reloaded atomic.Bool
		go func() {
			nx := configureRestart(t, armedUnits, "", &fakeUnits{})
			_ = in.Reconfigure(context.Background(), nx, module.Runtime{})
			reloaded.Store(true)
		}()
		tick(10 * time.Second)
		if reloaded.Load() {
			t.Fatal("a reload ran beside the restart")
		}
		tick(40 * time.Second)
		if !reloaded.Load() {
			t.Fatal("the reload did not run after the restart")
		}
		if _, apiErr := actFor(in, actDisable, "req-3"); apiErr != nil {
			t.Fatalf("after the restart: %+v", apiErr)
		}
	})
}

// enable and reset never restart a healthy unit: only an explicit
// restart does (review 3c-2 C5). reset of an exhausted unit restarts it
// at once, as the operator's attempt.
func TestUnitOperatorResetAndEnable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fu := &fakeUnits{}
		f := newFakeSystemd(fu, runningUnit('a', 0))
		in := configureRestart(t, armedUnits, "    recovery: {max_attempts: 1, delay: 1s, cooldown: 1h}\n", fu)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		for _, a := range []action{actDisable, actEnable, actReset} {
			if _, apiErr := actFor(in, a, "req"); apiErr != nil {
				t.Fatalf("%s: %+v", a, apiErr)
			}
			tick(time.Second)
		}
		tick(30 * time.Second)
		if n := fu.restartCount(); n != 0 {
			t.Fatalf("%d restarts of a healthy unit", n)
		}
		f.edit(func(f *fakeSystemd) { f.st, f.then = dead, dead })
		for range 4 {
			tick(10 * time.Second)
		}
		if s := statusOf(t, in, "web"); s.Phase != phaseExhausted || fu.restartCount() != 1 {
			t.Fatalf("status %+v, %d restarts", s, fu.restartCount())
		}
		if _, apiErr := actFor(in, actReset, "req-r"); apiErr != nil {
			t.Fatal(apiErr)
		}
		tick(5 * time.Second)
		if n := fu.restartCount(); n != 2 {
			t.Fatalf("%d restarts after reset, want 2", n)
		}
		var trigger string
		for _, e := range r.audit.recorded() {
			if e.Action == actionRestart && e.Result == audit.ResultIntent {
				trigger = e.Detail["trigger"]
			}
		}
		if trigger != triggerOperator {
			t.Errorf("the attempt after reset has trigger %q", trigger)
		}
	})
}

// A reload that revokes the switch ends a planned attempt: nothing is
// restarted afterwards (D-088).
func TestUnitRevokedSwitch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fu := &fakeUnits{}
		newFakeSystemd(fu, dead)
		in := configureRestart(t, armedUnits, "    recovery: {delay: 30s}\n", fu)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait() // the attempt waits 30 s
		nx := configureRestart(t, centralFile, "    recovery: {delay: 30s}\n", &fakeUnits{})
		if err := in.Reconfigure(context.Background(), nx, module.Runtime{}); err != nil {
			t.Fatal(err)
		}
		for range 6 {
			tick(10 * time.Second)
		}
		if n := fu.restartCount(); n != 0 {
			t.Fatalf("%d restarts after the switch was revoked", n)
		}
	})
}
