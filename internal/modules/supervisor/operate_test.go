package supervisor

import (
	"context"
	"encoding/json/jsontext"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/audit"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/module"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/platform/executor"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/api"
)

// testRequest is the correlation id the transport gives the request.
const testRequest = "REQUEST0CORRELATION0ID0AB2"

// opFiles is one process service, worker, stopped with SIGTERM within 3s.
func opFiles(extra string) map[string]string {
	return map[string]string{"supervisor/a.yaml": "version: 1\nservices:\n  - name: worker\n    command: /bin/sh\n" +
		"    startup_grace: 0s\n    stop_timeout: 3s\n" + extra}
}

// do runs an operator's command on name as the transport does: the
// request's correlation id is in the context.
func do(t *testing.T, in *instance, cmd api.Command, name string) (api.SupervisorActionResult, *api.Error) {
	t.Helper()
	return doCtx(t, audit.WithCorrelation(context.Background(), testRequest), in, cmd, name)
}

func doCtx(t *testing.T, ctx context.Context, in *instance, cmd api.Command, name string) (api.SupervisorActionResult, *api.Error) {
	t.Helper()
	res, apiErr := in.Handle(ctx, cmd.Name, jsontext.Value(`{"name":"`+name+`"}`))
	synctest.Wait()
	if apiErr != nil {
		return api.SupervisorActionResult{}, apiErr
	}
	return res.(api.SupervisorActionResult), nil
}

func mustDo(t *testing.T, in *instance, cmd api.Command, name string) api.SupervisorActionResult {
	t.Helper()
	res, apiErr := do(t, in, cmd, name)
	if apiErr != nil {
		t.Fatalf("%s %s: %v", cmd.Name, name, apiErr)
	}
	return res
}

// refused runs cmd on name and checks it is refused with code.
func refused(t *testing.T, in *instance, cmd api.Command, name string, code api.Code) {
	t.Helper()
	if _, apiErr := do(t, in, cmd, name); apiErr == nil || apiErr.Code != code {
		t.Errorf("%s %s: %v, want %s", cmd.Name, name, apiErr, code)
	}
}

// entries returns r's audit entries of action.
func entries(r *rig, action string) []module.AuditEntry {
	var out []module.AuditEntry
	for _, e := range r.audit.recorded() {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

func setStubborn(p *fakeProcess) {
	p.mu.Lock()
	p.stubborn = true
	p.mu.Unlock()
}

// D-089: stop stops the process with its own signal and timeout, holds it
// stopped (persisted), is not a failure and is audited with the request;
// start launches it again, not as a recovery attempt.
func TestOperatorStopAndStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := mustConfigureProcs(t, opFiles(""), l)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		res := mustDo(t, in, api.SupervisorStop, "worker")
		if !res.Done || !res.Changed || res.Phase != phaseStopped || res.Action != "stop" || res.Name != "worker" {
			t.Fatalf("stop: %+v", res)
		}
		if stops := l.proc(0).stopCalls(); len(stops) != 1 || stops[0].sig != syscall.SIGTERM || stops[0].timeout != 3*time.Second {
			t.Errorf("stop calls %+v", stops)
		}
		if s := statusOf(t, in, "worker"); s.Phase != phaseStopped || s.StoppedBy != "operator" || s.Pid != 0 || s.State == "failed" {
			t.Errorf("after stop: %+v", s)
		}
		if f := readFileState(t, r.stateDir); !f.Services["worker"].Stopped || len(f.Groups) != 0 {
			t.Errorf("state file after stop: %+v", f)
		}
		if res := mustDo(t, in, api.SupervisorStop, "worker"); res.Changed || !res.Done {
			t.Errorf("a second stop: %+v", res)
		}
		tick(time.Hour)
		if l.launches() != 1 {
			t.Fatal("a stopped service was launched again")
		}
		if res := mustDo(t, in, api.SupervisorStart, "worker"); !res.Done || !res.Changed || l.launches() != 2 {
			t.Fatalf("start: %+v, %d launches", res, l.launches())
		}
		if res := mustDo(t, in, api.SupervisorStart, "worker"); res.Changed || l.launches() != 2 {
			t.Errorf("start of a running service: %+v, %d launches", res, l.launches())
		}
		s := statusOf(t, in, "worker")
		if s.Phase != phaseRunning || s.StoppedBy != "" || s.Restarts != 0 || s.Attempts != 0 || s.Starts != 2 {
			t.Errorf("after start: %+v", s)
		}
		if readFileState(t, r.stateDir).Services["worker"].Stopped {
			t.Error("still stopped in the state file")
		}
		if n := countTypes(r.drain()); n[EventServiceFailed] != 0 || n[EventServiceRestarted] != 0 {
			t.Errorf("events %v", n)
		}
		stop := entries(r, "supervisor.stop")
		if len(stop) != 1 || stop[0].Result != audit.ResultOK || stop[0].Target != "worker" ||
			stop[0].CorrelationID != testRequest || stop[0].Detail["trigger"] != triggerOperator {
			t.Errorf("stop records %+v", stop)
		}
		launch := entries(r, actionStart)
		if last := launch[len(launch)-1]; last.Detail["trigger"] != triggerOperator || last.Detail["request"] != testRequest ||
			last.CorrelationID != testRequest {
			t.Errorf("operator launch record %+v", last)
		}
	})
}

