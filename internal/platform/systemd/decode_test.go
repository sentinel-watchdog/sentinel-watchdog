package systemd

import (
	"strings"
	"testing"

	"github.com/godbus/dbus/v5"
)

// props are the values of a healthy, running service.
func props() map[string]any {
	return map[string]any{
		"Id":           "nginx.service",
		"LoadState":    "loaded",
		"ActiveState":  "active",
		"SubState":     "running",
		"Job":          []any{uint32(0), dbus.ObjectPath("/")},
		"InvocationID": []byte("0123456789abcdef"),
		"NRestarts":    uint32(2),
		"Result":       "success",
	}
}

func TestDecodeState(t *testing.T) {
	st, err := decodeState(props())
	if err != nil {
		t.Fatal(err)
	}
	want := UnitState{ID: "nginx.service", Load: "loaded", Active: "active", Sub: "running",
		InvocationID: "30313233343536373839616263646566", NRestarts: 2, Result: "success"}
	if st != want {
		t.Errorf("decodeState = %+v, want %+v", st, want)
	}
}

func TestDecodeStateValues(t *testing.T) {
	tests := []struct {
		name string
		key  string
		val  any // nil: the key is missing
		ok   bool
		// check, when ok, inspects the decoded state.
		check func(UnitState) bool
	}{
		{"job queued", "Job", []any{uint32(7), dbus.ObjectPath("/org/freedesktop/systemd1/job/7")}, true,
			func(s UnitState) bool { return s.Job }},
		{"never started", "InvocationID", []byte{}, true, func(s UnitState) bool { return s.InvocationID == "" }},
		{"not found", "LoadState", "not-found", true, func(s UnitState) bool { return s.Load == LoadNotFound }},
		{"maintenance", "ActiveState", "maintenance", true, func(s UnitState) bool { return s.Active == "maintenance" }},
		{"auto-restart", "SubState", "auto-restart", true, func(s UnitState) bool { return s.Sub == "auto-restart" }},

		{"missing Id", "Id", nil, false, nil},
		{"Id not a string", "Id", uint32(1), false, nil},
		{"Id not a unit name", "Id", "a/b.service", false, nil},
		{"unknown load state", "LoadState", "weird", false, nil},
		{"unknown active state", "ActiveState", "exploded", false, nil},
		{"sub state with a space", "SubState", "run ning", false, nil},
		{"sub state too long", "SubState", strings.Repeat("a", 33), false, nil},
		{"sub state empty", "SubState", "", false, nil},
		{"result upper case", "Result", "Success", false, nil},
		{"invocation of 15 bytes", "InvocationID", []byte("0123456789abcde"), false, nil},
		{"invocation not bytes", "InvocationID", "0123456789abcdef", false, nil},
		{"NRestarts signed", "NRestarts", int32(1), false, nil},
		{"job of one field", "Job", []any{uint32(0)}, false, nil},
		{"job id not uint32", "Job", []any{"0", dbus.ObjectPath("/")}, false, nil},
		{"job not a struct", "Job", uint32(0), false, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := props()
			if tt.val == nil {
				delete(p, tt.key)
			} else {
				p[tt.key] = tt.val
			}
			st, err := decodeState(p)
			if (err == nil) != tt.ok {
				t.Fatalf("error = %v, want ok %v", err, tt.ok)
			}
			if tt.ok && !tt.check(st) {
				t.Errorf("decoded %+v", st)
			}
		})
	}
}

// An unloaded unit has no Service properties: they decode as zero.
func TestDecodeStateWithoutService(t *testing.T) {
	p := props()
	p["LoadState"] = "not-found"
	delete(p, "NRestarts")
	delete(p, "Result")
	st, err := decodeState(p)
	if err != nil {
		t.Fatal(err)
	}
	if st.NRestarts != 0 || st.Result != "" {
		t.Errorf("decoded %+v", st)
	}
}

func FuzzDecodeState(f *testing.F) {
	f.Add("nginx.service", "loaded", "active", "running", []byte("0123456789abcdef"), uint32(1), uint32(0), "success")
	f.Add("", "", "", "", []byte{}, uint32(0), uint32(9), "")
	f.Fuzz(func(t *testing.T, id, load, active, sub string, inv []byte, restarts, job uint32, result string) {
		st, err := decodeState(map[string]any{"Id": id, "LoadState": load, "ActiveState": active, "SubState": sub,
			"Job": []any{job, dbus.ObjectPath("/")}, "InvocationID": inv, "NRestarts": restarts, "Result": result})
		if err != nil {
			return
		}
		if _, err := ParseUnitName(st.ID); err != nil {
			t.Fatalf("accepted Id %q", st.ID)
		}
		if !loadStates[st.Load] || !activeStates[st.Active] || !token(st.Sub) || st.Result != "" && !token(st.Result) {
			t.Fatalf("accepted %+v", st)
		}
		if len(st.InvocationID) != 0 && len(st.InvocationID) != 32 {
			t.Fatalf("invocation %q", st.InvocationID)
		}
	})
}
