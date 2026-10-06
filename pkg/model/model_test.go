package model

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestMonitorTypes(t *testing.T) {
	for _, mt := range ImplementedMonitorTypes() {
		if !mt.IsImplemented() || mt.IsPlanned() {
			t.Errorf("%s: implemented=%v planned=%v", mt, mt.IsImplemented(), mt.IsPlanned())
		}
	}
	for _, mt := range PlannedMonitorTypes() {
		if mt.IsImplemented() || !mt.IsPlanned() {
			t.Errorf("%s: implemented=%v planned=%v", mt, mt.IsImplemented(), mt.IsPlanned())
		}
	}
}

func TestEventScopes(t *testing.T) {
	if s, _ := EventJobTimeout.Scope(); s != ScopeJob {
		t.Errorf("job_timeout scope = %s", s)
	}
	if ScopeForMonitorType(MonitorCron) != ScopeJob || ScopeForMonitorType(MonitorSystemd) != ScopeMonitor {
		t.Error("ScopeForMonitorType mismatch")
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
		ID: NewEventID(), Timestamp: time.Now(), MonitorName: "api", MonitorType: MonitorHTTP,
		Type: EventMonitorFailed, State: StateFailed, PreviousState: StateHealthy,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid event rejected: %v", err)
	}
	daemon := Event{ID: "x", Timestamp: time.Now(), Type: EventDaemonError}
	if err := daemon.Validate(); err != nil {
		t.Errorf("daemon event without monitor rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*Event)
		want   string
	}{
		{"no id", func(e *Event) { e.ID = "" }, "event_id"},
		{"no time", func(e *Event) { e.Timestamp = time.Time{} }, "timestamp"},
		{"bad type", func(e *Event) { e.Type = "boom" }, "unknown event type"},
		{"no monitor", func(e *Event) { e.MonitorName = "" }, "requires monitor_name"},
		{"bad state", func(e *Event) { e.State = "ok" }, "unknown monitor state"},
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
		MonitorName: "m", MonitorType: MonitorHTTP, State: StateFailed, PreviousState: StateHealthy,
		Type: EventMonitorFailed, Message: "msg", FailureCount: 1, RestartCount: 2, LastError: "err",
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
		"event_id", "timestamp", "hostname", "sentinel_version", "monitor_name", "monitor_type", "state",
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