// D-089 Q1, Q4 (review 3b-5 C5): stop and disable last across a restart
// of the module, as a reload that ends in module.ErrRestart does.
func TestOperatorFlagsSurviveARestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		files := procFiles("", procDef("a", "a1", "ops", ""), procDef("b", "b1", "ops", ""))
		l := &fakeLauncher{}
		in := mustConfigureProcs(t, files, l)
		r := start(t, in, "")
		tick(2 * time.Second)
		mustDo(t, in, api.SupervisorStop, "a")
		mustDo(t, in, api.SupervisorDisable, "b")
		r.stop(t)
		l2 := &fakeLauncher{}
		in2 := mustConfigureProcs(t, files, l2)
		r2 := start(t, in2, r.stateDir)
		defer r2.stop(t)
		tick(time.Minute)
		if l2.launches() != 0 {
			t.Fatalf("%d launches after the restart", l2.launches())
		}
		if a, b := statusOf(t, in2, "a"), statusOf(t, in2, "b"); a.Phase != phaseStopped || a.StoppedBy != "operator" ||
			b.Phase != phaseDisabled || b.Disabled != "operator" {
			t.Errorf("a %+v, b %+v", a, b)
		}
	})
}

// D-020, review 3b-5 C5: disable takes away, enable gives back what the
// other flags allow: a stopped service stays stopped.
func TestOperatorDisableAndEnable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := mustConfigureProcs(t, opFiles(""), l)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		if res := mustDo(t, in, api.SupervisorDisable, "worker"); !res.Done || !res.Changed || res.Phase != phaseDisabled {
			t.Fatalf("disable: %+v", res)
		}
		if s := statusOf(t, in, "worker"); s.Phase != phaseDisabled || s.Disabled != "operator" || s.Pid != 0 || s.StoppedBy != "" {
			t.Errorf("after disable: %+v", s)
		}
		if list := in.list(); !list.Services[0].Disabled {
			t.Errorf("list %+v", list)
		}
		if !readFileState(t, r.stateDir).Services["worker"].Disabled {
			t.Error("disable not saved")
		}
		refused(t, in, api.SupervisorStart, "worker", api.CodeConflict)
		refused(t, in, api.SupervisorRestart, "worker", api.CodeConflict)
		if res := mustDo(t, in, api.SupervisorEnable, "worker"); !res.Changed || l.launches() != 2 {
			t.Fatalf("enable: %+v, %d launches", res, l.launches())
		}
		if res := mustDo(t, in, api.SupervisorEnable, "worker"); res.Changed {
			t.Errorf("a second enable: %+v", res)
		}
		mustDo(t, in, api.SupervisorStop, "worker")
		mustDo(t, in, api.SupervisorDisable, "worker")
		mustDo(t, in, api.SupervisorEnable, "worker")
		if s := statusOf(t, in, "worker"); l.launches() != 2 || s.Phase != phaseStopped || s.StoppedBy != "operator" {
			t.Errorf("stop, disable, enable: %d launches, %+v", l.launches(), s)
		}
		mustDo(t, in, api.SupervisorStart, "worker")
		if l.launches() != 3 {
			t.Errorf("start after enable: %d launches", l.launches())
		}
		for _, a := range []string{"supervisor.disable", "supervisor.enable"} {
			if e := entries(r, a); len(e) != 2 || e[0].CorrelationID != testRequest || e[0].Result != audit.ResultOK {
				t.Errorf("%s records %+v", a, e)
			}
		}
	})
}

