package supervisor

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json/v2"
	"errors"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/clock"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/config"
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
		{"newer schema", `{"schema_version":3,"data":{}}`, true},
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

// D-082 (maintainer's choice A): every part of an http check is in the
// digest, so changing what is checked starts again from unknown; the
// interval and the routes are not (D-080 (3)). Header order does not
// matter.
func TestDigestCoversTheCheck(t *testing.T) {
	u, _ := url.Parse("https://web/health")
	base := Service{Name: "web", Type: "http", URL: u, Timeout: time.Second, Policy: Policy{3, 1},
		HTTP: HTTP{Method: "POST", Headers: map[string]string{"A": "1", "B": "2"}, Body: "b",
			SuccessCodes: []int{200}, BodyContains: "ok", TLSMinVersion: tls.VersionTLS12}}
	same := []func(*Service){
		func(s *Service) { s.CheckInterval = time.Hour },
		func(s *Service) { s.Route = config.Route{Channels: []string{"ops"}} },
		func(s *Service) { s.HTTP.Headers = map[string]string{"B": "2", "A": "1"} },
	}
	changed := []func(*Service){
		func(s *Service) { s.URL, _ = url.Parse("https://web/other") },
		func(s *Service) { s.Timeout = 2 * time.Second },
		func(s *Service) { s.Policy.Successes = 2 },
		func(s *Service) { s.HTTP.Method = "PUT" },
		func(s *Service) { s.HTTP.Headers = map[string]string{"A": "1", "B": "rotated"} },
		func(s *Service) { s.HTTP.Headers = map[string]string{"A": "1"} },
		func(s *Service) { s.HTTP.Body = "c" },
		func(s *Service) { s.HTTP.SuccessCodes = []int{200, 204} },
		func(s *Service) { s.HTTP.BodyContains = "up" },
		func(s *Service) { s.HTTP.FollowRedirects = true },
		func(s *Service) { s.HTTP.Address = "10.0.0.5:443" },
		func(s *Service) { s.HTTP.TLS.CAFile = "/etc/ca.pem" },
		func(s *Service) { s.HTTP.TLS.ServerName = "web.internal" },
		func(s *Service) { s.HTTP.TLS.InsecureSkipVerify = true },
		func(s *Service) { s.HTTP.TLSMinVersion = tls.VersionTLS13 },
		// Netstrings: moving bytes between fields is a change too.
		func(s *Service) { s.HTTP.Body, s.HTTP.BodyContains = "bo", "k" },
	}
	want := digestOf(base)
	for i, f := range same {
		s := base
		f(&s)
		if got := digestOf(s); got != want {
			t.Errorf("same[%d] changed the digest", i)
		}
	}
	for i, f := range changed {
		s := base
		s.HTTP.Headers = maps.Clone(base.HTTP.Headers)
		f(&s)
		if got := digestOf(s); got == want {
			t.Errorf("changed[%d] kept the digest", i)
		}
	}
}

// D-086: a state file written by v0.1.0 (schema 1) is migrated at Start:
// the http service keeps its open incident (its digest is unchanged since
// v0.1.0, value computed with that release), and the file is rewritten at
// schema 2, which v0.1.0 then refuses instead of misreading.
func TestUpgradeFromV010(t *testing.T) {
	const v010Digest = "sha256:f41a548c04a8633f6807690022db26d29ce71097043da5080d0f9f35e9e7c3f5"
	stateDir := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(filepath.Join(stateDir, moduleName), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stateDir, 0o750); err != nil {
		t.Fatal(err)
	}
	v010 := `{"schema_version": 1, "updated_at": "2026-10-19T10:00:00Z", "data": {"services": {"web": {` +
		`"digest": "` + v010Digest + `", "health": "failed", "since": "2026-10-19T09:00:00Z", "consecutive_failures": 3,` +
		` "consecutive_successes": 0, "last_failure": "connect_failed", "incident_id": "0123456789abcdef0123456789abcdef",` +
		` "checks": 10, "failures": 3}}}}`
	if err := os.WriteFile(filepath.Join(stateDir, moduleName, "state.json"), []byte(v010), 0o600); err != nil {
		t.Fatal(err)
	}
	fs := newFakes()
	in, err := configure(t, map[string]string{"supervisor/a.yaml": webService}, fs.build)
	if err != nil {
		t.Fatal(err)
	}
	if in.byName["web"].digest != v010Digest {
		t.Fatalf("the http digest changed: %s", in.byName["web"].digest)
	}
	fs.get("web").set(failing) // before Start: no check may close the incident
	r := start(t, in, stateDir)
	defer r.stop(t)
	if s := statusOf(t, in, "web"); s.IncidentID != "0123456789abcdef0123456789abcdef" || s.State != "failed" {
		t.Errorf("incident lost: %+v", s)
	}
	if !strings.Contains(r.logs.String(), "supervisor state file migrated") {
		t.Errorf("logs: %s", r.logs.String())
	}
	if got := readFileState(t, stateDir); got.Services["web"].IncidentID == "" {
		t.Errorf("rewritten state: %+v", got)
	}
}
