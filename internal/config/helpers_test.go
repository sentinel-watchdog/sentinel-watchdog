package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// env returns a LookupEnv backed by a map.
func env(vars map[string]string) LookupEnv {
	return func(name string) (string, bool) {
		v, ok := vars[name]
		return v, ok
	}
}

// writeTree writes main.yaml and conf.d fragments into a temp dir and
// returns the main file path.
func writeTree(t *testing.T, main string, fragments map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "sentinel.yaml")
	if err := os.WriteFile(mainPath, []byte(main), 0o600); err != nil {
		t.Fatal(err)
	}
	if len(fragments) > 0 {
		if err := os.Mkdir(filepath.Join(dir, "conf.d"), 0o700); err != nil {
			t.Fatal(err)
		}
		for name, body := range fragments {
			if err := os.WriteFile(filepath.Join(dir, "conf.d", name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	return mainPath
}

func loadString(t *testing.T, main string, vars map[string]string) (*Result, error) {
	t.Helper()
	return Load(LoadOptions{MainFile: writeTree(t, main, nil), Lookup: env(vars)})
}

func mustLoad(t *testing.T, main string, vars map[string]string) *Config {
	t.Helper()
	res, err := loadString(t, main, vars)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return res.Config
}

// requireProblem asserts err is a *ValidationError mentioning all substrings
// in a single problem.
func requireProblem(t *testing.T, err error, substrings ...string) {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected *ValidationError, got %T: %v", err, err)
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

// baseConfig wraps supervisor YAML (already indented by two spaces) in a valid
// configuration with one webhook channel named "hook".
func baseConfig(supervisors string) string {
	return `version: 1
notifications:
  - name: hook
    type: webhook
    url: https://hooks.example.org/x
supervisors:
` + supervisors
}
