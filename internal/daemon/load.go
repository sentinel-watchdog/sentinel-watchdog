// Package daemon is the composition root of sentineld (ADR-0015): it knows
// every module, loads the configuration, gives each module its runtime and
// runs the modules' lifecycle around the core services (events,
// notifications, state).
package daemon

import (
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/config"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/module"
)

// planned lists the modules on the roadmap that no release implements yet:
// enabling one is a configuration error, not a silent no-op.
var planned = []string{"firewall", "remote", "supervisor"}

// Modules returns the registry of the modules this binary knows. Each
// implemented module replaces its Planned entry with Register.
func Modules() (*module.Registry, error) {
	r := module.NewRegistry()
	for _, name := range planned {
		if err := r.Planned(name); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// Load reads the configuration at mainFile and configures every enabled
// module of reg. It changes nothing on the system. On error, cfg is set if
// the files themselves were valid (its warnings are worth reporting).
func Load(reg *module.Registry, mainFile string) (*config.Config, []module.Instance, error) {
	cfg, err := config.Load(config.LoadOptions{MainFile: mainFile, Modules: reg.Availability()})
	if err != nil {
		return nil, nil, err
	}
	mods, err := reg.Configure(cfg)
	if err != nil {
		return cfg, nil, err
	}
	return cfg, mods, nil
}

// Validate checks the configuration like Load and discards the modules.
// With all, it also validates the directories of available modules that
// are disabled or not named in the central file (`validate --all`).
func Validate(reg *module.Registry, mainFile string, all bool) (*config.Config, error) {
	cfg, err := config.Load(config.LoadOptions{MainFile: mainFile, Modules: reg.Availability(), IncludeDisabled: all})
	if err != nil {
		return nil, err
	}
	if all {
		return cfg, reg.Validate(cfg)
	}
	_, err = reg.Configure(cfg)
	return cfg, err
}
