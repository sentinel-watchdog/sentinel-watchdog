package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/config"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/module"
)

// strict is an available test module whose directory files may only hold
// `ok: true`.
type strict struct{}

func (strict) Name() string { return "strict" }

func (strict) Configure(mc config.ModuleConfig) (module.Configured, error) {
	var problems []config.Problem
	for _, f := range mc.Files {
		var doc struct {
			OK bool `yaml:"ok"`
		}
		if err := f.Decode(&doc); err != nil {
			problems = append(problems, config.ProblemsOf(err, f.File, "")...)
		} else if !doc.OK {
			problems = append(problems, config.Problem{File: f.File, Path: "ok", Message: "must be true"})
		}
	}
	if len(problems) > 0 {
		return nil, &config.ValidationError{Problems: problems}
	}
	return &testModule{name: "strict"}, nil
}

func testRegistry(t *testing.T) *module.Registry {
	t.Helper()
	reg, err := Modules()
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(func() module.Module { return strict{} }); err != nil {
		t.Fatal(err)
	}
	return reg
}

// writeTree writes the central file and a strict/ directory with one
// invalid file.
func writeTree(t *testing.T, central string) string {
	t.Helper()
	dir := t.TempDir()
	main := filepath.Join(dir, "sentinel.yaml")
	body := "version: 1\ndaemon:\n  state_dir: " + filepath.Join(dir, "state") + "\n" + central
	if err := os.WriteFile(main, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "strict"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "strict", "10-bad.yaml"), []byte("version: 1\nok: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return main
}

// validate --all reads the directories of available modules that are
// disabled or not named; a plain validate does not.
func TestValidateAll(t *testing.T) {
	tests := []struct {
		name    string
		central string
		all     bool
		invalid bool
	}{
		{"enabled", "modules:\n  strict: {enabled: true}\n", false, true},
		{"disabled", "modules:\n  strict: {enabled: false}\n", false, false},
		{"disabled, all", "modules:\n  strict: {enabled: false}\n", true, true},
		{"not named", "", false, false},
		{"not named, all", "", true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Validate(testRegistry(t), writeTree(t, tt.central), tt.all)
			var ve *config.ValidationError
			if tt.invalid != errors.As(err, &ve) {
				t.Fatalf("err = %v, want invalid %v", err, tt.invalid)
			}
			if tt.invalid && (len(ve.Problems) != 1 || ve.Problems[0].Message != "must be true") {
				t.Errorf("problems %+v", ve.Problems)
			}
			if cfg == nil {
				t.Error("configuration not returned with module problems")
			}
		})
	}
}

// Load returns the enabled modules ready to start, and nothing else.
func TestLoad(t *testing.T) {
	main := writeTree(t, "modules:\n  strict: {enabled: false}\n")
	cfg, mods, err := Load(testRegistry(t), main)
	if err != nil || cfg == nil || len(mods) != 0 {
		t.Fatalf("Load: %v, %d modules", err, len(mods))
	}
	if _, _, err := Load(testRegistry(t), filepath.Join(t.TempDir(), "none.yaml")); err == nil {
		t.Error("missing file accepted")
	}
}
