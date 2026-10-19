package supervisor

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/config"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/model"
)

// Limits and defaults (D-058: a service needs only its name and URL).
const (
	maxServices = 256 // D-080 (2)

	defaultCheckInterval = 30 * time.Second
	minCheckInterval     = time.Second
	maxCheckInterval     = 24 * time.Hour

	defaultTimeout = 10 * time.Second
	minTimeout     = 100 * time.Millisecond
	maxTimeout     = 5 * time.Minute

	defaultFailures  = 3
	defaultSuccesses = 1
	maxThreshold     = 100

	defaultMaxConcurrentChecks = 16

	maxURLBytes = 2048
)

// nameRe is the format of service names (D-011).
var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// settings are the module-wide settings, defaults applied.
type settings struct {
	MaxConcurrentChecks int
	CheckInterval       time.Duration
	Timeout             time.Duration
	Policy              Policy
}

// Service is a validated service definition, defaults applied.
type Service struct {
	Name          string
	Type          string // "http"
	URL           *url.URL
	CheckInterval time.Duration
	Timeout       time.Duration
	Policy        Policy
	Route         config.Route
	HTTP          HTTP
}

// The YAML shapes. Pointer fields tell a missing key (the default) from an
// explicit zero (an error).
type (
	policyYAML struct {
		ConsecutiveFailures  *int `yaml:"consecutive_failures"`
		ConsecutiveSuccesses *int `yaml:"consecutive_successes"`
	}
	settingsYAML struct {
		MaxConcurrentChecks *int             `yaml:"max_concurrent_checks"`
		CheckInterval       *config.Duration `yaml:"check_interval"`
		Timeout             *config.Duration `yaml:"timeout"`
		FailurePolicy       policyYAML       `yaml:"failure_policy"`
	}
	serviceYAML struct {
		Name          string           `yaml:"name"`
		Type          string           `yaml:"type"`
		URL           string           `yaml:"url"`
		CheckInterval *config.Duration `yaml:"check_interval"`
		Timeout       *config.Duration `yaml:"timeout"`
		FailurePolicy policyYAML       `yaml:"failure_policy"`
		Notifications config.Route     `yaml:"notifications"`
		// Recovery is decoded only to be refused with a reason (D-012).
		Recovery any      `yaml:"recovery"`
		HTTP     httpYAML `yaml:",inline"`
	}
	// typeYAML reads a service's type alone, before the rest is decoded
	// for that type.
	typeYAML struct {
		Type string         `yaml:"type"`
		Rest map[string]any `yaml:",inline"`
	}
)

// place is where something is defined in the configuration.
type place struct {
	file string
	line int
}

// parsed is what a module configuration says, once checked.
type parsed struct {
	settings settings
	services []Service // in file order
	where    []place   // where each service is defined
	problems []config.Problem
	warnings []config.Problem
}

// parse decodes and validates the supervisor's configuration. Every
// problem of every file is collected; problems locate keys and lines and
// never quote a configured value, which may be a secret.
func parse(mc config.ModuleConfig) parsed {
	var p parsed
	var none struct{} // the central block holds only `enabled` (ADR-0015 rule 1)
	if err := mc.Central.Decode(&none); err != nil {
		p.problems = append(p.problems, config.ProblemsOf(err, mc.Central.File, "modules."+moduleName)...)
	}
	if !mc.DirExists {
		p.warnings = append(p.warnings, config.Problem{Path: "modules." + moduleName,
			Message: fmt.Sprintf("directory %s does not exist: the supervisor checks no services", mc.Dir)})
	}
	p.settings = p.parseSettings(mc.Files)
	seen := map[string]place{} // service name → where it is first defined
	for _, f := range mc.Files {
		p.checkKeys(f)
		items, err := f.Items("services")
		if err != nil {
			p.problems = append(p.problems, config.ProblemsOf(err, f.File, "services")...)
		}
		for i, item := range items {
			svc, ok := p.parseService(f, i, item, mc.Channels)
			if svc.Name != "" { // a valid name, even when the rest has problems
				if first, dup := seen[svc.Name]; dup {
					p.addf(item, item.KeyLine("name"), fmt.Sprintf("services[%d].name", i),
						"this name is already used by the service in %s at line %d", first.file, first.line)
					continue
				}
				seen[svc.Name] = place{f.File, item.KeyLine("name")}
			}
			if !ok {
				continue
			}
			if len(p.services) == maxServices {
				p.addf(item, item.Line(), "services", "more than %d services", maxServices)
				continue
			}
			p.services = append(p.services, svc)
			p.where = append(p.where, place{f.File, item.Line()})
		}
	}
	if len(p.services) == 0 && len(p.problems) == 0 && mc.DirExists {
		p.warnings = append(p.warnings, config.Problem{Path: "modules." + moduleName,
			Message: "no services are configured: the supervisor checks nothing"})
	}
	return p
}