// D-020 and review 3b-5 C1: enabled: false wins over every command, and
// the group a previous run left is still cleaned up; nothing is launched.
func TestConfigurationDisabledWins(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := mustConfigureProcs(t, opFiles("    enabled: false\n"), l)
		stateDir := filepath.Join(t.TempDir(), "state")
		writeStateFile(t, stateDir, `{"services": {}, "groups": {"worker": {"pgid": 4242, "sid": 4242, "start": 9, "boot": "`+testBoot+`"}}}`)
		r := start(t, in, stateDir)
		defer r.stop(t)
		synctest.Wait()
		if c := l.cleaned(); len(c) != 1 || c[0].Pgid != 4242 {
			t.Fatalf("cleanups %+v", c)
		}
		if s := statusOf(t, in, "worker"); l.launches() != 0 || s.Phase != phaseDisabled || s.Disabled != "configuration" {
			t.Fatalf("%d launches, %+v", l.launches(), s)
		}
		for _, cmd := range []api.Command{api.SupervisorEnable, api.SupervisorStart, api.SupervisorRestart} {
			refused(t, in, cmd, "worker", api.CodeConflict)
		}
		if res := mustDo(t, in, api.SupervisorDisable, "worker"); !res.Changed {
			t.Errorf("disable %+v", res)
		}
		if l.launches() != 0 {
			t.Error("launched")
		}
	})
}

// Review 3b-5 C1: an operator-disabled and an unarmed service clean up
// the group a previous run left too.
func TestGatedServicesCleanUp(t *testing.T) {
	for _, tt := range []struct{ name, central, state string }{
		{"operator disabled", "", `"disabled": true`},
		{"not armed", centralFile, `"stopped": false`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				l := &fakeLauncher{}
				files := opFiles("")
				if tt.central != "" {
					files["sentinel.yaml"] = tt.central
				}
				in := mustConfigureProcs(t, files, l)
				stateDir := filepath.Join(t.TempDir(), "state")
				writeStateFile(t, stateDir, `{"services": {"worker": {"digest": "`+in.byName["worker"].digest+`", "health": "unknown", `+
					tt.state+`}}, "groups": {"worker": {"pgid": 4242, "sid": 4242, "start": 9, "boot": "`+testBoot+`"}}}`)
				r := start(t, in, stateDir)
				defer r.stop(t)
				synctest.Wait()
				if c := l.cleaned(); len(c) != 1 || l.launches() != 0 {
					t.Errorf("cleanups %+v, %d launches", c, l.launches())
				}
			})
		})
	}
}

// Review 3b-5 C6: a stop that outlasts the reply is reported in progress;
// it goes on under its own stop_timeout, not the exchange's deadline.
func TestOperatorLongStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := mustConfigureProcs(t, opFiles(""), l)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		p := l.proc(0)
		setStubborn(p)
		ctx, cancel := context.WithTimeout(audit.WithCorrelation(context.Background(), testRequest), 5*time.Second)
		defer cancel()
		res, apiErr := doCtx(t, ctx, in, api.SupervisorStop, "worker")
		if apiErr != nil || res.Done || res.Phase != phaseStopping {
			t.Fatalf("stop: %+v %v", res, apiErr)
		}
		tick(10 * time.Second) // past the exchange's deadline
		if s := statusOf(t, in, "worker"); s.Phase != phaseStopping {
			t.Fatalf("the stop did not go on: %+v", s)
		}
		p.end(executor.Exit{Code: 0}, nil)
		synctest.Wait()
		if s := statusOf(t, in, "worker"); s.Phase != phaseStopped || s.StoppedBy != "operator" {
			t.Errorf("after the process ended: %+v", s)
		}
	})
}

