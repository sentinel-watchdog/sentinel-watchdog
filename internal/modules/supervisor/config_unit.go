package supervisor

import (
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/config"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/platform/systemd"
)

// Defaults and limits of systemd services (D-093).
const (
	defaultTransitionGrace = 90 * time.Second
	maxTransitionGrace     = 10 * time.Minute
)

// Unit is how a systemd service is observed, defaults applied.
type Unit struct {
	Name systemd.UnitName
	// TransitionGrace is how long a unit may stay unsettled (activating,
	// deactivating, reloading, auto-restart, a job queued) before that
	// counts as a failure; the whole unsettled period counts, however its
	// substate changes.
	TransitionGrace time.Duration
}

type unitServiceYAML struct {
	Name            string           `yaml:"name"`
	Type            string           `yaml:"type"`
	Unit            string           `yaml:"unit"`
	CheckInterval   *config.Duration `yaml:"check_interval"`
	Timeout         *config.Duration `yaml:"timeout"`
	TransitionGrace *config.Duration `yaml:"transition_grace"`
	FailurePolicy   policyYAML       `yaml:"failure_policy"`
	Notifications   config.Route     `yaml:"notifications"`
	Enabled         *bool            `yaml:"enabled"`
	// Decoded only to be refused with a reason.
	Recovery     any `yaml:"recovery"`
	Limits       any `yaml:"limits"`
	StartupGrace any `yaml:"startup_grace"`
}

// refusedUnitKeys are keys a systemd service does not take, and why.
var refusedUnitKeys = []struct{ key, why string }{
	{"recovery", "recovery of systemd services is planned for Phase 3c-2: in this version units are observed, never restarted"},
	{"limits", "limits are for process services: a unit has its own limits in systemd"},
	{"startup_grace", "startup_grace is for process services; a unit in transition has transition_grace"},
}

// parseUnit decodes and checks a systemd service. Problems name keys and
// lines, never the unit's value, which may be expanded from the
// environment.
func (p *parsed) parseUnit(f config.Section, path string, item config.Section, channels []string) (Service, bool) {
	before := len(p.problems)
	var y unitServiceYAML
	bad := p.decode(item, &y, path)
	name := p.serviceName(item, path, y.Name)
	for _, r := range refusedUnitKeys {
		if item.Has(r.key) {
			p.addf(item, item.KeyLine(r.key), path+"."+r.key, "%s", r.why)
		}
	}
	var unit systemd.UnitName
	switch {
	case !item.Has("unit"):
		p.addf(item, item.Line(), path, "unit is required: the systemd unit to observe, such as nginx.service")
	case bad[item.KeyLine("unit")]: // not a string: the decode reported it
	default:
		n, err := systemd.ParseUnitName(y.Unit)
		switch {
		case err != nil:
			p.addf(item, item.KeyLine("unit"), path+".unit", "%s", err) // its errors never quote the name
		case n.Type != systemd.TypeService:
			p.addf(item, item.KeyLine("unit"), path+".unit", "unit must be a .service unit: other unit types are not supervised")
		default:
			unit = n
		}
	}
	svc := Service{Name: name, Type: typeSystemd,
		CheckInterval: p.interval(item, bad, path, y.CheckInterval, p.settings.CheckInterval),
		Timeout:       p.timeout(item, bad, path, y.Timeout, p.settings.Timeout),
		Policy:        p.policy(item, bad, path, y.FailurePolicy, p.settings.Policy),
		Route:         y.Notifications,
		Unit: Unit{Name: unit, TransitionGrace: p.duration(item, bad, path, "transition_grace", y.TransitionGrace,
			defaultTransitionGrace, 0, maxTransitionGrace)},
		Disabled: y.Enabled != nil && !*y.Enabled}
	p.route(f, item, path, y.Notifications, channels)
	return svc, len(p.problems) == before
}
