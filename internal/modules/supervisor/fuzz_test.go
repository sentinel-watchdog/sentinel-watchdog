package supervisor

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/config"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/api"
)

// FuzzConfigure: whatever a supervisor file holds, the loader and
// Configure never panic; a configuration they accept is within the
// module's limits, and no problem quotes the secret expanded into it
// (AGENTS.md: every loader of untrusted input has a Fuzz target).
func FuzzConfigure(f *testing.F) {
	f.Add(webService)
	// Full review 2026-10-28 F6: cooldown 0 is valid (D-013).
	f.Add("version: 1\nservices:\n  - {name: a, command: /bin/sh, recovery: {cooldown: 0s}}\n")
	f.Add("version: 1\nsettings: {max_concurrent_checks: 2}\nservices:\n  - {name: a, url: \"https://${TOKEN}@h/\"}\n")
	f.Add("version: 1\nservices:\n  - {name: a, type: systemd}\n  - 7\njobs: []\n")
	f.Add("version: 1\nservices:\n  - name: a\n    url: https://h:8443/\n    method: post\n" +
		"    headers: {Authorization: \"Bearer ${TOKEN}\", x-a: \"1\"}\n    body: \"${TOKEN}\"\n" +
		"    success_status_codes: [200, 204]\n    body_contains: ok\n    follow_redirects: true\n" +
		"    address: \"[::1]:8443\"\n    tls: {ca_file: /nonexistent/ca.pem, server_name: h, min_version: 1.3, insecure_skip_verify: true}\n")
	f.Add("version: 1\nservices:\n  - {name: a, url: \"http://h/\", method: HEAD, body_contains: \"\", address: 0.0.0.0, tls: {}}\n")
	f.Add("version: 1\nservices: &s\n  - {name: a, url: \"http://h/\", recovery: {}}\nx: *s\n")
	f.Add(workerService)
	f.Add("version: 1\nservices:\n  - {name: a, script: \"echo ${TOKEN}\", shell: /bin/sh, user: worker, group: \"50\"}\n" +
		"  - {name: b, command: /bin/sh, args: [\"${TOKEN}\"], environment: {K: \"${TOKEN}\"}, output: {file: /tmp/b.log, mode: \"0600\"}}\n" +
		"  - {name: c, command: /bin/sh, url: \"http://h/\"}\n")
	f.Add(loopService)
	f.Add("version: 1\nservices:\n  - {name: a, command: /bin/sh, limits: {memory: 512MiB, cpu: 2}}\n" +
		"  - {name: b, command: /bin/sh, limits: {memory: 18446744073709551615}}\n  - {name: c, url: \"http://h/\", limits: {}}\n")
	f.Add("version: 1\nservices:\n  - {name: a, command: /bin/sh, startup_grace: 5m, recovery: {backoff: fixed, delay: 10m," +
		" stable_after: 1s, cooldown: 0s, max_attempts: 101, window: \"${TOKEN}\"}}\n")
	// Jobs (3c-4a): a schedule holding a secret, every program key, the
	// planned values, a name shared with a service.
	f.Add("version: 1\njobs:\n  - {name: a, schedule: \"*/${TOKEN} * * * *\", command: /bin/sh}\n")
	f.Add("version: 1\njobs:\n  - {name: a, type: cron, schedule: \"@daily\", timezone: Asia/Kolkata, timeout: 24h, " +
		"script: \"echo ${TOKEN}\", user: worker, group: staff, environment: {K: \"${TOKEN}\"}, output: discard, " +
		"stop_signal: INT, stop_timeout: 5m, concurrency_policy: forbid, missed_runs: skip, enabled: false}\n")
	f.Add("version: 1\nservices:\n  - {name: a, command: /bin/sh}\njobs:\n  - {name: a, schedule: \"0 0 30 2 *\", command: /bin/sh, " +
		"concurrency_policy: replace, missed_runs: run_once, recovery: {}, timeout: 0s, timezone: \"${TOKEN}\"}\n")
	f.Fuzz(func(t *testing.T, body string) {
		dir := t.TempDir()
		main := filepath.Join(dir, "sentinel.yaml")
		if err := os.WriteFile(main, []byte(centralFile), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(dir, moduleName), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, moduleName, "f.yaml"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		const token = "tok-9f8e7d"
		cfg, err := config.Load(config.LoadOptions{MainFile: main,
			Modules: map[string]config.ModuleAvailability{moduleName: config.ModuleAvailable},
			Lookup:  func(k string) (string, bool) { return token, k == "TOKEN" }})
		if err != nil {
			return
		}
		mc, _ := cfg.Module(moduleName)
		c, err := New(Options{NewChecker: newFakes().build, Lookup: fakeLookup}).Configure(mc)
		if err != nil {
			if strings.Contains(err.Error(), token) {
				t.Fatalf("a problem quotes the secret: %v", err)
			}
			return
		}
		in := c.(*instance)
		if len(in.services)+len(in.jobs) > maxServices || in.settings.MaxConcurrentChecks < 1 {
			t.Fatalf("accepted %d services and %d jobs, %d slots", len(in.services), len(in.jobs), in.settings.MaxConcurrentChecks)
		}
		for _, j := range in.jobs {
			d := j.def
			if _, dup := in.byName[d.Name]; dup || d.Schedule == nil || d.Location == nil || d.Timeout < minJobTimeout ||
				d.Timeout > maxJobTimeout || d.Concurrency != concurrencyForbid || d.Missed != missedSkip {
				t.Fatalf("accepted job %+v", d)
			}
			checkAcceptedProcess(t, Service{Name: d.Name, Type: typeProcess, Process: d.Program,
				Recovery: Recovery{MaxAttempts: 1, Window: minWindow, Delay: minDelay, MaxDelay: minDelay,
					StableAfter: minStableAfter, Backoff: config.BackoffFixed}})
		}
		for _, s := range in.services {
			if s.def.Type == typeProcess {
				checkAcceptedProcess(t, s.def)
				continue
			}
			if s.def.Type != typeHTTP || !api.ValidName(s.def.Name) || s.def.URL == nil || s.def.URL.Hostname() == "" ||
				s.def.Timeout < minTimeout || s.def.CheckInterval < minCheckInterval {
				t.Fatalf("accepted service %+v", s.def)
			}
			h := s.def.HTTP
			if !slices.Contains(methods, h.Method) || len(h.Headers) > maxHeaders || len(h.Body) > maxBodyBytes ||
				(h.Body != "" && h.Method != http.MethodPost && h.Method != http.MethodPut) || len(h.BodyContains) > maxBodyContains {
				t.Fatalf("accepted request %+v", h)
			}
			for name, v := range h.Headers {
				if strings.ContainsFunc(v, isControl) || name == "Host" {
					t.Fatalf("accepted header %q", name)
				}
			}
		}
	})
}

// checkAcceptedProcess fails t unless def is a process service within the
// limits of D-005 and D-086.
func checkAcceptedProcess(t *testing.T, def Service) {
	t.Helper()
	pr := def.Process
	if !api.ValidName(def.Name) || def.URL != nil || !filepath.IsAbs(pr.Command) || !filepath.IsAbs(pr.WorkingDir) ||
		len(pr.Args) > maxArgs || len(pr.Env) > maxEnv+1 || pr.StopTimeout < minStopTimeout || pr.StopTimeout > maxStopTimeout ||
		pr.StartupGrace < 0 || pr.StartupGrace > maxStartupGrace || (pr.Script && (len(pr.Args) != 2 || pr.Args[0] != "-c")) {
		t.Fatalf("accepted process %+v", def)
	}
	for _, kv := range pr.Env {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || !envNameRe.MatchString(k) || strings.ContainsRune(kv, 0) {
			t.Fatalf("accepted environment entry of %d bytes", len(kv))
		}
	}
	if pr.Output.Kind == outputFile && (!filepath.IsAbs(pr.Output.File) || pr.Output.Mode&0o7022 != 0) {
		t.Fatalf("accepted output %+v", pr.Output)
	}
	r := def.Recovery
	if r.MaxAttempts < 1 || r.MaxAttempts > maxMaxAttempts || r.Window < minWindow || r.Window > maxWindow ||
		r.Delay < minDelay || r.Delay > maxDelayValue || r.MaxDelay < r.Delay || r.MaxDelay > maxMaxDelay ||
		r.StableAfter < pr.StartupGrace || r.StableAfter < minStableAfter || (r.Cooldown != 0 && r.Cooldown < minCooldown) || r.Cooldown > maxCooldown ||
		(r.Backoff != config.BackoffFixed && r.Backoff != config.BackoffExponential) {
		t.Fatalf("accepted recovery %+v", r)
	}
}

// FuzzStateValidate: a state file of a previous run is untrusted input.
// Whatever its data, Validate never panics; a budget it accepts is
// stored valid again after any admission and reservation (clock jumps
// included, also between the two), so that the next run never finds its
// own file corrupt.
func FuzzStateValidate(f *testing.F) {
	f.Add(`{"services": {"w": {"digest": "sha256:`+strings.Repeat("a", 64)+`", "health": "failed", "incident_id": "`+testIncident+
		`", "attempts": ["2026-10-23T12:00:00Z", "2026-10-23T12:00:00Z", "2026-10-23T12:01:00Z"], "exhausted_at": "2026-10-23T13:00:00Z",
		"starts": 3, "restarts": 2}}}`, int64(0), uint16(0))
	f.Add(`{"services": {"w": {"digest": "sha256:`+strings.Repeat("a", 64)+`", "health": "healthy", "attempts": ["2026-10-23T12:01:00Z", "2026-10-23T12:00:00Z"]}}}`, int64(-3600), uint16(60))
	f.Add(`{"services": {}, "groups": {"w": {"pgid": 2, "sid": 1, "start": 1, "boot": "`+testBoot+`"}}}`, int64(86400), uint16(3600))
	run := func(phase, extra string) string {
		return `{"job": "j", "phase": "` + phase + `", "scheduled": "2026-10-23T12:00:00Z"` + extra + `}`
	}
	grp := `, "started": "2026-10-23T12:00:01Z", "group": {"pgid": 4242, "sid": 50, "start": 9, "boot": "` + testBoot + `"}`
	f.Add(`{"services": {}, "jobs": {"j": {"digest": "sha256:`+strings.Repeat("b", 64)+`", "through": "2026-10-23T12:00:00Z", "runs": 3,
		"succeeded": 1, "failed": 1, "missed": 2, "last_missed": {"from": "2026-10-23T09:00:00Z", "to": "2026-10-23T11:59:00Z"},
		"last": {"run_id": "`+strings.Repeat("c", 32)+`", "scheduled": "2026-10-23T11:00:00Z", "ended": "2026-10-23T11:00:05Z", "result": "failed", "failure": "exit_code", "code": 3}}},
		"runs": {"`+strings.Repeat("d", 32)+`": `+run(runReserved, "")+`, "`+strings.Repeat("e", 32)+`": `+run(runRunning, grp)+`,
		"`+strings.Repeat("f", 32)+`": `+run(runCleanupPending, grp)+`}}`, int64(0), uint16(0))
	f.Add(`{"services": {}, "runs": {"`+strings.Repeat("a", 32)+`": `+run(runReserved, grp)+`}}`, int64(0), uint16(0))
	f.Fuzz(func(t *testing.T, data string, shift int64, back uint16) {
		var fs fileState
		if json.Unmarshal([]byte(data), &fs) != nil || fs.Validate() != nil {
			return
		}
		if len(fs.Groups)+len(fs.Runs) > maxGroups {
			t.Fatalf("accepted %d group and run records", len(fs.Groups)+len(fs.Runs))
		}
		for id, rec := range fs.Runs {
			known := rec.Phase == runReserved || rec.Phase == runRunning || rec.Phase == runCleanupPending
			if !runIDRe.MatchString(id) || rec.Scheduled.IsZero() || !known ||
				rec.Phase == runReserved && rec.Group != (groupRecord{}) {
				t.Fatalf("accepted run %s %+v", id, rec)
			}
		}
		now := time.Date(2026, 10, 23, 12, 30, 0, 0, time.UTC).Add(time.Duration(shift) * time.Second)
		r := Recovery{MaxAttempts: 3, Window: time.Hour, Delay: time.Second, Backoff: config.BackoffExponential,
			MaxDelay: time.Minute, StableAfter: time.Minute, Cooldown: 10 * time.Minute}
		for name, st := range fs.Services {
			b, a := admit(st.budget(), now, r)
			if a.OK { // the clock may go back between admission and reservation (review 3b-3 R2)
				b = reserve(b, now.Add(-time.Duration(back)*time.Second))
			}
			st.setBudget(b)
			if !validAttempts(st.Attempts) {
				t.Fatalf("%s: stored budget invalid after admit at %v: %v", name, now, st.Attempts)
			}
			if a.OK && (a.Attempt < 1 || a.Attempt > r.MaxAttempts) {
				t.Fatalf("%s: admitted attempt %d", name, a.Attempt)
			}
		}
	})
}
