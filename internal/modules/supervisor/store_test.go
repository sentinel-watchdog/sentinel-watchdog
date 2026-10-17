package supervisor

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/clock"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/state"
)

func stateFile(r *rig) string { return filepath.Join(r.stateDir, moduleName, "state.json") }

// readState returns the services in the state file.
func readState(t *testing.T, r *rig) (map[string]serviceState, time.Time) {
	t.Helper()
	data, err := os.ReadFile(stateFile(r))
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		UpdatedAt time.Time `json:"updated_at"`
		Data      fileState `json:"data"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatal(err)
	}
	return env.Data.Services, env.UpdatedAt
}

// failWeb runs web until it fails and returns the incident.
func failWeb(t *testing.T, fs *fakes, r *rig) string {
	t.Helper()
	fs.get("web").set(failing)
	tick(10 * time.Second)
	tick(10 * time.Second)
	evs := r.drain()
	if len(evs) != 1 || evs[0].Type != EventServiceFailed {
		t.Fatalf("events %+v", evs)
	}
	return evs[0].ID
}

// AC5: the state file holds the transition before the event is published.
func TestSavedBeforePublish(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fs := newFakes()
		in, _ := configure(t, map[string]string{"supervisor/a.yaml": webService}, fs.build)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		incident := failWeb(t, fs, r)
		got, _ := readState(t, r)
		if s := got["web"]; s.Health != HealthFailed || s.IncidentID != incident || !digestRe.MatchString(s.Digest) {
			t.Errorf("state file %+v", s)
		}
	})
}

// AC4/AC5: state survives a restart; the incident stays open and its
// recovery is correlated with the failure that a previous run published.
func TestStateSurvivesRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fs := newFakes()
		in, _ := configure(t, map[string]string{"supervisor/a.yaml": webService}, fs.build)
		r := start(t, in, "")
		synctest.Wait()
		incident := failWeb(t, fs, r)
		r.stop(t)

		fs2 := newFakes()
		in2, _ := configure(t, map[string]string{"supervisor/a.yaml": webService}, fs2.build)
		r2 := start(t, in2, r.stateDir)
		defer r2.stop(t)
		if s := statusOf(t, in2, "web"); s.State != "failed" || s.IncidentID != incident {
			t.Fatalf("after restart: %+v", s)
		}
		synctest.Wait() // success 1
		tick(10 * time.Second)
		evs := r2.drain()
		if len(evs) != 1 || evs[0].Type != EventServiceRecovered || evs[0].CorrelationID != incident {
			t.Errorf("events after restart %+v", evs)
		}
	})
}

// AC5/AC8: a changed definition starts from unknown; a change of routes
// or interval keeps the incident (D-080 (3)); removed services are
// dropped from the file.
func TestReconcile(t *testing.T) {
	tests := []struct {
		name, config string
		keep         bool
	}{
		{"route only", strings.Replace(webService, "[ops]", "[pager]", 1), true},
		{"interval", strings.Replace(webService, "check_interval: 10s", "check_interval: 20s", 1), true},
		{"url", strings.Replace(webService, "/hook/", "/other/", 1), false},
		{"policy", strings.Replace(webService, "consecutive_failures: 2", "consecutive_failures: 3", 1), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				fs := newFakes()
				two := webService + "  - {name: gone, url: \"https://gone/\"}\n"
				in, _ := configure(t, map[string]string{"supervisor/a.yaml": two}, fs.build)
				r := start(t, in, "")
				synctest.Wait()
				incident := failWeb(t, fs, r)
				r.stop(t)
				in2, err := configure(t, map[string]string{"supervisor/a.yaml": tt.config}, newFakes().build)
				if err != nil {
					t.Fatal(err)
				}
				fs2 := newFakes()
				fs2.all["web"] = &fake{check: failing}
				in2.services[0].checker = fs2.all["web"]
				r2 := start(t, in2, r.stateDir)
				defer r2.stop(t)
				got := statusOf(t, in2, "web")
				if kept := got.IncidentID == incident && got.State == "failed"; kept != tt.keep {
					t.Errorf("incident kept = %v, want %v: %+v", kept, tt.keep, got)
				}
				if !tt.keep && got.State != "unknown" {
					t.Errorf("changed service in state %s", got.State)
				}
				file, _ := readState(t, r2)
				if _, ok := file["gone"]; ok {
					t.Error("a removed service is still in the state file")
				}
			})
		})
	}
}

// AC5, D-015: a corrupt file is moved aside and the module starts fresh;
// a file of another schema version stops the module and is left untouched.
func TestStateFileProblems(t *testing.T) {
	tests := []struct {
		name, content string
		startFails    bool
	}{
		{"not JSON", "{", false},
		{"unknown health", `{"schema_version":1,"data":{"services":{"web":{"digest":"sha256:` + strings.Repeat("0", 64) + `","health":"dead"}}}}`, false},
		{"failed without incident", `{"schema_version":1,"data":{"services":{"web":{"digest":"sha256:` + strings.Repeat("0", 64) + `","health":"failed"}}}}`, false},
		{"null", `{"schema_version":1,"data":null}`, false},
		{"newer schema", `{"schema_version":2,"data":{}}`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stateDir := filepath.Join(t.TempDir(), "state")
			if err := os.MkdirAll(filepath.Join(stateDir, moduleName), 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(stateDir, moduleName, "state.json")
			if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
				t.Fatal(err)
			}
			in, _ := configure(t, map[string]string{"supervisor/a.yaml": webService}, newFakes().build)
			dir, err := state.OpenDir(stateDir, clock.Real())
			if err != nil {
				t.Fatal(err)
			}
			defer dir.Close()
			r := &rig{logs: &syncBuffer{}}
			rt := runtimeFor(r, dir)
			err = in.Start(context.Background(), rt)
			if tt.startFails {
				kept, _ := os.ReadFile(path)
				if !errors.Is(err, state.ErrNewerSchema) || !bytes.Equal(kept, []byte(tt.content)) {
					t.Errorf("Start = %v; file %q", err, kept)
				}
				return
			}
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			defer func() { _ = in.Stop(context.Background()) }()
			if !strings.Contains(r.logs.String(), "moved aside") {
				t.Errorf("logs: %s", r.logs)
			}
		})
	}
}

// AC5: counters alone are saved at most every 60 s; transitions at once.
func TestCountersSavedAtMostEveryMinute(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in, _ := configure(t, map[string]string{"supervisor/a.yaml": webService}, newFakes().build)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait() // first success: a transition, saved
		_, first := readState(t, r)
		tick(10 * time.Second)
		tick(10 * time.Second)
		if _, at := readState(t, r); !at.Equal(first) {
			t.Errorf("counters saved after 20 s (%s, then %s)", first, at)
		}
		for range 5 {
			tick(10 * time.Second)
		}
		if got, at := readState(t, r); !at.After(first) || got["web"].Checks < 7 {
			t.Errorf("counters not saved after 70 s: %s, %+v", at, got["web"])
		}
	})
}

// M3: a failed save does not stop supervision: the event is still
// published, status says the state is not saved, a later save retries.
func TestSaveFailureKeepsSupervising(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	synctest.Test(t, func(t *testing.T) {
		fs := newFakes()
		in, _ := configure(t, map[string]string{"supervisor/a.yaml": webService}, fs.build)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		moduleDir := filepath.Join(r.stateDir, moduleName)
		if err := os.Chmod(moduleDir, 0o500); err != nil {
			t.Fatal(err)
		}
		failWeb(t, fs, r) // the event is published although the save failed
		if st, _ := in.status(""); !st.StateNotSaved {
			t.Error("status does not say the state is not saved")
		}
		if err := os.Chmod(moduleDir, 0o700); err != nil {
			t.Fatal(err)
		}
		tick(10 * time.Second)
		if st, _ := in.status(""); st.StateNotSaved {
			t.Error("the next check did not save again")
		}
		if got, _ := readState(t, r); got["web"].Health != HealthFailed {
			t.Errorf("state file %+v", got["web"])
		}
	})
}
