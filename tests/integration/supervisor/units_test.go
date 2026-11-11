//go:build integration && linux

package supervisor_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/api"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/model"
)

// Throwaway units of the end-to-end tests (3c-6a): sentineld observes them
// and restarts them, as it would a unit of the host. Recovery is quick:
// one failed check opens an incident, the attempt waits 1 s, and one
// second of running closes it (stable_after's default of 1 min would hold
// service_recovered back). A read that systemd never answers (B-040: the
// first call on a connection opened after a daemon-reload, which each test
// makes) decides nothing; timeout: 2s keeps its cost within the tests'
// 20 s instead of the default 10 s.
const (
	sleeperUnit   = "[Service]\nExecStart=/bin/sleep infinity\n"
	quickCheck    = "check_interval: 1s, timeout: 2s, failure_policy: {consecutive_failures: 1}"
	quickRecovery = "recovery: {max_attempts: 2, window: 10m, delay: 1s, stable_after: 1s, restart_timeout: 1m}"
)

// installUnit writes a throwaway unit under /run/systemd/system and
// removes it when the test ends. tests/integration/systemd has its own:
// test areas do not import each other (B-014).
func installUnit(t *testing.T, name, body string) {
	t.Helper()
	path := filepath.Join(runtimeUnits, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Best effort, each bounded on its own: a stuck call must not keep
		// the file from being removed or the next test from running.
		quiet := func(args ...string) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if out, err := exec.CommandContext(ctx, "systemctl", args...).CombinedOutput(); err != nil {
				t.Logf("cleanup: systemctl %s: %v\n%s", strings.Join(args, " "), err, out)
			}
		}
		quiet("stop", name)
		quiet("reset-failed", name) // a failed unit stays listed otherwise
		if err := os.Remove(path); err != nil {
			t.Logf("cleanup: %v", err)
		}
		quiet("daemon-reload")
	})
	systemctl(t, "daemon-reload")
}

// e2eUnit is a unit name of this test process, for scenario.
func e2eUnit(scenario string) string {
	return fmt.Sprintf("sentinel-itest-e2e-%s-%d.service", scenario, os.Getpid())
}

// unitProp reads a property of a unit with systemctl, apart from
// sentineld's own reader.
func unitProp(t *testing.T, unit, prop string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemctl", "show", "--property="+prop, "--value", unit).Output()
	if err != nil {
		t.Fatalf("systemctl show %s %s: %v", prop, unit, err)
	}
	return strings.TrimSpace(string(out))
}

// awaitActive waits until unit runs under an invocation other than not,
// and returns it.
func awaitActive(t *testing.T, unit, not string) string {
	t.Helper()
	var id string
	eventually(t, unit+" active under a new invocation", func() bool {
		id = unitProp(t, unit, "InvocationID")
		return id != "" && id != not && unitProp(t, unit, "ActiveState") == "active"
	})
	return id
}

