package module

import (
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/config"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/model"
)

// nameRe is the format of module names: short, lower case, usable as a
// YAML key, a directory name and a CLI word.
var nameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// Registry records the modules a binary knows: available ones with their
// factory, planned ones not yet implemented, and ones excluded from this
// build. The daemon fills it explicitly at start-up.
type Registry struct {
	entries map[string]entry
}

type entry struct {
	availability config.ModuleAvailability
	factory      Factory
}

// Instance is a configured module, ready to start.
type Instance struct {
	Name   string
	Module Configured
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{entries: map[string]entry{}}
}

// Register adds an available module. Its name is taken from the module.
func (r *Registry) Register(f Factory) error {
	if f == nil {
		return errors.New("module: nil factory")
	}
	name, err := moduleName(f)
	if err != nil {
		return err
	}
	return r.add(name, entry{availability: config.ModuleAvailable, factory: f})
}

// moduleName builds a module with f and returns its name. Like
// safeConfigure, it is a recovery boundary: a broken module makes
// registration fail instead of crashing start-up.
func moduleName(f Factory) (name string, err error) {
	defer func() {
		if recover() != nil {
			name, err = "", errors.New("module: factory panicked while registering")
		}
	}()
	m := f()
	if isNil(m) {
		return "", errors.New("module: factory returned nil")
	}
	return m.Name(), nil
}

// Planned records a module that is on the roadmap but not implemented in
// this release: enabling it is a configuration error.
func (r *Registry) Planned(name string) error {
	return r.add(name, entry{availability: config.ModulePlanned})
}

// NotBuilt records a module excluded from this binary by a build tag:
// enabling it is a configuration error.
func (r *Registry) NotBuilt(name string) error {
	return r.add(name, entry{availability: config.ModuleNotBuilt})
}

func (r *Registry) add(name string, e entry) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("module: invalid name %q (must match %s)", name, nameRe)
	}
	if name == model.ModuleCore { // the daemon's own events, state and commands
		return fmt.Errorf("module: the name %q is reserved for the core", name)
	}
	if _, dup := r.entries[name]; dup {
		return fmt.Errorf("module: %q registered twice", name)
	}
	r.entries[name] = e
	return nil
}

// Availability returns every known module name with its availability, as
// config.LoadOptions.Modules expects.
func (r *Registry) Availability() map[string]config.ModuleAvailability {
	out := make(map[string]config.ModuleAvailability, len(r.entries))
	for name, e := range r.entries {
		out[name] = e.availability
	}
	return out
}

// Configure configures every enabled module of cfg, in name order, and
// returns them ready to start. All problems of all modules are collected
// into one *config.ValidationError. The modules' warnings (Warner, or a
// ValidationError's) are added to cfg.Warnings, so that whoever reports the
// loader's warnings reports them too. Problems and warnings are redacted
// with cfg's secrets: a module may quote the values it was given.
func (r *Registry) Configure(cfg *config.Config) ([]Instance, error) {
	return r.configure(cfg, func(mc config.ModuleConfig) bool { return mc.Enabled })
}

// Validate configures every available module that has configuration,
// enabled or not, and discards the result (`sentinelctl validate --all`,
// with config.LoadOptions.IncludeDisabled).
func (r *Registry) Validate(cfg *config.Config) error {
	_, err := r.configure(cfg, func(mc config.ModuleConfig) bool {
		return mc.Availability == config.ModuleAvailable
	})
	return err
}

func (r *Registry) configure(cfg *config.Config, include func(config.ModuleConfig) bool) ([]Instance, error) {
	var (
		out      []Instance
		problems []config.Problem
	)
	for _, mc := range cfg.Modules {
		if !include(mc) {
			continue
		}
		path := "modules." + mc.Name
		e, ok := r.entries[mc.Name]
		if !ok || e.availability != config.ModuleAvailable {
			problems = append(problems, config.Problem{Path: path, Message: "module is not available in this binary"})
			continue
		}
		configured, warnings, err := safeConfigure(e.factory, mc)
		cfg.Warnings = append(cfg.Warnings, cfg.RedactProblems(warnings)...)
		if err != nil {
			problems = append(problems, cfg.RedactProblems(config.ProblemsOf(err, "", path))...)
			continue
		}
		out = append(out, Instance{Name: mc.Name, Module: configured})
	}
	if len(problems) > 0 {
		return nil, &config.ValidationError{Problems: problems}
	}
	return out, nil
}

// warningsOf returns the warnings of a module's Configure: those of its
// ValidationError when it failed, those of the module (Warner) otherwise.
func warningsOf(c Configured, err error) []config.Problem {
	if err != nil {
		var ve *config.ValidationError
		if errors.As(err, &ve) {
			return slices.Clone(ve.Warnings)
		}
		return nil
	}
	if w, ok := c.(Warner); ok {
		return slices.Clone(w.Warnings())
	}
	return nil
}

// safeConfigure builds and configures a module, and collects its warnings
// (warningsOf), inside one recovery boundary, so a broken factory,
// Configure or Warnings cannot take down the whole configuration step. A
// panic is reported without its value, which could carry secrets (a URL
// with a token, a header). The value and the stack are discarded for now;
// Phase 2c decides how the daemon records them through its redacting
// logger.
func safeConfigure(factory Factory, mc config.ModuleConfig) (c Configured, warnings []config.Problem, err error) {
	defer func() {
		if p := recover(); p != nil {
			c, warnings, err = nil, nil, fmt.Errorf("module %q panicked while configuring", mc.Name)
		}
	}()
	m := factory()
	if isNil(m) {
		return nil, nil, fmt.Errorf("module %q: factory returned nil", mc.Name)
	}
	if m.Name() != mc.Name {
		return nil, nil, fmt.Errorf("factory returned module %q", m.Name())
	}
	c, err = m.Configure(mc)
	if err == nil && isNil(c) {
		err = fmt.Errorf("module %q returned no configured module", mc.Name)
	}
	return c, warningsOf(c, err), err
}

// isNil reports whether v is nil or an interface holding a nil pointer,
// map, slice, channel or function (a "typed nil").
func isNil(v any) bool {
	if v == nil {
		return true
	}
	switch rv := reflect.ValueOf(v); rv.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Chan, reflect.Func, reflect.Interface:
		return rv.IsNil()
	}
	return false
}
