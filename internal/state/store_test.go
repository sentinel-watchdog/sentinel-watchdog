package state

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/model"
)

var fixedNow = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

func newTestStore(t *testing.T, opts ...Option) *Store {
	t.Helper()
	opts = append([]Option{WithClock(func() time.Time { return fixedNow })}, opts...)
	return NewStore(filepath.Join(t.TempDir(), "lib", "state.json"), opts...)
}

func TestFirstRun(t *testing.T) {
	s := newTestStore(t)
	f, rep, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !rep.FirstRun || rep.Recovered {
		t.Errorf("report = %+v", rep)
	}
	if f.SchemaVersion != SchemaVersion || f.Monitors == nil || f.Events == nil {
		t.Errorf("unexpected empty state: %+v", f)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	s := newTestStore(t)
	f := New()
	m := f.Monitor("worker", model.MonitorProcess)
	m.CurrentState = model.StateHealthy
	m.RestartCount = 2
	m.RestartAttempts = []time.Time{fixedNow.Add(-time.Minute)}
	code := 137
	m.LastExitCode = &code
	m.LastFailure = fixedNow.Add(-time.Minute)
	f.Monitor("backup", model.MonitorCron).Job = &Job{LastScheduled: fixedNow, LastOutcome: JobSucceeded, Runs: 1}
	f.AppendEvent(model.Event{
		ID: "e1", Timestamp: fixedNow, MonitorName: "worker", MonitorType: model.MonitorProcess,
		Type: model.EventMonitorRecovered, State: model.StateHealthy, PreviousState: model.StateRecovering,
	}, Retention{History: 10, Events: 10})

	if err := s.Save(f); err != nil {
		t.Fatal(err)
	}
	got, rep, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if rep != (LoadReport{}) {
		t.Errorf("report = %+v", rep)
	}
	w := got.Monitors["worker"]
	if w.RestartCount != 2 || *w.LastExitCode != 137 || len(w.RestartAttempts) != 1 ||
		w.LastEvent == nil || w.LastEvent.Type != model.EventMonitorRecovered || !w.LastFailure.Equal(fixedNow.Add(-time.Minute)) {
		t.Errorf("monitor round trip mismatch: %+v", w)
	}
	if got.Monitors["backup"].Job.LastOutcome != JobSucceeded {
		t.Errorf("job round trip mismatch: %+v", got.Monitors["backup"].Job)
	}
	if !got.UpdatedAt.Equal(fixedNow) || len(got.Events) != 1 {
		t.Errorf("file round trip mismatch: %+v", got)
	}
}

func TestSavePermissionsAndNoTempLeftovers(t *testing.T) {
	s := newTestStore(t)
	for range 3 {
		if err := s.Save(New()); err != nil {
			t.Fatal(err)
		}
	}
	info, err := os.Stat(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("state file mode = %o, want 600", perm)
	}
	dirInfo, err := os.Stat(filepath.Dir(s.Path()))
	if err != nil {
		t.Fatal(err)
	}
	if perm := dirInfo.Mode().Perm(); perm&0o007 != 0 {
		t.Errorf("state directory is accessible to others: %o", perm)
	}
	entries, err := os.ReadDir(filepath.Dir(s.Path()))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("unexpected files left behind: %v", entries)
	}
}

func TestSaveOmitsZeroTimestamps(t *testing.T) {
	s := newTestStore(t)
	f := New()
	f.Monitor("x", model.MonitorHTTP)
	if err := s.Save(f); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "0001-01-01") {
		t.Errorf("zero timestamps serialised:\n%s", data)
	}
}

func TestSaveFailureKeepsPreviousFile(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("permission test requires a non-root Unix user")
	}
	s := newTestStore(t)
	f := New()
	f.Monitor("keep", model.MonitorHTTP)
	if err := s.Save(f); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(s.Path())
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o750) })

	err := s.Save(New())
	if err == nil || !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("expected permission error, got %v", err)
	}
	_ = os.Chmod(dir, 0o750)
	got, _, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Monitors["keep"]; !ok {
		t.Error("previous state lost after failed save")
	}
}