// armSystemdRestart sets modules.supervisor.systemd_restart in the central
// file, the last mapping newSetup writes.
func (s setup) armSystemdRestart(t *testing.T) {
	t.Helper()
	f, err := os.OpenFile(s.config, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString("    systemd_restart: true\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatal(err)
	}
}

// unitDaemon starts the daemon with services as its only supervisor file:
// without the worker, whose TERM-ignoring leaf would outlive the daemon's
// SIGKILL at cleanup (review 3c-6a F1).
func (s setup) unitDaemon(t *testing.T, services string) {
	t.Helper()
	if err := os.Remove(filepath.Join(s.dir, "etc", "supervisor", "worker.yaml")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(s.dir, "etc", "supervisor", "units.yaml"), "version: 1\nservices:\n"+services)
	s.startDaemon(t)
	eventually(t, "the control socket", func() bool {
		_, err := os.Stat(filepath.Join(s.dir, "run", "s.sock"))
		return err == nil
	})
}

// serviceStatus is the supervisor status of the service name.
func (s setup) serviceStatus(t *testing.T, name string) api.ServiceStatus {
	t.Helper()
	resp := s.call(t, api.SupervisorStatus.Name, `{"name":"`+name+`"}`)
	var st api.SupervisorStatusResult
	if resp.Error != nil || json.Unmarshal(resp.Result, &st) != nil || len(st.Services) != 1 {
		t.Fatalf("status of %s: %+v", name, resp)
	}
	return st.Services[0]
}

// eventsOf returns the recent events from source, oldest first.
func (s setup) eventsOf(t *testing.T, source string) []model.Event {
	t.Helper()
	resp := s.call(t, api.CoreEvents.Name, fmt.Sprintf(`{"limit":%d}`, api.MaxEventsLimit))
	var r api.EventsResult
	if resp.Error != nil || json.Unmarshal(resp.Result, &r) != nil {
		t.Fatalf("core.events: %+v", resp)
	}
	var out []model.Event
	for _, e := range slices.Backward(r.Events) {
		if e.Source == source {
			out = append(out, e)
		}
	}
	return out
}

// eventOf returns the first event of type typ in evs.
func eventOf(evs []model.Event, typ model.EventType) (model.Event, bool) {
	i := slices.IndexFunc(evs, func(e model.Event) bool { return e.Type == typ })
	if i < 0 {
		return model.Event{}, false
	}
	return evs[i], true
}

// audited returns the audit records of action on target, oldest first.
func (s setup) audited(t *testing.T, action, target string) []api.AuditRecord {
	t.Helper()
	resp := s.call(t, api.CoreAudit.Name, `{"limit":1000}`)
	var a api.AuditResult
	if resp.Error != nil || json.Unmarshal(resp.Result, &a) != nil {
		t.Fatalf("core.audit: %+v", resp)
	}
	var out []api.AuditRecord
	for _, r := range a.Records {
		if r.Action == action && r.Target == target {
			out = append(out, r)
		}
	}
	return out
}

// Scenario 1 (3c-6a AC1): a unit that fails is restarted by sentineld, not
// by systemd, within its budget; the incident is reported and audited from
// failure to recovery under one correlation id.
func TestSentinelRestartsAFailedUnit(t *testing.T) {
	if !hasSystemd() {
		t.Skip("no systemd")
	}
	unit := e2eUnit("restart")
	installUnit(t, unit, sleeperUnit)
	systemctl(t, "start", unit)
	s := newSetup(t)
	s.armSystemdRestart(t)
	s.unitDaemon(t, "  - {name: kept, unit: "+unit+", "+quickCheck+", "+quickRecovery+"}\n")
	eventually(t, "kept healthy", func() bool { return s.serviceStatus(t, "kept").State == "healthy" })
	first := unitProp(t, unit, "InvocationID")

	systemctl(t, "kill", "--signal=SIGKILL", unit)
	awaitActive(t, unit, first)
	var evs []model.Event
	eventually(t, "service_recovered", func() bool {
		evs = s.eventsOf(t, "kept")
		_, ok := eventOf(evs, "service_recovered")
		return ok
	})
	failed, _ := eventOf(evs, "service_failed")
	incident := failed.CorrelationID
	for _, typ := range []model.EventType{"service_failed", "service_restarted", "service_recovered"} {
		if e, ok := eventOf(evs, typ); !ok || e.CorrelationID != incident || incident == "" {
			t.Errorf("%s: %+v (incident %q)", typ, e, incident)
		}
	}
	var results []string
	for _, r := range s.audited(t, api.SupervisorRestart.Name, "kept") {
		if r.Actor.Kind != "daemon" || r.CorrelationID != incident || r.Detail["trigger"] != "failure" ||
			r.Detail["attempt"] != "1" || r.Detail["unit"] != unit {
			t.Errorf("restart record %+v", r)
		}
		results = append(results, r.Result)
	}
	if !slices.Equal(results, []string{"intent", "ok"}) {
		t.Errorf("restart records: %v", results)
	}
	// systemd did not restart it (Restart=no): sentineld did.
	if n := unitProp(t, unit, "NRestarts"); n != "0" {
		t.Errorf("NRestarts = %s", n)
	}
	if st := s.serviceStatus(t, "kept"); st.Restarts != 1 || st.State != "healthy" || st.IncidentID != "" {
		t.Errorf("status %+v", st)
	}
}

// Scenario 2 (AC2): without modules.supervisor.systemd_restart a failed unit
// is reported and left as it is, and status and the operator's restart say
// why.
func TestFailedUnitNotRestartedWhenNotArmed(t *testing.T) {
	if !hasSystemd() {
		t.Skip("no systemd")
	}
	unit := e2eUnit("unarmed")
	installUnit(t, unit, sleeperUnit)
	systemctl(t, "start", unit)
	s := newSetup(t)
	s.unitDaemon(t, "  - {name: left, unit: "+unit+", "+quickCheck+", "+quickRecovery+"}\n")
	eventually(t, "left healthy", func() bool { return s.serviceStatus(t, "left").State == "healthy" })
	first := unitProp(t, unit, "InvocationID")

	systemctl(t, "kill", "--signal=SIGKILL", unit)
	var st api.ServiceStatus
	// The status changes before the event is published, so both are waited for.
	eventually(t, "left failed, not armed, and service_failed", func() bool {
		st = s.serviceStatus(t, "left")
		_, published := eventOf(s.eventsOf(t, "left"), "service_failed")
		return st.State == "failed" && st.BlockedReason == "not_armed" && published
	})
	// Three more checks of sentineld's own: time it had to restart the unit.
	eventually(t, "three more checks", func() bool { return s.serviceStatus(t, "left").Checks >= st.Checks+3 })
	if state, id := unitProp(t, unit, "ActiveState"), unitProp(t, unit, "InvocationID"); state != "failed" || id != first {
		t.Errorf("the unit is %s under %s, was failed under %s", state, id, first)
	}
	resp := s.call(t, api.SupervisorRestart.Name, `{"name":"left"}`)
	if resp.Error == nil || resp.Error.Code != api.CodeConflict || !strings.Contains(resp.Error.Message, "not armed") {
		t.Errorf("operator restart: %+v", resp)
	}
	if _, ok := eventOf(s.eventsOf(t, "left"), "service_restarted"); ok {
		t.Error("service_restarted while not armed")
	}
	// The socket records the refused request; the daemon recorded no restart.
	for _, r := range s.audited(t, api.SupervisorRestart.Name, "left") {
		if r.Actor.Kind == "daemon" {
			t.Errorf("a restart by the daemon: %+v", r)
		}
	}
}

// Scenario 3 (AC3): an operator restarts a running unit. The reply may come
// before the restart is done (2 s, D-089 (4)), so the unit and the audit log
// are read until it is; the request is never sent again.
func TestOperatorRestartsAUnit(t *testing.T) {
	if !hasSystemd() {
		t.Skip("no systemd")
	}
	unit := e2eUnit("operator")
	installUnit(t, unit, sleeperUnit)
	systemctl(t, "start", unit)
	s := newSetup(t)
	s.armSystemdRestart(t)
	s.unitDaemon(t, "  - {name: asked, unit: "+unit+", "+quickCheck+"}\n")
	eventually(t, "asked healthy", func() bool { return s.serviceStatus(t, "asked").State == "healthy" })
	first := unitProp(t, unit, "InvocationID")

	resp := s.call(t, api.SupervisorRestart.Name, `{"name":"asked"}`)
	var out api.SupervisorActionResult
	// changed is known only once the action is done within the reply's 2 s.
	if resp.Error != nil || json.Unmarshal(resp.Result, &out) != nil || out.Name != "asked" || out.Action != "restart" ||
		(out.Done && !out.Changed) {
		t.Fatalf("operator restart: %+v", resp)
	}
	awaitActive(t, unit, first)
	// The daemon's records of the restart, apart from the socket's.
	var results []string
	eventually(t, "the restart's outcome in the audit log", func() bool {
		results = nil
		for _, r := range s.audited(t, api.SupervisorRestart.Name, "asked") {
			if r.Actor.Kind == "daemon" && r.Detail["trigger"] == "operator" && r.Detail["unit"] == unit {
				results = append(results, r.Result)
			}
		}
		return slices.Contains(results, "ok")
	})
	if !slices.Equal(results, []string{"intent", "ok"}) {
		t.Errorf("restart records: %v", results)
	}
}

// Scenario 4 (AC4): a unit systemd keeps restarting is reported as flapping,
// also when sentineld only observes it: its own budget is not charged.
func TestFlappingUnitReported(t *testing.T) {
	if !hasSystemd() {
		t.Skip("no systemd")
	}
	unit := e2eUnit("flapping")
	installUnit(t, unit, "[Unit]\nStartLimitIntervalSec=0\n[Service]\nExecStart=/bin/sh -c 'sleep 1; exit 1'\nRestart=always\nRestartSec=1\n")
	systemctl(t, "start", unit)
	s := newSetup(t)
	s.unitDaemon(t, "  - {name: flaps, unit: "+unit+", check_interval: 1s, timeout: 2s, recovery: {max_attempts: 2, window: 10m}}\n")
	var flap model.Event
	// 30 s: while systemd restarts the unit, lost reads (B-040) can follow
	// one another.
	eventuallyFor(t, 30*time.Second, "service_flapping", func() bool {
		var ok bool
		flap, ok = eventOf(s.eventsOf(t, "flaps"), "service_flapping")
		return ok
	})
	if n, _ := flap.Attributes["restarts"].(float64); n < 2 || flap.Attributes["unit"] != unit { // JSON numbers decode as float64
		t.Errorf("service_flapping %+v", flap)
	}
	if st := s.serviceStatus(t, "flaps"); st.UnitRestarts < 2 || st.Restarts != 0 {
		t.Errorf("status %+v", st)
	}
}