// Review 3b-5 C2, D-088 (3): a process that does not stop for an operator
// keeps its name blocked until sentineld restarts; Stop stops it.
func TestOperatorStopThatFails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := mustConfigureProcs(t, opFiles(""), l)
		r := start(t, in, "")
		synctest.Wait()
		p := l.proc(0)
		setStubborn(p)
		if res, apiErr := do(t, in, api.SupervisorStop, "worker"); apiErr != nil || res.Done {
			t.Fatalf("stop: %+v %v", res, apiErr)
		}
		tick(time.Minute)
		if s := statusOf(t, in, "worker"); s.Phase != phaseBlocked || s.BlockedReason != blockedOperator {
			t.Fatalf("after a failed stop: %+v", s)
		}
		if e := entries(r, "supervisor.stop"); len(e) != 1 || e[0].Result != audit.ResultFailed {
			t.Errorf("stop records %+v", e)
		}
		for _, cmd := range []api.Command{api.SupervisorStart, api.SupervisorStop, api.SupervisorReset, api.SupervisorEnable} {
			refused(t, in, cmd, "worker", api.CodeConflict)
		}
		if l.launches() != 1 {
			t.Errorf("%d launches beside the blocked process", l.launches())
		}
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		if err := in.Stop(ctx); err == nil || !strings.Contains(err.Error(), "1 processes did not end") {
			t.Errorf("Stop: %v", err)
		}
		if len(p.stopCalls()) < 2 {
			t.Error("Stop did not stop the blocked process")
		}
	})
}

// D-089 Q3: restart of a healthy service stops and launches it once, not
// as a failure nor a recovery attempt.
func TestOperatorRestartHealthy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := mustConfigureProcs(t, opFiles(""), l)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		if res := mustDo(t, in, api.SupervisorRestart, "worker"); !res.Done || !res.Changed {
			t.Fatalf("restart %+v", res)
		}
		if len(l.proc(0).stopCalls()) != 1 || l.launches() != 2 {
			t.Fatalf("%d stops, %d launches", len(l.proc(0).stopCalls()), l.launches())
		}
		if s := statusOf(t, in, "worker"); s.Restarts != 0 || s.Attempts != 0 || s.State == "failed" {
			t.Errorf("after restart: %+v", s)
		}
		if n := countTypes(r.drain()); n[EventServiceFailed] != 0 || n[EventServiceRestarted] != 0 {
			t.Errorf("events %v", n)
		}
	})
}

// exhaust crashes the worker of loopService until its budget is spent.
func exhaust(l *fakeLauncher) {
	for i, d := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second} {
		crash(l, i)
		tick(d)
	}
	crash(l, 3)
}

// D-089 Q3: with an open incident and a spent budget, restart is refused
// before anything is stopped; the process stays watched.
func TestOperatorRestartRefusedWhenSpent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := mustConfigureProcs(t, loopFiles(), l)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		for i, d := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second} {
			crash(l, i)
			tick(d)
		}
		refused(t, in, api.SupervisorRestart, "worker", api.CodeConflict)
		if len(l.proc(3).stopCalls()) != 0 || l.launches() != 4 {
			t.Fatalf("a refused restart stopped or launched: %d launches", l.launches())
		}
		crash(l, 3)
		if n := countTypes(r.drain()); n[EventServiceRecoveryExhausted] != 1 {
			t.Errorf("the process is no longer watched: %v", n)
		}
		refused(t, in, api.SupervisorStart, "worker", api.CodeConflict)
	})
}

// D-089 Q3: a start refused for a spent budget changes nothing, the
// operator's hold included (it is checked before the hold is cleared).
func TestOperatorRefusedStartKeepsTheHold(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := mustConfigureProcs(t, loopFiles(), l)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		exhaust(l)
		mustDo(t, in, api.SupervisorStop, "worker")
		refused(t, in, api.SupervisorStart, "worker", api.CodeConflict)
		if s := statusOf(t, in, "worker"); s.StoppedBy != "operator" || !readFileState(t, r.stateDir).Services["worker"].Stopped {
			t.Errorf("a refused start cleared the hold: %+v", s)
		}
	})
}

