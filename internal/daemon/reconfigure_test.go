package daemon

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/module"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/redact"
)

// reconfigurable makes every instance of the named modules a
// module.Reconfigurer.
func reconfigurable(names ...string) func(*testModule) {
	return func(m *testModule) { m.reconfigurable = slices.Contains(names, m.name) }
}

// D-088: a module that implements module.Reconfigurer takes a changed
// configuration in place: the running instance stays, gets the runtime of
// the new configuration, and the reload reports and audits it as
// reconfigured.
func TestReloadReconfiguresInPlace(t *testing.T) {
	t.Setenv("SENTINEL_TEST_TOKEN", "token-two-value")
	h := newReloadHarnessWith(t, reconfigurable("alpha"), "", baseModules, map[string]string{"alpha/10.yaml": alphaV1})
	h.write(t, map[string]string{"alpha/10.yaml": "version: 1\nsettings:\n  x: ${SENTINEL_TEST_TOKEN}\n"})
	res, apiErr := h.reload(t)
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	if !slices.Equal(res.Reconfigured, []string{"alpha"}) || len(res.Restarted)+len(res.Failed) != 0 {
		t.Errorf("result %+v", res)
	}
	if !slices.Equal(h.rec.list(), []string{"start alpha", "start beta", "reconfigure alpha"}) {
		t.Errorf("calls %v", h.rec.list())
	}
	if got := redact.Text("a token-two-value b", h.instance("alpha", 0).rt.Secrets...); strings.Contains(got, "token-two-value") {
		t.Errorf("the reconfigured instance kept the old secrets: %q", got)
	}
	if st := h.d.Snapshot().Modules["alpha"]; st != ModuleRunning {
		t.Errorf("alpha is %s", st)
	}
	if resp := h.call(t, "alpha.ping", ""); resp.Error != nil {
		t.Errorf("the reconfigured instance takes no commands: %+v", resp.Error)
	}
	recs := h.auditRecords(t)
	if len(recs) != 2 || recs[1].Detail["reconfigured"] != "alpha" || recs[1].Detail["restarted"] != "" {
		t.Errorf("audit %+v", recs)
	}
	// Unchanged: nothing is offered again.
	if res, apiErr := h.reload(t); apiErr != nil || len(res.Reconfigured) != 0 {
		t.Errorf("unchanged reload: %+v %v", res, apiErr)
	}
}

// D-088 amends D-078 (3): a change of daemon.timezone reconfigures a module
// that can take it in place and restarts the others.
func TestReloadTimezoneReconfiguresInPlace(t *testing.T) {
	h := newReloadHarnessWith(t, reconfigurable("alpha"), "", baseModules, nil)
	h.rewriteCentral(t, "  timezone: Europe/Rome\n", "https://hooks.example.com/1", baseModules)
	res, apiErr := h.reload(t)
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	if !slices.Equal(res.Reconfigured, []string{"alpha"}) || !slices.Equal(res.Restarted, []string{"beta"}) {
		t.Errorf("result %+v", res)
	}
	if slices.Contains(h.rec.list(), "stop alpha") {
		t.Errorf("alpha was stopped: %v", h.rec.list())
	}
}

// module.ErrRestart asks for the stop and start of D-078: no failure is
// reported, the module is restarted.
func TestReloadRestartsOnErrRestart(t *testing.T) {
	h := newReloadHarnessWith(t, reconfigurable("alpha"), "", baseModules, map[string]string{"alpha/10.yaml": alphaV1})
	h.instance("alpha", 0).reconfErr = module.ErrRestart
	h.write(t, map[string]string{"alpha/10.yaml": alphaV2})
	res, apiErr := h.reload(t)
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	if !slices.Equal(res.Restarted, []string{"alpha"}) || len(res.Reconfigured)+len(res.Failed)+len(res.Warnings) != 0 {
		t.Errorf("result %+v", res)
	}
	if !slices.Equal(h.rec.list(), []string{"start alpha", "start beta", "reconfigure alpha", "stop alpha", "start alpha"}) {
		t.Errorf("calls %v", h.rec.list())
	}
	if errs := h.daemonErrors(); len(errs) != 0 {
		t.Errorf("ErrRestart reported as a failure: %+v", errs)
	}
}

