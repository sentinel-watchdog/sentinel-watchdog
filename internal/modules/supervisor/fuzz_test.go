package supervisor

import (
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/config"
)

// FuzzConfigure: whatever a supervisor file holds, the loader and
// Configure never panic; a configuration they accept is within the
// module's limits, and no problem quotes the secret expanded into it
// (AGENTS.md: every loader of untrusted input has a Fuzz target).
func FuzzConfigure(f *testing.F) {
	f.Add(webService)
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
		if len(in.services) > maxServices || in.settings.MaxConcurrentChecks < 1 {
			t.Fatalf("accepted %d services, %d slots", len(in.services), in.settings.MaxConcurrentChecks)
		}
		for _, s := range in.services {
			if s.def.Type == typeProcess {
				checkAcceptedProcess(t, s.def)
				continue
			}
			if s.def.Type != typeHTTP || !nameRe.MatchString(s.def.Name) || s.def.URL == nil || s.def.URL.Hostname() == "" ||
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
	if !nameRe.MatchString(def.Name) || def.URL != nil || !filepath.IsAbs(pr.Command) || !filepath.IsAbs(pr.WorkingDir) ||
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
}