// Review 3b-5 C3: the budget is checked again once the loop is joined; a
// restart that reaches the action with a spent budget (it was spent after
// the command's first check) stops nothing and the process stays watched.
func TestOperatorRestartChecksTheBudgetAfterTheJoin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := mustConfigureProcs(t, loopFiles(), l)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		for i, d := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second} {
			crash(l, i)
			tick(d)
		}
		in.ops <- struct{}{} // as act does
		res := in.carry(t.Context(), in.byName["worker"], actRestart, testRequest)
		<-in.ops
		synctest.Wait()
		if res.err == nil || res.err.Code != api.CodeConflict || len(l.proc(3).stopCalls()) != 0 || l.launches() != 4 {
			t.Fatalf("restart with a spent budget: %+v, %d launches", res, l.launches())
		}
		crash(l, 3)
		if n := countTypes(r.drain()); n[EventServiceRecoveryExhausted] != 1 {
			t.Errorf("the process is no longer watched: %v", n)
		}
	})
}

// D-013, D-089 Q3: reset renews a spent budget and launches at once, as
// an operator's attempt of the open incident, which stays open until
// stable_after.
func TestOperatorReset(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := mustConfigureProcs(t, loopFiles(), l)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		exhaust(l)
		before := statusOf(t, in, "worker")
		if before.Phase != phaseExhausted {
			t.Fatalf("not exhausted: %+v", before)
		}
		r.drain()
		if res := mustDo(t, in, api.SupervisorReset, "worker"); !res.Done || !res.Changed {
			t.Fatalf("reset %+v", res)
		}
		s := statusOf(t, in, "worker")
		if l.launches() != 5 || s.Attempts != 1 || !s.ExhaustedAt.IsZero() || s.IncidentID != before.IncidentID || s.State != "failed" {
			t.Fatalf("%d launches, %+v", l.launches(), s)
		}
		evs := r.drain()
		if len(evs) != 1 || evs[0].Type != EventServiceRestarted || evs[0].Attributes["trigger"] != triggerOperator ||
			evs[0].CorrelationID != before.IncidentID {
			t.Errorf("events after reset %+v", evs)
		}
		launch := entries(r, actionRestart)
		if last := launch[len(launch)-1]; last.Detail["request"] != testRequest || last.CorrelationID != before.IncidentID ||
			last.Detail["attempt"] != "1" {
			t.Errorf("launch record %+v", last)
		}
		if e := entries(r, "supervisor.reset"); len(e) != 1 || e[0].CorrelationID != testRequest {
			t.Errorf("reset records %+v", e)
		}
		tick(time.Minute)
		if n := countTypes(r.drain()); n[EventServiceRecovered] != 1 {
			t.Errorf("not recovered at stable_after: %v", n)
		}
	})
}

// Review 3b-5 C3, C7: reset of a running process leaves it alone and the
// new loop adopts it; the next automatic retry is sentineld's own.
func TestOperatorResetAdoptsTheProcess(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := mustConfigureProcs(t, loopFiles(), l)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		crash(l, 0)
		tick(time.Second)
		pid := statusOf(t, in, "worker").Pid
		if res := mustDo(t, in, api.SupervisorReset, "worker"); !res.Changed {
			t.Fatalf("reset %+v", res)
		}
		if s := statusOf(t, in, "worker"); l.launches() != 2 || len(l.proc(1).stopCalls()) != 0 || s.Pid != pid || s.Attempts != 0 {
			t.Fatalf("%d launches, %+v", l.launches(), s)
		}
		crash(l, 1)
		tick(time.Second)
		if l.launches() != 3 {
			t.Fatalf("the adopted process's crash was not followed: %d launches", l.launches())
		}
		launch := entries(r, actionRestart)
		if last := launch[len(launch)-1]; last.Detail["trigger"] != triggerFailure || last.Detail["request"] != "" {
			t.Errorf("automatic retry record %+v", last)
		}
	})
}

// Review 3b-5 C3: a reset while a recovery launch is in flight waits for
// the loop; the launch that completes late is stopped by its own loop, so
// two processes of the name never run, and the budget the reset cleared
// holds only the operator's attempt.
func TestOperatorResetBesideALaunch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := mustConfigureProcs(t, loopFiles(), l)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		gate := make(chan struct{})
		l.mu.Lock()
		l.gate = gate
		l.mu.Unlock()
		crash(l, 0)
		tick(time.Second) // attempt 1 waits in Launch
		if res, apiErr := do(t, in, api.SupervisorReset, "worker"); apiErr != nil || res.Done {
			t.Fatalf("reset beside a launch: %+v %v", res, apiErr)
		}
		close(gate)
		synctest.Wait()
		if l.launches() != 3 {
			t.Fatalf("%d launches", l.launches())
		}
		select {
		case <-l.proc(1).Done():
		default:
			t.Fatal("the late launch is still running beside the operator's")
		}
		if s := statusOf(t, in, "worker"); s.Attempts != 1 || s.Pid != 1003 || s.Phase == phaseStopping {
			t.Errorf("after the reset: %+v", s)
		}
	})
}

