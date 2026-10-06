package state

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/model"
)

func event(i int, monitor string) model.Event {
	return model.Event{
		ID: fmt.Sprintf("e%d", i), Timestamp: time.Unix(int64(i), 0), MonitorName: monitor,
		MonitorType: model.MonitorHTTP, Type: model.EventMonitorFailed, State: model.StateFailed,
	}
}

func TestMonitorGetOrCreate(t *testing.T) {
	f := New()
	m := f.Monitor("api", model.MonitorHTTP)
	if m.CurrentState != model.StateUnknown || !m.Enabled || m.Name != "api" {
		t.Errorf("unexpected new monitor: %+v", m)
	}
	m.FailureCount = 3
	if f.Monitor("api", model.MonitorHTTP).FailureCount != 3 {
		t.Error("Monitor must return the existing entry")
	}
}

func TestRetention(t *testing.T) {
	f := New()
	r := Retention{History: 3, Events: 5}
	for i := range 10 {
		mon := "a"
		if i%2 == 1 {
			mon = "b"
		}
		f.AppendEvent(event(i, mon), r)
	}
	f.AppendEvent(model.Event{ID: "d", Timestamp: time.Unix(100, 0), Type: model.EventDaemonError}, r)

	if len(f.Events) != 5 || f.Events[4].ID != "d" || f.Events[0].ID != "e6" {
		t.Errorf("global events not trimmed to newest 5: %v", ids(f.Events))
	}
	a := f.Monitors["a"]
	if len(a.History) != 3 || a.History[2].EventID != "e8" || a.LastEvent.EventID != "e8" {
		t.Errorf("history of a = %+v", a.History)
	}
	if len(f.Monitors) != 2 {
		t.Error("daemon events must not create monitor entries")
	}
}

func TestPrune(t *testing.T) {
	f := New()
	for _, n := range []string{"a", "b", "c"} {
		f.Monitor(n, model.MonitorHTTP)
	}
	removed := f.Prune(func(name string) bool { return name == "b" })
	if !slices.Equal(removed, []string{"a", "c"}) || len(f.Monitors) != 1 {
		t.Errorf("removed %v, left %v", removed, f.Monitors)
	}
}

func ids(evs []model.Event) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.ID
	}
	return out
}
