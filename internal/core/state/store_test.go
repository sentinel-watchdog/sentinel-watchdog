package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/clock"
)

// sample is a module's state type.
type sample struct {
	Restarts map[string]int `json:"restarts"`
}

// Validate rejects negative counters: a decoded file that breaks an
// invariant is corrupt.
func (s *sample) Validate() error {
	for name, n := range s.Restarts {
		if n < 0 {
			return errors.New("negative restart count for " + name)
		}
	}
	return nil
}

var now = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

// open returns the state directory path and a store for module "test".
func open(t *testing.T, version int) (string, *Store) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	d, err := OpenDir(dir, clock.NewFake(now))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	s, err := d.Store("test", version)
	if err != nil {
		t.Fatal(err)
	}
	return dir, s
}

func TestFirstRunAndRoundTrip(t *testing.T) {
	dir, s := open(t, 1)
	got, report, err := Load[sample](s)
	if err != nil || !report.FirstRun || got.Restarts != nil {
		t.Fatalf("first run: %+v %+v %v", got, report, err)
	}
	if err := s.Save(sample{Restarts: map[string]int{"nginx": 2}}); err != nil {
		t.Fatal(err)
	}
	got, report, err = Load[sample](s)
	if err != nil || report != (Report{}) || got.Restarts["nginx"] != 2 {
		t.Fatalf("round trip: %+v %+v %v", got, report, err)
	}

	path := filepath.Join(dir, "test", "state.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != fileMode {
		t.Errorf("mode %04o, want %04o", info.Mode().Perm(), fileMode)
	}
	var env struct {
		SchemaVersion int       `json:"schema_version"`
		UpdatedAt     time.Time `json:"updated_at"`
	}
	raw, _ := os.ReadFile(path)
	if err := json.Unmarshal(raw, &env); err != nil || env.SchemaVersion != 1 || !env.UpdatedAt.Equal(now) {
		t.Errorf("envelope %+v (%v)", env, err)
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "test"))
	if len(entries) != 1 {
		t.Errorf("leftover files after Save: %v", entries)
	}
}