// D-013: with a cooldown of 0 an exhausted service waits for a reset,
// however long.
func TestCooldownZero(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		files := map[string]string{"supervisor/a.yaml": strings.Replace(loopService, "cooldown: 10m", "cooldown: 0s", 1)}
		in := mustConfigureProcs(t, files, l)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		exhaust(l)
		s := statusOf(t, in, "worker")
		if s.Phase != phaseExhausted || !s.ResumeAt.IsZero() {
			t.Fatalf("exhausted: %+v", s)
		}
		var exhausted bool
		for _, e := range r.drain() {
			if e.Type == EventServiceRecoveryExhausted {
				_, has := e.Attributes["resume_at"]
				exhausted = !has && strings.Contains(e.Message, "supervisor reset worker")
			}
		}
		if !exhausted {
			t.Error("the exhausted event promises a cooldown")
		}
		tick(7 * 24 * time.Hour)
		if l.launches() != 4 {
			t.Fatalf("launched without a reset: %d", l.launches())
		}
		mustDo(t, in, api.SupervisorReset, "worker")
		if l.launches() != 5 {
			t.Errorf("reset did not launch: %d", l.launches())
		}
	})
}

// D-089 Q4: the operator's flags follow the name across a reload that
// changes the definition, and are dropped with the name.
func TestOperatorFlagsAcrossReload(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := mustConfigureProcs(t, procFiles("", procDef("a", "a1", "ops", ""), procDef("b", "b1", "ops", "")), l)
		r := start(t, in, "")
		defer r.stop(t)
		tick(2 * time.Second)
		mustDo(t, in, api.SupervisorStop, "a")
		mustDo(t, in, api.SupervisorDisable, "b")
		if err := reconfigureTo(t, t.Context(), r, l, procFiles("", procDef("a", "a2", "ops", ""), procDef("b", "b2", "ops", ""))); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if n := len(launchesOf(l, "a2")) + len(launchesOf(l, "b2")); n != 0 {
			t.Fatalf("%d launches of changed services an operator held", n)
		}
		if a, b := statusOf(t, in, "a"), statusOf(t, in, "b"); a.StoppedBy != "operator" || b.Disabled != "operator" || a.State != "unknown" {
			t.Errorf("a %+v, b %+v", a, b)
		}
		if err := reconfigureTo(t, t.Context(), r, l, procFiles("", procDef("b", "b2", "ops", ""))); err != nil {
			t.Fatal(err)
		}
		if err := reconfigureTo(t, t.Context(), r, l, procFiles("", procDef("a", "a2", "ops", ""), procDef("b", "b2", "ops", ""))); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if len(launchesOf(l, "a2")) != 1 {
			t.Error("a re-added name kept the operator's stop")
		}
	})
}

// D-089 Q2: an http service can be disabled (its checks pause, no event)
// and enabled; the process commands do not apply to it.
func TestOperatorHTTP(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fs := newFakes()
		cfg := "version: 1\nservices:\n  - {name: web, url: \"https://web/\", check_interval: 5s}\n"
		in, err := configure(t, map[string]string{"supervisor/a.yaml": cfg}, fs.build)
		if err != nil {
			t.Fatal(err)
		}
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		for _, cmd := range []api.Command{api.SupervisorStart, api.SupervisorStop, api.SupervisorRestart, api.SupervisorReset} {
			refused(t, in, cmd, "web", api.CodeConflict)
		}
		if res := mustDo(t, in, api.SupervisorDisable, "web"); !res.Done || res.Phase != phaseDisabled {
			t.Fatalf("disable %+v", res)
		}
		n := fs.get("web").count()
		tick(time.Minute)
		if fs.get("web").count() != n {
			t.Error("a disabled service is still checked")
		}
		if s := statusOf(t, in, "web"); s.Phase != phaseDisabled || s.Disabled != "operator" || s.State != "healthy" {
			t.Errorf("disabled: %+v", s)
		}
		mustDo(t, in, api.SupervisorEnable, "web")
		tick(time.Second)
		if fs.get("web").count() == n {
			t.Error("checks did not resume")
		}
		if s := statusOf(t, in, "web"); s.Phase != "" || s.Disabled != "" {
			t.Errorf("enabled: %+v", s)
		}
		if evs := r.drain(); len(evs) != 0 {
			t.Errorf("events %+v", evs)
		}
	})
}

