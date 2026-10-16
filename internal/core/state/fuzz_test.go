package state

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// FuzzLoad: whatever the state file holds, Load returns a valid state, or
// moves the file aside and starts fresh, or refuses a file of another
// schema version and leaves it untouched; Save never replaces such a file
// (design review 2026-10-16 Q13).
func FuzzLoad(f *testing.F) {
	f.Add([]byte(`{"schema_version": 2, "data": {"restarts": {"nginx": 1}}}`))
	f.Add([]byte(`{"schema_version": 2, "data": {"restarts": {"nginx": -1}}}`))
	f.Add([]byte(`{"schema_version": 1, "data": {}}`))
	f.Add([]byte(`{"schema_version": 3, "data": {}}`))
	f.Add([]byte(`{"schema_version": 2, "data": null}`))
	f.Add([]byte(`{"schema_version": 0}`))
	f.Add([]byte("{"))
	f.Fuzz(func(t *testing.T, data []byte) {
		dir, s := open(t, 2)
		path := filepath.Join(dir, "test", "state.json")
		if err := os.WriteFile(path, data, fileMode); err != nil {
			t.Fatal(err)
		}
		got, report, err := Load[*sample](s)
		switch {
		case err != nil:
			if !errors.Is(err, ErrNewerSchema) && !errors.Is(err, ErrOlderSchema) {
				t.Fatalf("Load: %v", err)
			}
			if err := s.Save(&sample{}); err == nil {
				t.Fatal("Save replaced a file of another schema version")
			}
			if kept, _ := os.ReadFile(path); !bytes.Equal(kept, data) {
				t.Fatal("a file of another schema version was changed")
			}
		case report.Recovered:
			kept, err := os.ReadFile(report.QuarantinedTo)
			if got != nil || err != nil || !bytes.Equal(kept, data) {
				t.Fatalf("recovered %+v, value %+v, quarantined file: %v", report, got, err)
			}
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("corrupt file left in place: %v", err)
			}
		default:
			if got == nil || got.Validate() != nil {
				t.Fatalf("accepted state %+v", got)
			}
			if err := s.Save(got); err != nil {
				t.Fatalf("Save of a loaded state: %v", err)
			}
		}
	})
}
