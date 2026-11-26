// Package daemon is the composition root of sentineld (ADR-0015): it knows
// every module, loads the configuration, gives each module its runtime and
// runs the modules' lifecycle around the core services (events,
// notifications, state).
package daemon

import (
	"errors"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/config"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/module"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/tlsconfig"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/modules/supervisor"
)

// planned lists the modules on the roadmap that no release implements yet:
// enabling one is a configuration error, not a silent no-op.
var planned = []string{"firewall", "remote"}

// Modules returns the registry of the modules this binary knows. Each
// implemented module replaces its Planned entry with Register. journal sends
// one native journal entry, or is nil when sentineld's stderr is not
// journald's: the supervisor then writes program output to the logger
// (D-100).
func Modules(journal func(entry []byte) error) (*module.Registry, error) {
	return registry(supervisor.Options{JournalSend: journal})
}

// ValidationModules returns the registry for `validate`, which sends
// nothing to the journal. sentineld runs as root, so the supervisor warns
// about programs that will run as root whoever validates (D-105): an
// operator in the socket group sees what the daemon will do.
func ValidationModules() (*module.Registry, error) {
	return registry(supervisor.Options{EUID: func() int { return 0 }})
}

// registry builds the registry with the supervisor's options.
func registry(opts supervisor.Options) (*module.Registry, error) {
	r := module.NewRegistry()
	if err := r.Register(func() module.Module { return supervisor.New(opts) }); err != nil {
		return nil, err
	}
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
// are disabled or not named in the central file (`validate --all`). It
// also reads the CA bundle of every enabled channel, as the dispatcher
// does at start (B-024).
func Validate(reg *module.Registry, mainFile string, all bool) (*config.Config, error) {
	cfg, err := config.Load(config.LoadOptions{MainFile: mainFile, Modules: reg.Availability(), IncludeDisabled: all,
		CheckCAFile: checkCAFile})
	if err != nil {
		return nil, err
	}
	if all {
		return cfg, reg.Validate(cfg)
	}
	_, err = reg.Configure(cfg)
	return cfg, err
}

// checkCAFile reads the CA bundle at path as notify.NewWebhook does, and
// says what is wrong without naming the path (tlsconfig.Reason).
func checkCAFile(path string) error {
	if _, err := tlsconfig.LoadCA(path); err != nil {
		return errors.New(tlsconfig.Reason(err))
	}
	return nil
}