// D-084 (1): an operator cannot start what the central switch does not
// arm; disable is still recorded.
func TestOperatorSwitchOff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := mustConfigureProcs(t, procFiles(centralFile, procDef("a", "a1", "ops", "")), l)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		refused(t, in, api.SupervisorStart, "a", api.CodeConflict)
		refused(t, in, api.SupervisorRestart, "a", api.CodeConflict)
		mustDo(t, in, api.SupervisorDisable, "a")
		if l.launches() != 0 || !readFileState(t, r.stateDir).Services["a"].Disabled {
			t.Errorf("%d launches", l.launches())
		}
	})
}

// D-089: one action at a time; another is refused, and a reload waits for
// it or changes nothing.
func TestOperatorOneActionAtATime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		files := procFiles("", procDef("a", "a1", "ops", ""), procDef("b", "b1", "ops", ""))
		in := mustConfigureProcs(t, files, l)
		r := start(t, in, "")
		defer r.stop(t)
		tick(2 * time.Second)
		p := l.proc(launchesOf(l, "a1")[0])
		setStubborn(p)
		if res, apiErr := do(t, in, api.SupervisorStop, "a"); apiErr != nil || res.Done {
			t.Fatalf("stop %+v %v", res, apiErr)
		}
		refused(t, in, api.SupervisorStop, "b", api.CodeUnavailable)
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		err := reconfigureTo(t, ctx, r, l, procFiles("", procDef("a", "a1", "ops", ""), procDef("b", "b2", "ops", "")))
		if err == nil || len(launchesOf(l, "b2")) != 0 {
			t.Fatalf("a reload beside an action: %v", err)
		}
		p.end(executor.Exit{}, nil)
		synctest.Wait()
		if res := mustDo(t, in, api.SupervisorStop, "b"); !res.Done {
			t.Errorf("stop after the first action: %+v", res)
		}
	})
}

// Review 3b-5 C4: a change that lets a service run is saved before it
// applies; one that takes away applies even when it cannot be saved.
func TestOperatorUnsavedChanges(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := mustConfigureProcs(t, procFiles("", procDef("a", "a1", "ops", ""), procDef("b", "b1", "ops", "")), l)
		r := start(t, in, "")
		defer r.stop(t)
		tick(2 * time.Second)
		mustDo(t, in, api.SupervisorStop, "a")
		dir := filepath.Join(r.stateDir, moduleName)
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		refused(t, in, api.SupervisorStart, "a", api.CodeUnavailable)
		if s := statusOf(t, in, "a"); s.StoppedBy != "operator" || len(launchesOf(l, "a1")) != 1 {
			t.Errorf("an unsaved start changed something: %+v", s)
		}
		res := mustDo(t, in, api.SupervisorDisable, "b")
		if !res.NotSaved || !res.Done || statusOf(t, in, "b").Phase != phaseDisabled {
			t.Errorf("unsaved disable %+v", res)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		f := readFileState(t, r.stateDir)
		if !f.Services["a"].Stopped || f.Services["b"].Disabled {
			t.Errorf("state file %+v", f.Services)
		}
		// Review 3b-5 R1: repeating a hold whose save failed saves it.
		if res := mustDo(t, in, api.SupervisorDisable, "b"); res.NotSaved || res.Changed ||
			!readFileState(t, r.stateDir).Services["b"].Disabled {
			t.Errorf("a repeated disable did not save it: %+v", res)
		}
	})
}