// checkKeys reports the top-level keys of a supervisor file it does not
// know. Keys are never expanded (D-006), so quoting them is safe.
func (p *parsed) checkKeys(f config.Section) {
	for _, key := range f.Keys() {
		switch key {
		case "settings", "services":
		case "jobs":
			p.addf(f, f.KeyLine(key), "jobs", "jobs are planned but not implemented (Phase 3c)")
		default:
			p.addf(f, f.KeyLine(key), key, "unknown key %q (valid keys: services, settings)", key)
		}
	}
}

// parseSettings decodes the module's settings block (the loader allows
// at most one per module directory) and applies the defaults.
func (p *parsed) parseSettings(files []config.Section) settings {
	s := settings{MaxConcurrentChecks: defaultMaxConcurrentChecks, CheckInterval: defaultCheckInterval,
		Timeout: defaultTimeout, Policy: Policy{Failures: defaultFailures, Successes: defaultSuccesses}}
	for _, f := range files {
		block, err := f.Child("settings")
		if err != nil {
			p.problems = append(p.problems, config.ProblemsOf(err, f.File, "settings")...)
			continue
		}
		if block.IsZero() {
			continue
		}
		var y settingsYAML
		bad := p.decode(block, &y, "settings")
		if y.MaxConcurrentChecks != nil && !bad[block.KeyLine("max_concurrent_checks")] {
			if n := *y.MaxConcurrentChecks; n < 1 || n > maxServices {
				p.addf(block, block.KeyLine("max_concurrent_checks"), "settings.max_concurrent_checks",
					"must be between 1 and %d", maxServices)
			} else {
				s.MaxConcurrentChecks = n
			}
		}
		s.CheckInterval = p.interval(block, bad, "settings", y.CheckInterval, s.CheckInterval)
		s.Timeout = p.timeout(block, bad, "settings", y.Timeout, s.Timeout)
		s.Policy = p.policy(block, bad, "settings", y.FailurePolicy, s.Policy)
	}
	return s
}

// parseService decodes and checks the i-th service of f; ok is false
// when it has problems. Problems locate it by position and line, never by
// its name, which may be expanded from the environment.
func (p *parsed) parseService(f config.Section, i int, item config.Section, channels []string) (Service, bool) {
	before := len(p.problems)
	path := fmt.Sprintf("services[%d]", i)
	var t typeYAML
	if err := item.Decode(&t); err != nil {
		p.problems = append(p.problems, config.ProblemsOf(err, f.File, path)...)
		return Service{}, false
	}
	switch t.Type {
	case "http":
	case "":
		if !item.Has("url") {
			p.addf(item, item.Line(), path, "url is required (an http service)")
			return Service{}, false
		}
	case "process", "systemd":
		p.addf(item, item.KeyLine("type"), path+".type", "service type %q is planned but not implemented (Phase 3b, 3c)", t.Type)
		return Service{}, false
	default:
		p.addf(item, item.KeyLine("type"), path+".type", "unknown service type (valid: http; planned: process, systemd)")
		return Service{}, false
	}
	var y serviceYAML
	bad := p.decode(item, &y, path)
	name := ""
	if !nameRe.MatchString(y.Name) {
		p.addf(item, lineOr(item, "name"), path+".name", "a service needs a name of lower-case letters, digits, '.', '_' or '-' (at most 63, D-011)")
	} else {
		name = y.Name
	}
	if item.Has("recovery") {
		p.addf(item, item.KeyLine("recovery"), path+".recovery", "recovery is not supported on http services: there is nothing to restart (D-012)")
	}
	u, why := checkURL(y.URL)
	if why != "" {
		p.addf(item, lineOr(item, "url"), path+".url", "%s", why)
	}
	svc := Service{Name: y.Name, Type: "http", URL: u,
		CheckInterval: p.interval(item, bad, path, y.CheckInterval, p.settings.CheckInterval),
		Timeout:       p.timeout(item, bad, path, y.Timeout, p.settings.Timeout),
		Policy:        p.policy(item, bad, path, y.FailurePolicy, p.settings.Policy),
		Route:         y.Notifications,
		HTTP:          p.http(item, bad, path, y.HTTP, u)}
	for _, problem := range y.Notifications.Problems(f.File, path+".notifications", channels, eventTypes) {
		if line := item.KeyLine("notifications"); line > 0 {
			problem.Message = fmt.Sprintf("line %d: %s", line, problem.Message)
		}
		p.problems = append(p.problems, problem)
	}
	svc.Name = name
	return svc, len(p.problems) == before
}

