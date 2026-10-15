package module_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/config"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/module"
)

// echo is a test module: its central block holds a `mode` gate, its
// directory holds `items`, and it requires the directory to exist.
type echo struct{ name string }

type echoConfigured struct {
	mode  string
	items []string
}

func (e echo) Name() string { return e.name }

func (e echo) Configure(mc config.ModuleConfig) (module.Configured, error) {
	gates := struct {
		Mode string `yaml:"mode"`
	}{Mode: "read_only"}
	var problems []config.Problem
	if err := mc.Central.Decode(&gates); err != nil {
		problems = append(problems, config.ProblemsOf(err, "", "modules."+e.name)...)
	}
	if !mc.DirExists {
		problems = append(problems, config.Problem{Path: "modules." + e.name, Message: "needs the directory " + mc.Dir})
	}
	out := &echoConfigured{mode: gates.Mode}
	for _, f := range mc.Files {
		var doc struct {
			Items []string `yaml:"items"`
		}
		if err := f.Decode(&doc); err != nil {
			problems = append(problems, config.ProblemsOf(err, f.File, "")...)
			continue
		}
		out.items = append(out.items, doc.Items...)
	}
	if len(problems) > 0 {
		return nil, &config.ValidationError{Problems: problems}
	}
	return out, nil
}

func (*echoConfigured) Start(context.Context, module.Runtime) error { return nil }
func (*echoConfigured) Stop(context.Context) error                  { return nil }

// misbehaving modules
type panicky struct{}

func (panicky) Name() string { return "panicky" }
func (panicky) Configure(config.ModuleConfig) (module.Configured, error) {
	panic("dial https://user:hunter2@example.org failed") // payload carries a secret
}

type silent struct{}

func (silent) Name() string                                             { return "silent" }
func (silent) Configure(config.ModuleConfig) (module.Configured, error) { return nil, nil }