func TestCorruptFilesAreQuarantined(t *testing.T) {
	tests := []struct {
		name, content, reason string
	}{
		{"empty", "", "file is empty"},
		{"truncated JSON", `{"schema_version": 1, "monitors": {`, "invalid JSON"},
		{"not an object", `[1,2,3]`, "invalid JSON"},
		{"missing version", `{"monitors": {}}`, "schema_version is missing"},
		{"zero version", `{"schema_version": 0}`, "invalid schema_version 0"},
		{"wrong field type", `{"schema_version": 1, "monitors": {"a": {"monitor_name": "a", "restart_count": "x"}}}`, "invalid content"},
		{"key mismatch", `{"schema_version": 1, "monitors": {"a": {"monitor_name": "b", "current_state": "healthy"}}}`, "monitor_name is"},
		{"unknown state", `{"schema_version": 1, "monitors": {"a": {"monitor_name": "a", "current_state": "weird"}}}`, "unknown monitor state"},
		{"negative counter", `{"schema_version": 1, "monitors": {"a": {"monitor_name": "a", "current_state": "healthy", "failure_count": -1}}}`, "negative counter"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestStore(t)
			if err := os.MkdirAll(filepath.Dir(s.Path()), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(s.Path(), []byte(tt.content), 0o600); err != nil {
				t.Fatal(err)
			}
			f, rep, err := s.Load()
			if err != nil {
				t.Fatal(err)
			}
			if !rep.Recovered || !strings.Contains(rep.Reason, tt.reason) {
				t.Errorf("report = %+v, want reason containing %q", rep, tt.reason)
			}
			if len(f.Monitors) != 0 {
				t.Error("recovered state should be empty")
			}
			moved, err := os.ReadFile(rep.QuarantinedTo)
			if err != nil {
				t.Fatalf("quarantined file missing: %v", err)
			}
			if string(moved) != tt.content {
				t.Error("quarantined content differs from original")
			}
			if _, err := os.Stat(s.Path()); !errors.Is(err, fs.ErrNotExist) {
				t.Error("corrupt file still at original path")
			}
			// The store must be usable afterwards.
			if err := s.Save(f); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOversizedFileIsQuarantined(t *testing.T) {
	s := newTestStore(t, WithMaxFileSize(16))
	if err := os.MkdirAll(filepath.Dir(s.Path()), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.Path(), []byte(`{"schema_version": 1, "monitors": {}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, rep, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Recovered || !strings.Contains(rep.Reason, "size limit") {
		t.Errorf("report = %+v", rep)
	}
}

func TestNewerSchemaIsRefusedAndKept(t *testing.T) {
	s := newTestStore(t)
	if err := os.MkdirAll(filepath.Dir(s.Path()), 0o750); err != nil {
		t.Fatal(err)
	}
	content := `{"schema_version": 99, "future": true}`
	if err := os.WriteFile(s.Path(), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := s.Load()
	if !errors.Is(err, ErrNewerSchema) {
		t.Fatalf("err = %v, want ErrNewerSchema", err)
	}
	data, err := os.ReadFile(s.Path())
	if err != nil || string(data) != content {
		t.Fatalf("newer state file was modified: %q, %v", data, err)
	}
}

func TestUnreadableFileIsAnError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read any file")
	}
	s := newTestStore(t)
	if err := s.Save(New()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(s.Path(), 0o000); err != nil {
		t.Fatal(err)
	}
	_, _, err := s.Load()
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("err = %v, want permission error (not quarantine)", err)
	}
}

func TestConcurrentSaves(t *testing.T) {
	s := newTestStore(t)
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			f := New()
			f.Monitor("m", model.MonitorHTTP).FailureCount = i
			if err := s.Save(f); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	data, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	var f File
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("state file is not valid JSON after concurrent saves: %v", err)
	}
}