// decode decodes s into v, records its problems and returns the lines
// they are on: a value that did not decode is not range-checked too.
func (p *parsed) decode(s config.Section, v any, path string) map[int]bool {
	err := s.Decode(v)
	if err == nil {
		return nil
	}
	bad := map[int]bool{}
	for _, problem := range config.ProblemsOf(err, s.File, path) {
		var line int
		if _, scanErr := fmt.Sscanf(problem.Message, "line %d:", &line); scanErr == nil {
			bad[line] = true
		}
		p.problems = append(p.problems, problem)
	}
	return bad
}

func (p *parsed) interval(s config.Section, bad map[int]bool, path string, v *config.Duration, def time.Duration) time.Duration {
	return p.duration(s, bad, path, "check_interval", v, def, minCheckInterval, maxCheckInterval)
}

func (p *parsed) timeout(s config.Section, bad map[int]bool, path string, v *config.Duration, def time.Duration) time.Duration {
	return p.duration(s, bad, path, "timeout", v, def, minTimeout, maxTimeout)
}

// duration returns v, or def when v is absent or did not decode, checking
// its range.
func (p *parsed) duration(s config.Section, bad map[int]bool, path, key string, v *config.Duration, def, lo, hi time.Duration) time.Duration {
	if v == nil || bad[s.KeyLine(key)] {
		return def
	}
	if d := v.Std(); d >= lo && d <= hi {
		return d
	}
	p.addf(s, s.KeyLine(key), path+"."+key, "must be between %s and %s", lo, hi)
	return def
}

// policy returns the failure_policy of s, each threshold defaulting to def.
func (p *parsed) policy(s config.Section, bad map[int]bool, path string, y policyYAML, def Policy) Policy {
	out := def
	block, _ := s.Child("failure_policy") // not a mapping: the decode reported it
	check := func(key string, v *int, dst *int) {
		if v == nil || bad[block.KeyLine(key)] {
			return
		}
		if *v < 1 || *v > maxThreshold {
			p.addf(s, lineOr(block, key), path+".failure_policy."+key, "must be between 1 and %d", maxThreshold)
			return
		}
		*dst = *v
	}
	check("consecutive_failures", y.ConsecutiveFailures, &out.Failures)
	check("consecutive_successes", y.ConsecutiveSuccesses, &out.Successes)
	return out
}

// addf records a problem at line of s (0: no line known).
func (p *parsed) addf(s config.Section, line int, path, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if line > 0 {
		msg = fmt.Sprintf("line %d: %s", line, msg)
	}
	p.problems = append(p.problems, config.Problem{File: s.File, Path: path, Message: msg})
}

// lineOr returns the line of key in s, or the line of s itself.
func lineOr(s config.Section, key string) int {
	if line := s.KeyLine(key); line > 0 {
		return line
	}
	return s.Line()
}

// checkURL parses an http service's URL. The reason it gives never quotes
// the URL, which may carry credentials or a token.
func checkURL(raw string) (*url.URL, string) {
	switch {
	case raw == "":
		return nil, "url is required"
	case len(raw) > maxURLBytes:
		return nil, fmt.Sprintf("url is longer than %d bytes", maxURLBytes)
	}
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return nil, "url does not parse"
	case u.Scheme != "http" && u.Scheme != "https":
		return nil, "url must start with http:// or https://"
	case u.Opaque != "" || u.Hostname() == "":
		return nil, "url has no host"
	case u.Fragment != "":
		return nil, "url must not have a fragment (#...)"
	}
	if port := u.Port(); port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return nil, "url has an invalid port"
		}
	}
	return u, ""
}

// eventTypes are the event types a service route may filter on.
var eventTypes = []model.EventType{EventServiceFailed, EventServiceRecovered}
