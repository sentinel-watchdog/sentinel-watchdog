package state

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/model"
)

func event(i int, supervisor string) model.Event {
	return model.Event{
		ID: fmt.Sprintf("e%d", i), Timestamp: time.Unix(int64(i), 0), SupervisorName: supervisor,
		SupervisorType: model.SupervisorHTTP, Type: model.EventSupervisorFailed, State: model.StateFailed,
	}
}

func TestSupervisorGetOrCreate(t *testing.T) {
	f := New()
	m := f.Supervisor("api", model.SupervisorHTTP)
	if m.CurrentState != model.StateUnknown || !m.Enabled || m.Name != "api" {
		t.Errorf("unexpected new supervisor: %+v", m)
	}
	m.FailureCount = 3
	if f.Supervisor("api", model.SupervisorHTTP).FailureCount != 3 {
		t.Error("Supervisor must return the existing entry")
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
	a := f.Supervisors["a"]
	if len(a.History) != 3 || a.History[2].EventID != "e8" || a.LastEvent.EventID != "e8" {
		t.Errorf("history of a = %+v", a.History)
	}
	if len(f.Supervisors) != 2 {
		t.Error("daemon events must not create supervisor entries")
	}
}

func TestPrune(t *testing.T) {
	f := New()
	for _, n := range []string{"a", "b", "c"} {
		f.Supervisor(n, model.SupervisorHTTP)
	}
	removed := f.Prune(func(name string) bool { return name == "b" })
	if !slices.Equal(removed, []string{"a", "c"}) || len(f.Supervisors) != 1 {
		t.Errorf("removed %v, left %v", removed, f.Supervisors)
	}
}

func ids(evs []model.Event) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.ID
	}
	return out
}