func newRegistry(t *testing.T) *module.Registry {
	t.Helper()
	r := module.NewRegistry()
	for _, err := range []error{
		r.Register(func() module.Module { return echo{name: "echo"} }),
		r.Register(func() module.Module { return echo{name: "other"} }),
		r.Register(func() module.Module { return panicky{} }),
		r.Register(func() module.Module { return silent{} }),
		r.Planned("future"),
		r.NotBuilt("slim"),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	return r
}

// loadTree writes files into a private temporary directory and loads them.
func loadTree(t *testing.T, r *module.Registry, files map[string]string, includeDisabled bool) *config.Config {
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
	cfg, err := config.Load(config.LoadOptions{
		MainFile:        filepath.Join(dir, "sentinel.yaml"),
		Modules:         r.Availability(),
		IncludeDisabled: includeDisabled,
		Lookup:          func(string) (string, bool) { return "", false },
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

func requireProblem(t *testing.T, err error, substrings ...string) {
	t.Helper()
	var ve *config.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %v, want *config.ValidationError", err)
	}
	for _, p := range ve.Problems {
		matched := true
		for _, s := range substrings {
			matched = matched && strings.Contains(p.String(), s)
		}
		if matched {
			return
		}
	}
	t.Fatalf("no problem contains %q in:\n%v", substrings, err)
}

func TestRegistryRegistration(t *testing.T) {
	r := newRegistry(t)
	want := map[string]config.ModuleAvailability{
		"echo": config.ModuleAvailable, "other": config.ModuleAvailable,
		"panicky": config.ModuleAvailable, "silent": config.ModuleAvailable,
		"future": config.ModulePlanned, "slim": config.ModuleNotBuilt,
	}
	got := r.Availability()
	if len(got) != len(want) {
		t.Fatalf("Availability() = %v", got)
	}
	for name, a := range want {
		if got[name] != a {
			t.Errorf("%s: %s, want %s", name, got[name], a)
		}
	}
	if names := r.Names(); !slices.IsSorted(names) || len(names) != 6 {
		t.Errorf("Names() = %v", names)
	}

	tests := []struct {
		name string
		err  error
		want string
	}{
		{"duplicate", r.Planned("echo"), "registered twice"},
		{"invalid name", r.Planned("Bad-Name"), "invalid name"},
		{"nil factory", r.Register(nil), "nil factory"},
		{"factory returns nil", r.Register(func() module.Module { return nil }), "returned nil"},
		// A broken module must not crash start-up, nor leak its panic value.
		{"factory panics", r.Register(func() module.Module { panic("s3cret") }), "panicked while registering"},
		{"Name panics", r.Register(func() module.Module { return panicName{} }), "panicked while registering"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.err == nil || !strings.Contains(tt.err.Error(), tt.want) || strings.Contains(tt.err.Error(), "s3cret") {
				t.Errorf("err = %v, want %q without the panic value", tt.err, tt.want)
			}
		})
	}
}

// panicName is a module whose Name method panics.
type panicName struct{ module.Module }

func (panicName) Name() string { panic("s3cret") }

func TestConfigureEnabledModules(t *testing.T) {
	r := newRegistry(t)
	cfg := loadTree(t, r, map[string]string{
		"sentinel.yaml": `
version: 1
modules:
  echo: {enabled: true, mode: enforce}
  other: {enabled: false}
  future: {enabled: false}
`,
		"echo/10-a.yaml": "version: 1\nitems: [a, b]\n",
		"echo/20-b.yaml": "version: 1\nitems: [c]\n",
	}, false)

	instances, err := r.Configure(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(instances) != 1 || instances[0].Name != "echo" {
		t.Fatalf("instances = %+v, want only the enabled echo module", instances)
	}
	e := instances[0].Module.(*echoConfigured)
	if e.mode != "enforce" || !slices.Equal(e.items, []string{"a", "b", "c"}) {
		t.Errorf("echo configured as %+v", e)
	}
}

func TestConfigureCollectsProblemsFromAllModules(t *testing.T) {
	r := newRegistry(t)
	cfg := loadTree(t, r, map[string]string{
		"sentinel.yaml": `
version: 1
modules:
  echo: {enabled: true, mod: typo}
  other: {enabled: true}
  panicky: {enabled: true}
  silent: {enabled: true}
`,
		"echo/10-a.yaml": "version: 1\nitem: [a]\n",
	}, false)

	_, err := r.Configure(cfg)
	requireProblem(t, err, "sentinel.yaml", `unknown field "mod"`)
	requireProblem(t, err, "10-a.yaml", `line 2: unknown field "item"`)
	requireProblem(t, err, "modules.other", "needs the directory")
	requireProblem(t, err, "modules.panicky", "panicked while configuring")
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("panic payload leaked into the error: %v", err)
	}
	requireProblem(t, err, "modules.silent", "returned no configured module")
}

func TestValidateIncludesDisabledModules(t *testing.T) {
	r := newRegistry(t)
	files := map[string]string{
		"sentinel.yaml":   "version: 1\nmodules:\n  echo: {enabled: false}\n",
		"echo/10-a.yaml":  "version: 1\nitem: [a]\n",
		"other/10-a.yaml": "version: 1\nitems: [a]\n",
	}
	if _, err := r.Configure(loadTree(t, r, files, false)); err != nil {
		t.Fatalf("Configure looked at a disabled module: %v", err)
	}
	err := r.Validate(loadTree(t, r, files, true))
	requireProblem(t, err, "10-a.yaml", `unknown field "item"`)
}

func TestConfigureRejectsNameMismatch(t *testing.T) {
	r := module.NewRegistry()
	if err := r.Register(func() module.Module { return echo{name: "echo"} }); err != nil {
		t.Fatal(err)
	}
	calls := 0
	if err := r.Register(func() module.Module {
		calls++
		if calls == 1 {
			return echo{name: "drift"} // name used at registration
		}
		return echo{name: "echo"} // a later call disagrees
	}); err != nil {
		t.Fatal(err)
	}
	cfg := loadTree(t, r, map[string]string{
		"sentinel.yaml": "version: 1\nmodules:\n  drift: {enabled: true}\n",
		"drift/a.yaml":  "version: 1\n",
	}, false)
	_, err := r.Configure(cfg)
	requireProblem(t, err, "modules.drift", `factory returned module "echo"`)
}

// typedNil returns a nil *echoConfigured as a module.Configured: not equal
// to nil as an interface, but unusable.
type typedNil struct{}

func (typedNil) Name() string { return "typednil" }
func (typedNil) Configure(config.ModuleConfig) (module.Configured, error) {
	var c *echoConfigured
	return c, nil
}

func TestConfigureSurvivesBrokenFactories(t *testing.T) {
	calls := map[string]int{}
	later := func(name string, broken func() module.Module) module.Factory {
		return func() module.Module {
			calls[name]++
			if calls[name] == 1 {
				return echo{name: name} // fine at registration
			}
			return broken()
		}
	}
	r := module.NewRegistry()
	for _, f := range []module.Factory{
		later("panics", func() module.Module { panic("factory exploded") }),
		later("nils", func() module.Module { return nil }),
		later("typednils", func() module.Module { var e *echo; return e }),
		func() module.Module { return typedNil{} },
	} {
		if err := r.Register(f); err != nil {
			t.Fatal(err)
		}
	}
	cfg := loadTree(t, r, map[string]string{"sentinel.yaml": `
version: 1
modules:
  panics: {enabled: true}
  nils: {enabled: true}
  typednils: {enabled: true}
  typednil: {enabled: true}
`}, false)
	_, err := r.Configure(cfg)
	requireProblem(t, err, "modules.panics", "panicked while configuring")
	requireProblem(t, err, "modules.nils", "factory returned nil")
	requireProblem(t, err, "modules.typednils", "factory returned nil")
	requireProblem(t, err, "modules.typednil", "returned no configured module")
}

// risky is a test module whose configuration is legal but risky: it warns,
// quoting the token it was given, and fails as well when its central block
// asks for it.
type risky struct{}

type riskyConfigured struct{ warnings []config.Problem }

func (risky) Name() string { return "risky" }
func (risky) Configure(mc config.ModuleConfig) (module.Configured, error) {
	var c struct {
		Token string `yaml:"token"`
		Fail  bool   `yaml:"fail"`
	}
	if err := mc.Central.Decode(&c); err != nil {
		return nil, err
	}
	warning := config.Problem{Path: "modules.risky.token", Message: "weak token " + c.Token}
	if c.Fail {
		return nil, &config.ValidationError{
			Problems: []config.Problem{{Path: "modules.risky.fail", Message: "refused with token " + c.Token}},
			Warnings: []config.Problem{warning},
		}
	}
	return &riskyConfigured{warnings: []config.Problem{warning}}, nil
}

func (*riskyConfigured) Start(context.Context, module.Runtime) error { return nil }
func (*riskyConfigured) Stop(context.Context) error                  { return nil }
func (c *riskyConfigured) Warnings() []config.Problem                { return c.warnings }

// A module's warnings reach cfg.Warnings whether it succeeds or fails, and
// its problems and warnings never quote a secret of the configuration.
func TestConfigureReportsModuleWarningsRedacted(t *testing.T) {
	const token = "tok-s3cret-value"
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("fail=%v", fail), func(t *testing.T) {
			r := module.NewRegistry()
			if err := r.Register(func() module.Module { return risky{} }); err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			main := filepath.Join(dir, "sentinel.yaml")
			body := fmt.Sprintf("version: 1\nmodules:\n  risky:\n    enabled: true\n    token: ${TOKEN}\n    fail: %v\n", fail)
			if err := os.WriteFile(main, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(dir, "risky"), 0o700); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(config.LoadOptions{MainFile: main, Modules: r.Availability(),
				Lookup: func(k string) (string, bool) { return token, k == "TOKEN" }})
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			_, err = r.Configure(cfg)
			if (err != nil) != fail {
				t.Fatalf("Configure error = %v, want failure %v", err, fail)
			}
			if fail {
				requireProblem(t, err, "modules.risky.fail", "refused with token")
				if strings.Contains(err.Error(), token) {
					t.Errorf("module problem quotes the secret: %v", err)
				}
			}
			var found bool
			for _, w := range cfg.Warnings {
				if strings.Contains(w.String(), token) {
					t.Errorf("module warning quotes the secret: %s", w)
				}
				found = found || strings.Contains(w.String(), "modules.risky.token")
			}
			if !found {
				t.Errorf("module warning missing from cfg.Warnings: %v", cfg.Warnings)
			}
		})
	}
}

// warnPanics is a module whose Warnings panics: the registry must contain
// it like a panic in Configure, without the payload (review R1).
type warnPanics struct{}

type warnPanicsConfigured struct{ riskyConfigured }

func (warnPanics) Name() string { return "warnpanics" }
func (warnPanics) Configure(config.ModuleConfig) (module.Configured, error) {
	return &warnPanicsConfigured{}, nil
}
func (*warnPanicsConfigured) Warnings() []config.Problem { panic("hunter2") }

func TestConfigureContainsAPanicInWarnings(t *testing.T) {
	r := module.NewRegistry()
	if err := r.Register(func() module.Module { return warnPanics{} }); err != nil {
		t.Fatal(err)
	}
	_, err := r.Configure(&config.Config{Modules: []config.ModuleConfig{{Name: "warnpanics", Enabled: true}}})
	requireProblem(t, err, "modules.warnpanics", "panicked while configuring")
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("panic payload leaked into the error: %v", err)
	}
}
