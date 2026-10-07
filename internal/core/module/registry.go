package module

import (
	"errors"
	"fmt"
	"regexp"
	"slices"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/config"
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
	m := f()
	if m == nil {
		return errors.New("module: factory returned nil")
	}
	return r.add(m.Name(), entry{availability: config.ModuleAvailable, factory: f})
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
// into one *config.ValidationError.
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
		m := e.factory()
		if m.Name() != mc.Name {
			problems = append(problems, config.Problem{Path: path,
				Message: fmt.Sprintf("factory returned module %q", m.Name())})
			continue
		}
		configured, err := safeConfigure(m, mc)
		if err != nil {
			problems = append(problems, config.ProblemsOf(err, "", path)...)
			continue
		}
		out = append(out, Instance{Name: mc.Name, Module: configured})
	}
	if len(problems) > 0 {
		return nil, &config.ValidationError{Problems: problems}
	}
	return out, nil
}

// safeConfigure turns a panic in a module's Configure into an error, so
// one broken module cannot take down the whole configuration step.
func safeConfigure(m Module, mc config.ModuleConfig) (c Configured, err error) {
	defer func() {
		if p := recover(); p != nil {
			c, err = nil, fmt.Errorf("module %q panicked while configuring: %v", mc.Name, p)
		}
	}()
	c, err = m.Configure(mc)
	if err == nil && c == nil {
		err = fmt.Errorf("module %q returned no configured module", mc.Name)
	}
	return c, err
}

// Names returns the registered names in order, for diagnostics.
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.entries))
	for name := range r.entries {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}
