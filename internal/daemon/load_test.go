package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
	reg, err := Modules(nil)
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

// B-024: validate reads the CA bundle of every enabled channel, as the
// dispatcher does at start, and reports a bad one at its line with the
// other problems; a disabled channel is never built, so its bundle is not
// read. Load (start and reload) is unchanged: the dispatcher reports it.
func TestValidateReadsChannelCABundles(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad-ca-bundle-0123456789.pem")
	if err := os.WriteFile(bad, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	channel := func(name, enabled string) string {
		return "    - name: " + name + "\n      type: webhook\n      enabled: " + enabled +
			"\n      url: https://x.org\n      tls:\n        ca_file: ${CA}\n"
	}
	t.Setenv("CA", bad)
	tests := []struct {
		name     string
		channels string
		want     []string // problems at each path, in order
	}{
		{"enabled", channel("w", "true"), []string{"notifications.channels[w].tls.ca_file"}},
		{"disabled", channel("w", "false"), nil},
		{"two enabled", channel("a", "true") + channel("b", "true"),
			[]string{"notifications.channels[a].tls.ca_file", "notifications.channels[b].tls.ca_file"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			main := writeTree(t, "notifications:\n  channels:\n"+tt.channels)
			_, err := Validate(testRegistry(t), main, false)
			got := config.ProblemsOf(err, "", "")
			if len(got) != len(tt.want) {
				t.Fatalf("problems = %v, want %d at %v", got, len(tt.want), tt.want)
			}
			for i, p := range got {
				if p.Path != tt.want[i] || !strings.HasPrefix(p.Message, "line ") || !strings.Contains(p.Message, "holding PEM certificates") {
					t.Errorf("problem %d = %s, want at %s with a line", i, p, tt.want[i])
				}
				// The path came from the environment: it is a secret.
				if strings.Contains(p.String(), "0123456789") {
					t.Errorf("problem quotes the expanded value: %s", p)
				}
			}
			if _, _, err := Load(testRegistry(t), main); err != nil {
				t.Errorf("Load reads CA bundles: %v", err)
			}
		})
	}
}

// Review 3e-2 (CR R1, C2 R1, LR R1): a CA bundle error names no part of
// the path, which may come from the environment: masking the whole value
// does not hide its directory or file name on their own. The problem says
// what is wrong; the file, key and line say where.
func TestValidateChannelCAErrorsNameNoPath(t *testing.T) {
	dir := t.TempDir()
	unsafeDir := filepath.Join(dir, "unsafe-secret-0123456789")
	if err := os.Mkdir(unsafeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	unsafeCA := filepath.Join(unsafeDir, "ca.pem")
	if err := os.WriteFile(unsafeCA, []byte("not pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unsafeDir, 0o777); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unsafeDir, 0o700) })
	tests := []struct{ name, path, want string }{
		{"missing file", filepath.Join(dir, "missing-secret-0123456789.pem"), "does not exist"},
		{"missing directory", filepath.Join(dir, "nodir-secret-0123456789", "ca.pem"), "does not exist"},
		{"writable directory", unsafeCA, "unsafe path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CA", tt.path)
			main := writeTree(t, "notifications:\n  channels:\n    - {name: w, type: webhook, url: https://x.org, tls: {ca_file: \"${CA}\"}}\n")
			_, err := Validate(testRegistry(t), main, false)
			got := config.ProblemsOf(err, "", "")
			if len(got) != 1 || got[0].Path != "notifications.channels[w].tls.ca_file" || !strings.Contains(got[0].Message, tt.want) {
				t.Fatalf("problems = %v, want one at tls.ca_file containing %q", got, tt.want)
			}
			if strings.Contains(err.Error(), "0123456789") || strings.Contains(err.Error(), ".pem") {
				t.Errorf("problem names part of the path: %v", err)
			}
		})
	}
}

// A channel with another problem is not built, so its CA bundle is not
// read: the problem would be beside the point (review 3e-2, T5).
func TestValidateSkipsTheCABundleOfABadChannel(t *testing.T) {
	t.Setenv("CA", filepath.Join(t.TempDir(), "missing.pem"))
	main := writeTree(t, "notifications:\n  channels:\n    - {name: w, type: webhook, url: https://x.org, method: GET, tls: {ca_file: \"${CA}\"}}\n")
	_, err := Validate(testRegistry(t), main, false)
	got := config.ProblemsOf(err, "", "")
	if len(got) != 1 || got[0].Path != "notifications.channels[w].method" {
		t.Errorf("problems = %v, want the method's only", got)
	}
}

// Q-A of review 3e-2: validate warns about programs that will run as root
// whoever runs it, since sentineld runs as root (D-105). A group member
// validating the files must see what the daemon will do.
func TestValidationAssumesRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("the point is a validate run by another user")
	}
	dir := t.TempDir()
	main := filepath.Join(dir, "sentinel.yaml")
	files := map[string]string{
		main: "version: 1\ndaemon:\n  state_dir: " + filepath.Join(dir, "state") +
			"\nmodules:\n  supervisor:\n    enabled: true\n    processes: true\n",
		filepath.Join(dir, "supervisor", "a.yaml"): "version: 1\nservices:\n  - {name: w, command: /bin/sh}\n",
	}
	for path, body := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	reg, err := ValidationModules()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Validate(reg, main, false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(cfg.Warnings, func(p config.Problem) bool { return strings.Contains(p.Message, "runs as root") }) {
		t.Errorf("no root warning in %v", cfg.Warnings)
	}
	// The daemon's own registry keeps the real euid: not root here.
	reg, err = Modules(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg, err := Validate(reg, main, false); err != nil || slices.ContainsFunc(cfg.Warnings, func(p config.Problem) bool { return strings.Contains(p.Message, "runs as root") }) {
		t.Errorf("Modules warns as root: %v %v", err, cfg)
	}
}