// Review 3b-5 R1: the same for a repeated stop.
func TestOperatorRepeatedStopSaves(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := mustConfigureProcs(t, opFiles(""), l)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		dir := filepath.Join(r.stateDir, moduleName)
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		if res := mustDo(t, in, api.SupervisorStop, "worker"); !res.NotSaved {
			t.Fatalf("stop %+v", res)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if res := mustDo(t, in, api.SupervisorStop, "worker"); res.NotSaved || !readFileState(t, r.stateDir).Services["worker"].Stopped {
			t.Errorf("a repeated stop did not save it: %+v", res)
		}
	})
}

// Review 3b-5 R2: a save can fail after the file was replaced (the sync
// of its directory): a failed start must not leave its change on disk.
func TestOperatorFailedStartKeepsTheHoldOnDisk(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := mustConfigureProcs(t, opFiles(""), l)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		mustDo(t, in, api.SupervisorStop, "worker")
		dir := filepath.Join(r.stateDir, moduleName)
		if err := os.Chmod(dir, 0o300); err != nil { // files can be replaced, the directory not opened
			t.Fatal(err)
		}
		_, apiErr := do(t, in, api.SupervisorStart, "worker")
		// Review 3b-5 R4: neither save was confirmed, so a repeated hold
		// really saves, and says it could not.
		if res := mustDo(t, in, api.SupervisorStop, "worker"); !res.NotSaved {
			t.Errorf("an unconfirmed hold reported saved: %+v", res)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if apiErr == nil || apiErr.Code != api.CodeUnavailable {
			t.Fatalf("start: %v", apiErr)
		}
		if !readFileState(t, r.stateDir).Services["worker"].Stopped || l.launches() != 1 {
			t.Fatalf("a failed start cleared the hold on disk (%d launches)", l.launches())
		}
		if res := mustDo(t, in, api.SupervisorStop, "worker"); res.NotSaved {
			t.Fatalf("a repeated hold once the disk is fixed: %+v", res)
		}
		if st, _ := in.status(""); st.StateNotSaved {
			t.Error("the repeated hold did not save")
		}
	})
}

// Stop during an action: the action ends without blocking the name, and
// Stop stops the process.
func TestOperatorActionDuringStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := mustConfigureProcs(t, opFiles(""), l)
		start(t, in, "")
		synctest.Wait()
		p := l.proc(0)
		setStubborn(p)
		if res, apiErr := do(t, in, api.SupervisorStop, "worker"); apiErr != nil || res.Done {
			t.Fatalf("stop %+v %v", res, apiErr)
		}
		var wg sync.WaitGroup
		wg.Go(func() {
			time.Sleep(time.Second)
			p.end(executor.Exit{}, nil)
		})
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		if err := in.Stop(ctx); err != nil {
			t.Errorf("Stop: %v", err)
		}
		wg.Wait()
		if l.launches() != 1 || len(in.retired) != 0 {
			t.Errorf("%d launches, retired %v", l.launches(), in.retired)
		}
	})
}

// The arguments of an operator's command: one valid name, nothing else.
func TestOperatorBadRequests(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := mustConfigureProcs(t, opFiles(""), l)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		for _, args := range []string{"", `{}`, `{"name":"Worker"}`, `{"name":"worker","x":1}`, `{"name":7}`, `{"name":"nobody"}`} {
			if _, apiErr := in.Handle(context.Background(), api.SupervisorStop.Name, jsontext.Value(args)); apiErr == nil ||
				apiErr.Code != api.CodeBadRequest {
				t.Errorf("args %q: %v", args, apiErr)
			}
		}
		if l.launches() != 1 || len(l.proc(0).stopCalls()) != 0 {
			t.Error("a bad request acted")
		}
	})
}

// FuzzActionName: whatever a client sends, the arguments of an operator's
// command give a valid name or bad_request, never a panic.
func FuzzActionName(f *testing.F) {
	for _, seed := range []string{``, `{}`, `{"name":"web"}`, `{"name":"web","x":1}`, `{"name":"../x"}`, `[]`, `{"name":null}`} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, args []byte) {
		name, apiErr := actionName(api.SupervisorStop.Name, jsontext.Value(args))
		if (apiErr == nil) != api.ValidName(name) || (apiErr != nil && apiErr.Code != api.CodeBadRequest) {
			t.Fatalf("args %q: name %q, %v", args, name, apiErr)
		}
	})
}
