package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testModules is the module catalogue used by loader tests.
var testModules = map[string]ModuleAvailability{
	"alpha":    ModuleAvailable,
	"beta":     ModuleAvailable,
	"planned":  ModulePlanned,
	"excluded": ModuleNotBuilt,
}

// env returns a LookupEnv backed by a map.
func env(vars map[string]string) LookupEnv {
	return func(name string) (string, bool) {
		v, ok := vars[name]
		return v, ok
	}
}

// writeTree writes files (paths relative to a fresh temporary directory,
// e.g. "sentinel.yaml" or "alpha/10-a.yaml") with private modes and
// returns the path of sentinel.yaml.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "sentinel.yaml")
}

// load writes files and loads them with testModules and vars.
func load(t *testing.T, files map[string]string, vars map[string]string) (*Config, error) {
	t.Helper()
	return Load(LoadOptions{MainFile: writeTree(t, files), Modules: testModules, Lookup: env(vars)})
}

func mustLoad(t *testing.T, files map[string]string, vars map[string]string) *Config {
	t.Helper()
	cfg, err := load(t, files, vars)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

// requireProblem fails unless err is a *ValidationError with a problem
// whose text contains every one of the substrings.
func requireProblem(t *testing.T, err error, substrings ...string) {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %v, want *ValidationError", err)
	}
	for _, p := range ve.Problems {
		s := p.String()
		ok := true
		for _, sub := range substrings {
			if !strings.Contains(s, sub) {
				ok = false
				break
			}
		}
		if ok {
			return
		}
	}
	t.Fatalf("no problem contains %q; got:\n%v", substrings, err)
}

func hasWarning(warnings []Problem, substring string) bool {
	for _, w := range warnings {
		if strings.Contains(w.String(), substring) {
			return true
		}
	}
	return false
}
