package supervisor

import (
	"os"
	"path/filepath"
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
	f.Add("version: 1\nservices: &s\n  - {name: a, url: \"http://h/\", recovery: {}}\nx: *s\n")
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
		c, err := New(Options{NewChecker: newFakes().build}).Configure(mc)
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
			if !nameRe.MatchString(s.def.Name) || s.def.URL == nil || s.def.URL.Hostname() == "" ||
				s.def.Timeout < minTimeout || s.def.CheckInterval < minCheckInterval {
				t.Fatalf("accepted service %+v", s.def)
			}
		}
	})
}
