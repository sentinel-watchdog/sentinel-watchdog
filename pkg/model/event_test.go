package model

import (
	"encoding/json"
	"regexp"
	"testing"
	"time"
)

func TestSeverityRank(t *testing.T) {
	order := []Severity{SeverityInfo, SeverityWarning, SeverityError, SeverityCritical}
	for i, s := range order {
		if got := s.Rank(); got != i+1 {
			t.Errorf("%s.Rank() = %d, want %d", s, got, i+1)
		}
	}
	for _, s := range []Severity{"", "fatal", "INFO"} {
		if got := s.Rank(); got != 0 {
			t.Errorf("%q.Rank() = %d, want 0", s, got)
		}
	}
}

func TestNewEventID(t *testing.T) {
	hex32 := regexp.MustCompile(`^[0-9a-f]{32}$`)
	a, b := NewEventID(), NewEventID()
	if !hex32.MatchString(a) || a == b {
		t.Errorf("ids %q, %q: want two different 32-digit hex strings", a, b)
	}
}

// The JSON names are the webhook contract (docs/notifications.md).
func TestEventJSONNames(t *testing.T) {
	e := Event{
		ID: "id", Timestamp: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC), Hostname: "h",
		SentinelVersion: "v", Module: ModuleCore, Source: "daemon", SourceType: SourceTypeDaemon,
		Type: EventDaemonError, Severity: SeverityError, Message: "m",
	}
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"event_id":"id","timestamp":"2026-10-08T12:00:00Z","hostname":"h","sentinel_version":"v",` +
		`"module":"core","source":"daemon","source_type":"daemon","event_type":"daemon_error",` +
		`"severity":"error","message":"m"}`
	if string(data) != want {
		t.Errorf("JSON =\n%s\nwant\n%s", data, want)
	}
}
