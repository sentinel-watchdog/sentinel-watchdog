package model

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestSupervisorTypes(t *testing.T) {
	for _, mt := range ImplementedSupervisorTypes() {
		if !mt.IsImplemented() || mt.IsPlanned() {
			t.Errorf("%s: implemented=%v planned=%v", mt, mt.IsImplemented(), mt.IsPlanned())
		}
	}
	for _, mt := range PlannedSupervisorTypes() {
		if mt.IsImplemented() || !mt.IsPlanned() {
			t.Errorf("%s: implemented=%v planned=%v", mt, mt.IsImplemented(), mt.IsPlanned())
		}
	}
}

func TestEventScopes(t *testing.T) {
	if s, _ := EventJobTimeout.Scope(); s != ScopeJob {
		t.Errorf("job_timeout scope = %s", s)
	}
	if ScopeForSupervisorType(SupervisorCron) != ScopeJob || ScopeForSupervisorType(SupervisorSystemd) != ScopeSupervisor {
		t.Error("ScopeForSupervisorType mismatch")
	}
	got := EventTypesForScope(ScopeDaemon)
	if len(got) != 2 || got[0] != EventConfigurationError || got[1] != EventDaemonError {
		t.Errorf("daemon events = %v", got)
	}
	if err := EventType("nope").Validate(); err == nil {
		t.Error("unknown event type must not validate")
	}
}

func TestEventValidate(t *testing.T) {
	valid := Event{
		ID: NewEventID(), Timestamp: time.Now(), SupervisorName: "api", SupervisorType: SupervisorHTTP,
		Type: EventSupervisorFailed, State: StateFailed, PreviousState: StateHealthy,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid event rejected: %v", err)
	}
	daemon := Event{ID: "x", Timestamp: time.Now(), Type: EventDaemonError}
	if err := daemon.Validate(); err != nil {
		t.Errorf("daemon event without supervisor rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*Event)
		want   string
	}{
		{"no id", func(e *Event) { e.ID = "" }, "event_id"},
		{"no time", func(e *Event) { e.Timestamp = time.Time{} }, "timestamp"},
		{"bad type", func(e *Event) { e.Type = "boom" }, "unknown event type"},
		{"no supervisor", func(e *Event) { e.SupervisorName = "" }, "requires supervisor_name"},
		{"bad state", func(e *Event) { e.State = "ok" }, "unknown supervisor state"},
		{"bad previous", func(e *Event) { e.PreviousState = "ok" }, "previous_state"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := valid
			tt.mutate(&e)
			if err := e.Validate(); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestEventJSONFieldNames(t *testing.T) {
	e := Event{
		ID: "id", Timestamp: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), Hostname: "h", SentinelVersion: "v",
		SupervisorName: "m", SupervisorType: SupervisorHTTP, State: StateFailed, PreviousState: StateHealthy,
		Type: EventSupervisorFailed, Message: "msg", FailureCount: 1, RestartCount: 2, LastError: "err",
		Metadata: map[string]string{"k": "v"},
	}
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{
		"event_id", "timestamp", "hostname", "sentinel_version", "supervisor_name", "supervisor_type", "state",
		"previous_state", "event_type", "message", "failure_count", "restart_count", "last_error", "metadata",
	} {
		if _, ok := m[k]; !ok {
			t.Errorf("payload is missing %q: %s", k, data)
		}
	}
}

func TestNewEventID(t *testing.T) {
	a, b := NewEventID(), NewEventID()
	if len(a) != 32 || a == b {
		t.Errorf("ids %q %q", a, b)
	}
}

func TestStateValidate(t *testing.T) {
	if err := StateExhausted.Validate(); err != nil {
		t.Error(err)
	}
	if err := CapabilityPermissionDenied.Validate(); err != nil {
		t.Error(err)
	}
	if State("x").Validate() == nil || CapabilityStatus("x").Validate() == nil {
		t.Error("unknown values must not validate")
	}
}