// Review 3b-4 C3: a reconfiguration that applies in part leaves the module
// running and pending, so an identical reload offers it again until it
// takes the configuration in full. Its error text is masked with the new
// configuration's secrets.
func TestReloadRetriesAPartialReconfiguration(t *testing.T) {
	t.Setenv("SENTINEL_TEST_TOKEN", "token-two-value")
	h := newReloadHarnessWith(t, reconfigurable("alpha"), "", baseModules, map[string]string{"alpha/10.yaml": alphaV1})
	alpha := h.instance("alpha", 0)
	alpha.reconfErr = errors.New("service web not replaced: token-two-value")
	h.write(t, map[string]string{"alpha/10.yaml": "version: 1\nsettings:\n  x: ${SENTINEL_TEST_TOKEN}\n"})
	for i := range 2 { // the second reload reads the same files
		res, apiErr := h.reload(t)
		if apiErr != nil || !slices.Equal(res.Failed, []string{"alpha"}) || len(res.Reconfigured) != 0 || len(res.Warnings) == 0 {
			t.Fatalf("reload %d: %+v %v", i, res, apiErr)
		}
		if st := h.d.Snapshot().Modules["alpha"]; st != ModuleRunning {
			t.Fatalf("reload %d: alpha is %s", i, st)
		}
	}
	if strings.Contains(h.logs.String(), "token-two-value") {
		t.Error("the error of Reconfigure was logged with a secret of the new configuration")
	}
	if resp := h.call(t, "alpha.ping", ""); resp.Error != nil {
		t.Errorf("a partly reconfigured instance takes no commands: %+v", resp.Error)
	}
	alpha.reconfErr = nil
	res, apiErr := h.reload(t)
	if apiErr != nil || !slices.Equal(res.Reconfigured, []string{"alpha"}) || len(res.Failed) != 0 {
		t.Fatalf("third reload: %+v %v", res, apiErr)
	}
	if res, _ := h.reload(t); len(res.Reconfigured)+len(res.Failed) != 0 {
		t.Errorf("a reconfiguration in full is offered again: %+v", res)
	}
	if !slices.Equal(h.rec.list(), []string{"start alpha", "start beta", "reconfigure alpha", "reconfigure alpha", "reconfigure alpha"}) {
		t.Errorf("calls %v", h.rec.list())
	}
}

// Q4 of 3b-4: a Reconfigure that panics leaves an instance in an unknown
// state: it is treated as a Stop that failed, never reconfigured, stopped
// or started beside again, and Stop keeps the state directory and the
// lock.
func TestReloadQuarantinesAPanickingReconfigure(t *testing.T) {
	h := newReloadHarnessWith(t, reconfigurable("alpha"), "", baseModules, map[string]string{"alpha/10.yaml": alphaV1})
	defer h.d.server.Release()
	defer func() { _ = h.d.state.Close() }()
	h.instance("alpha", 0).reconfPanic = "secret-panic-value"
	h.write(t, map[string]string{"alpha/10.yaml": alphaV2})
	res, apiErr := h.reload(t)
	if apiErr != nil || !slices.Equal(res.Failed, []string{"alpha"}) || len(res.Warnings) == 0 {
		t.Fatalf("result %+v %v", res, apiErr)
	}
	if st := h.d.Snapshot().Modules["alpha"]; st != ModuleFailed {
		t.Errorf("alpha is %s", st)
	}
	if strings.Contains(h.logs.String(), "secret-panic-value") {
		t.Error("the panic value was logged")
	}
	h.write(t, map[string]string{"alpha/10.yaml": "version: 1\nsettings:\n  x: 3\n"})
	if res, _ := h.reload(t); len(res.Restarted)+len(res.Reconfigured) != 0 || len(res.Warnings) == 0 {
		t.Errorf("second reload %+v", res)
	}
	if calls := h.rec.list(); slices.Contains(calls[2:], "start alpha") || slices.Contains(calls, "stop alpha") ||
		strings.Count(strings.Join(calls, ","), "reconfigure alpha") != 1 {
		t.Errorf("calls %v", calls)
	}
	if err := h.d.Stop(context.Background()); err == nil {
		t.Error("Stop did not report the module in an unknown state")
	}
	if server, err := h.d.listen(); err == nil {
		_ = server.Close(context.Background())
		t.Error("socket lock released beside an instance that may run")
	}
}

// Q4 of 3b-4: a Reconfigure that does not return is abandoned, as a Stop
// that does not return; a late return changes nothing.
func TestReloadAbandonsAStuckReconfigure(t *testing.T) {
	release := make(chan struct{})
	h := newReloadHarnessWith(t, reconfigurable("alpha"), "", baseModules, map[string]string{"alpha/10.yaml": alphaV1})
	h.instance("alpha", 0).reconfBlock = release
	h.write(t, map[string]string{"alpha/10.yaml": alphaV2})
	res, apiErr := h.reload(t)
	if apiErr != nil || !slices.Equal(res.Failed, []string{"alpha"}) || len(res.Warnings) == 0 {
		t.Fatalf("result %+v %v", res, apiErr)
	}
	close(release) // the late return
	h.write(t, map[string]string{"alpha/10.yaml": "version: 1\nsettings:\n  x: 3\n"})
	if res, _ := h.reload(t); len(res.Restarted)+len(res.Reconfigured) != 0 || len(res.Warnings) == 0 {
		t.Errorf("second reload %+v", res)
	}
	if st := h.d.Snapshot().Modules["alpha"]; st != ModuleAbandoned {
		t.Errorf("alpha is %s", st)
	}
	if calls := h.rec.list(); slices.Contains(calls[2:], "start alpha") || strings.Count(strings.Join(calls, ","), "reconfigure alpha") != 1 {
		t.Errorf("calls %v", calls)
	}
}