func TestCorruptFilesAreQuarantined(t *testing.T) {
	tests := []struct{ name, content, reason string }{
		{"empty", "  \n", "file is empty"},
		{"invalid JSON", "{", "invalid JSON"},
		{"no schema version", `{"data": {}}`, "schema_version is missing"},
		{"schema version zero", `{"schema_version": 0, "data": {}}`, "invalid schema_version 0"},
		{"no data", `{"schema_version": 1}`, "data is missing"},
		{"data of the wrong shape", `{"schema_version": 1, "data": {"restarts": "many"}}`, "invalid data"},
		{"broken invariant", `{"schema_version": 1, "data": {"restarts": {"nginx": -1}}}`, "negative restart count"},
		{"too large", `{"schema_version": 1, "data": {"x": "` + strings.Repeat("a", maxFileBytes) + `"}}`, "exceeds"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, s := open(t, 1)
			path := filepath.Join(dir, "test", "state.json")
			if err := os.WriteFile(path, []byte(tt.content), fileMode); err != nil {
				t.Fatal(err)
			}
			got, report, err := Load[sample](s)
			if err != nil {
				t.Fatal(err)
			}
			if !report.Recovered || !strings.Contains(report.Reason, tt.reason) || got.Restarts != nil {
				t.Fatalf("report %+v, value %+v", report, got)
			}
			kept, err := os.ReadFile(report.QuarantinedTo)
			if err != nil || string(kept) != tt.content {
				t.Errorf("quarantined file %s: %v", report.QuarantinedTo, err)
			}
			// The module starts fresh and can save again.
			if err := s.Save(sample{}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Another schema version is never overwritten or moved: a downgrade must
// not destroy state, and there is no migration yet.
func TestOtherSchemaVersionsAreLeftUntouched(t *testing.T) {
	tests := []struct {
		name    string
		version int
		want    error
	}{
		{"newer", 3, ErrNewerSchema},
		{"older", 1, ErrOlderSchema},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, s := open(t, 2)
			path := filepath.Join(dir, "test", "state.json")
			content := `{"schema_version": ` + strconv.Itoa(tt.version) + `, "data": {}}`
			if err := os.WriteFile(path, []byte(content), fileMode); err != nil {
				t.Fatal(err)
			}
			if _, _, err := Load[sample](s); !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
			if kept, _ := os.ReadFile(path); string(kept) != content {
				t.Error("the file was changed")
			}
		})
	}
}

// State another user could have written is not trusted.
func TestUntrustedFilesAndDirectories(t *testing.T) {
	t.Run("group-writable state file", func(t *testing.T) {
		dir, s := open(t, 1)
		path := filepath.Join(dir, "test", "state.json")
		if err := os.WriteFile(path, []byte(`{"schema_version": 1, "data": {}}`), fileMode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o660); err != nil {
			t.Fatal(err)
		}
		if _, _, err := Load[sample](s); err == nil || !strings.Contains(err.Error(), "writable by group or others") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("group-writable state directory", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "state")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o770); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenDir(dir, clock.NewFake(now)); err == nil || !strings.Contains(err.Error(), "writable by group or others") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("module directory is a link", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "state")
		d, err := OpenDir(dir, clock.NewFake(now))
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		if err := os.Symlink(t.TempDir(), filepath.Join(dir, "test")); err != nil {
			t.Fatal(err)
		}
		if _, err := d.Store("test", 1); err == nil || !strings.Contains(err.Error(), "not a directory") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("invalid module name", func(t *testing.T) {
		d, err := OpenDir(filepath.Join(t.TempDir(), "state"), clock.NewFake(now))
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		if _, err := d.Store("../core", 1); err == nil {
			t.Fatal("path-like module name accepted")
		}
	})
}

// Concurrent saves never interleave: the file is always one complete
// document. Run with -race.
func TestConcurrentSaves(t *testing.T) {
	_, s := open(t, 1)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			if err := s.Save(sample{Restarts: map[string]int{"n": i}}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if _, report, err := Load[sample](s); err != nil || report.Recovered {
		t.Fatalf("after concurrent saves: %+v %v", report, err)
	}
}

// R1: a quarantine never removes a file that a concurrent Save just wrote.
var validating, resume chan struct{}

type slowInvalid struct{}

func (*slowInvalid) Validate() error {
	close(validating)
	<-resume
	return errors.New("invalid state")
}

func TestQuarantineDoesNotRemoveAConcurrentSave(t *testing.T) {
	_, s := open(t, 1)
	if err := s.Save(sample{}); err != nil {
		t.Fatal(err)
	}
	validating, resume = make(chan struct{}), make(chan struct{})
	loaded := make(chan struct{})
	go func() {
		defer close(loaded)
		if _, _, err := Load[slowInvalid](s); err != nil {
			t.Error(err)
		}
	}()
	<-validating
	saved := make(chan struct{})
	go func() {
		defer close(saved)
		if err := s.Save(sample{Restarts: map[string]int{"nginx": 2}}); err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-saved:
	case <-time.After(100 * time.Millisecond): // Save waits for Load's lock
	}
	close(resume)
	<-loaded
	<-saved
	got, report, err := Load[sample](s)
	if err != nil || report.FirstRun || report.Recovered || got.Restarts["nginx"] != 2 {
		t.Fatalf("the saved state was lost: %+v %+v %v", got, report, err)
	}
}

// R1: stores of one module share their lock.
func TestStoreIsSharedPerModule(t *testing.T) {
	d, err := OpenDir(filepath.Join(t.TempDir(), "state"), clock.NewFake(now))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	a, err := d.Store("test", 1)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := d.Store("test", 1); err != nil || a != b {
		t.Errorf("second Store for the module: %p, %v; want %p", b, err, a)
	}
	if _, err := d.Store("test", 2); err == nil {
		t.Error("a second schema version for the same module accepted")
	}
}

// R5: Save refuses what Load would quarantine, and keeps the old file.
func TestOversizedSaveKeepsThePreviousState(t *testing.T) {
	_, s := open(t, 1)
	if err := s.Save("previous"); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(strings.Repeat("x", maxFileBytes)); err == nil {
		t.Error("oversized Save succeeded")
	}
	if got, report, err := Load[string](s); err != nil || report.Recovered || got != "previous" {
		t.Fatalf("previous state lost: %q %+v %v", got, report, err)
	}
}
