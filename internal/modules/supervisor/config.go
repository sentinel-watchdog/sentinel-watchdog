package supervisor

import (
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/config"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/api"
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
	defaultMaxConcurrentRuns   = 32

	maxURLBytes = 2048
)

// settings are the module-wide settings, defaults applied.
type settings struct {
	MaxConcurrentChecks int
	// MaxConcurrentRuns bounds the runs launched under concurrency_policy:
	// allow, in the whole module (Q4).
	MaxConcurrentRuns int
	CheckInterval     time.Duration
	Timeout           time.Duration
	Policy            Policy
}

// Service types.
const (
	typeHTTP    = "http"
	typeProcess = "process"
	typeSystemd = "systemd"
)

// Service is a validated service definition, defaults applied. The
// fields of the other types are zero: an http service has no Process or
// Unit, a process service no URL, interval, timeout, policy, HTTP or Unit,
// a systemd service no URL, HTTP or Process.
type Service struct {
	Name          string
	Type          string // typeHTTP, typeProcess or typeSystemd
	URL           *url.URL
	CheckInterval time.Duration
	Timeout       time.Duration
	Policy        Policy
	Route         config.Route
	HTTP          HTTP
	Process       Process
	Unit          Unit
	// Recovery is how a process service is started again, and how a
	// systemd service with Unit.HasRecovery is restarted. It is set, with
	// its defaults, for both types (a systemd service's flapping threshold
	// comes from it too, D-095), never for http services. Not in the
	// digest: changing it keeps the budget.
	Recovery Recovery
	// Disabled is `enabled: false`: the service is validated but not run
	// or checked, whatever an operator asks (D-020). It is not in the
	// digest: turning it off and on keeps the service's state.
	Disabled bool
}

// restarted reports whether sentineld starts s again after it fails:
// every process service, and a systemd service with recovery (D-087,
// D-095).
func (s Service) restarted() bool {
	return s.Type == typeProcess || s.Type == typeSystemd && s.Unit.HasRecovery
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
		MaxConcurrentRuns   *int             `yaml:"max_concurrent_runs"`
		CheckInterval       *config.Duration `yaml:"check_interval"`
		Timeout             *config.Duration `yaml:"timeout"`
		FailurePolicy       policyYAML       `yaml:"failure_policy"`
	}
	// centralYAML is the supervisor's block in the central file, without
	// `enabled` (ADR-0015 rule 1).
	centralYAML struct {
		// Processes arms process services (D-084 (1)); default false.
		Processes *bool `yaml:"processes"`
		// SystemdRestart arms the restarts of systemd services (D-095);
		// default false: they are observed only.
		SystemdRestart *bool `yaml:"systemd_restart"`
	}
	httpServiceYAML struct {
		Name          string           `yaml:"name"`
		Type          string           `yaml:"type"`
		URL           string           `yaml:"url"`
		CheckInterval *config.Duration `yaml:"check_interval"`
		Timeout       *config.Duration `yaml:"timeout"`
		FailurePolicy policyYAML       `yaml:"failure_policy"`
		Notifications config.Route     `yaml:"notifications"`
		Enabled       *bool            `yaml:"enabled"`
		// Recovery and Limits are decoded only to be refused with a reason
		// (D-012, D-090).
		Recovery any      `yaml:"recovery"`
		Limits   any      `yaml:"limits"`
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
	// processes is the central switch that lets process services run;
	// systemdRestart the one that lets systemd services be restarted.
	processes      bool
	systemdRestart bool
	settings       settings
	services       []Service // in file order
	where          []place   // where each service is defined
	jobs           []Job     // in file order
	jobWhere       []place   // where each job is defined
	problems       []config.Problem
	warnings       []config.Problem
}

// parse decodes and validates the supervisor's configuration. Every
// problem of every file is collected; problems locate keys and lines and
// never quote a configured value, which may be a secret.
func parse(mc config.ModuleConfig) parsed {
	var p parsed
	var central centralYAML
	if err := mc.Central.Decode(&central); err != nil {
		p.problems = append(p.problems, config.ProblemsOf(err, mc.Central.File, "modules."+moduleName)...)
	}
	p.processes = central.Processes != nil && *central.Processes
	p.systemdRestart = central.SystemdRestart != nil && *central.SystemdRestart
	if !mc.DirExists {
		p.warnings = append(p.warnings, config.Problem{Path: "modules." + moduleName,
			Message: fmt.Sprintf("directory %s does not exist: the supervisor checks no services", mc.Dir)})
	}
	p.settings = p.parseSettings(mc.Files)
	seen := map[string]place{}      // service name → where it is first defined
	seenUnits := map[string]place{} // unit name → its unit key in the service that first observes it
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
			if svc.Type == typeSystemd {
				unit := svc.Unit.Name.String()
				if first, dup := seenUnits[unit]; dup {
					p.addf(item, item.KeyLine("unit"), fmt.Sprintf("services[%d].unit", i),
						"this unit name is already observed by the service in %s at line %d", first.file, first.line)
					continue
				}
				seenUnits[unit] = place{f.File, item.KeyLine("unit")}
			}
			if len(p.services)+len(p.jobs) == maxServices {
				p.addf(item, item.Line(), "services", "more than %d services and jobs", maxServices)
				continue
			}
			p.services = append(p.services, svc)
			p.where = append(p.where, place{f.File, item.Line()})
		}
		p.parseJobs(f, seen, mc.Channels)
	}
	if len(p.services) == 0 && len(p.jobs) == 0 && len(p.problems) == 0 && mc.DirExists {
		p.warnings = append(p.warnings, config.Problem{Path: "modules." + moduleName,
			Message: "no services are configured: the supervisor checks nothing"})
	}
	if n := p.count(typeProcess); n > 0 && !p.processes {
		p.warnings = append(p.warnings, config.Problem{File: mc.Central.File, Path: "modules." + moduleName + ".processes",
			Message: fmt.Sprintf("%d process services are configured but modules.%s.processes is false: they are validated, not started", n, moduleName)})
	}
	if n := len(p.jobs); n > 0 && !p.processes {
		p.warnings = append(p.warnings, config.Problem{File: mc.Central.File, Path: "modules." + moduleName + ".processes",
			Message: fmt.Sprintf("%d jobs are configured but modules.%s.processes is false: they are validated, never run", n, moduleName)})
	}
	p.warnRestarts(mc.Central.File)
	return p
}

// warnRestarts warns when the switch of systemd restarts and the services
// that ask for them disagree: either is then without effect.
func (p *parsed) warnRestarts(central string) {
	n := 0
	for _, s := range p.services {
		if s.Type == typeSystemd && s.restarted() {
			n++
		}
	}
	path := "modules." + moduleName + ".systemd_restart"
	switch {
	case n > 0 && !p.systemdRestart:
		p.warnings = append(p.warnings, config.Problem{File: central, Path: path,
			Message: fmt.Sprintf("%d systemd services have recovery but modules.%s.systemd_restart is false: they are observed, not restarted", n, moduleName)})
	case n == 0 && p.systemdRestart && len(p.problems) == 0:
		p.warnings = append(p.warnings, config.Problem{File: central, Path: path,
			Message: "modules." + moduleName + ".systemd_restart is true but no systemd service has recovery: nothing is restarted"})
	}
}

// count returns how many valid services have type typ.
func (p *parsed) count(typ string) int {
	n := 0
	for _, s := range p.services {
		if s.Type == typ {
			n++
		}
	}
	return n
}

// checkKeys reports the top-level keys of a supervisor file it does not
// know. Keys are never expanded (D-006), so quoting them is safe.
func (p *parsed) checkKeys(f config.Section) {
	for _, key := range f.Keys() {
		switch key {
		case "settings", "services", "jobs":
		default:
			p.addf(f, f.KeyLine(key), key, "unknown key %q (valid keys: jobs, services, settings)", key)
		}
	}
}

// parseSettings decodes the module's settings block (the loader allows
// at most one per module directory) and applies the defaults.
func (p *parsed) parseSettings(files []config.Section) settings {
	s := settings{MaxConcurrentChecks: defaultMaxConcurrentChecks, MaxConcurrentRuns: defaultMaxConcurrentRuns, CheckInterval: defaultCheckInterval,
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
		if y.MaxConcurrentRuns != nil && !bad[block.KeyLine("max_concurrent_runs")] {
			if n := *y.MaxConcurrentRuns; n < 1 || n > maxServices {
				p.addf(block, block.KeyLine("max_concurrent_runs"), "settings.max_concurrent_runs",
					"must be between 1 and %d", maxServices)
			} else {
				s.MaxConcurrentRuns = n
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
	path := fmt.Sprintf("services[%d]", i)
	var t typeYAML
	if err := item.Decode(&t); err != nil {
		p.problems = append(p.problems, config.ProblemsOf(err, f.File, path)...)
		return Service{}, false
	}
	switch p.serviceType(item, path, t.Type) {
	case typeHTTP:
		return p.parseHTTP(f, path, item, channels)
	case typeProcess:
		return p.parseProcess(f, path, item, channels)
	case typeSystemd:
		return p.parseUnit(f, path, item, channels)
	}
	return Service{}, false
}

// serviceType returns the type of the service in item: its type key, or
// the type its identifying keys infer (D-058: url for http, command or
// script for process, unit for systemd). It reports a type that is
// unknown, ambiguous or missing, and then returns "".
func (p *parsed) serviceType(item config.Section, path, typ string) string {
	var inferred []string
	if item.Has("url") {
		inferred = append(inferred, typeHTTP)
	}
	if item.Has("command") || item.Has("script") {
		inferred = append(inferred, typeProcess)
	}
	if item.Has("unit") {
		inferred = append(inferred, typeSystemd)
	}
	switch typ {
	case typeHTTP, typeProcess, typeSystemd:
		return typ // the strict decode of the type reports foreign keys
	case "":
		switch len(inferred) {
		case 1:
			return inferred[0]
		case 0:
			p.addf(item, item.Line(), path, "a service needs url (an http service), command or script (a process service) or unit (a systemd service)")
		default:
			p.addf(item, item.Line(), path, "ambiguous service: url belongs to http services, command and script to process services, unit to systemd services")
		}
	default:
		p.addf(item, item.KeyLine("type"), path+".type", "unknown service type (valid: http, process, systemd)")
	}
	return ""
}

// serviceName returns the name of a service or job if valid, reporting it
// otherwise.
func (p *parsed) serviceName(item config.Section, path, name string) string {
	if !api.ValidName(name) {
		p.addf(item, lineOr(item, "name"), path+".name", "the name must be lower-case letters, digits, '.', '_' or '-' (at most 63, D-011)")
		return ""
	}
	return name
}

// route checks the notifications of a service or job, whose events are
// types.
func (p *parsed) route(f, item config.Section, path string, r config.Route, channels []string, types []model.EventType) {
	for _, problem := range r.Problems(f.File, path+".notifications", channels, types) {
		if line := item.KeyLine("notifications"); line > 0 {
			problem.Message = fmt.Sprintf("line %d: %s", line, problem.Message)
		}
		p.problems = append(p.problems, problem)
	}
}

// parseHTTP decodes and checks an http service.
func (p *parsed) parseHTTP(f config.Section, path string, item config.Section, channels []string) (Service, bool) {
	before := len(p.problems)
	var y httpServiceYAML
	bad := p.decode(item, &y, path)
	name := p.serviceName(item, path, y.Name)
	if item.Has("recovery") {
		p.addf(item, item.KeyLine("recovery"), path+".recovery", "recovery is not supported on http services: there is nothing to restart (D-012)")
	}
	if item.Has("limits") {
		p.addf(item, item.KeyLine("limits"), path+".limits", "limits are for process services: an http service runs nothing (D-090)")
	}
	u, why := checkURL(y.URL)
	if why != "" {
		p.addf(item, lineOr(item, "url"), path+".url", "%s", why)
	}
	svc := Service{Name: y.Name, Type: typeHTTP, URL: u,
		CheckInterval: p.interval(item, bad, path, y.CheckInterval, p.settings.CheckInterval),
		Timeout:       p.timeout(item, bad, path, y.Timeout, p.settings.Timeout),
		Policy:        p.policy(item, bad, path, y.FailurePolicy, p.settings.Policy),
		Route:         y.Notifications,
		HTTP:          p.http(item, bad, path, y.HTTP, u),
		Disabled:      y.Enabled != nil && !*y.Enabled}
	p.route(f, item, path, y.Notifications, channels, eventTypes)
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
var eventTypes = []model.EventType{EventServiceFailed, EventServiceRecovered, EventServiceRestarted, EventServiceRecoveryExhausted}

// parseJobs decodes the jobs of f. seen holds the names of the services
// and jobs met so far: they share one name space (ADR-0015).
func (p *parsed) parseJobs(f config.Section, seen map[string]place, channels []string) {
	items, err := f.Items("jobs")
	if err != nil {
		p.problems = append(p.problems, config.ProblemsOf(err, f.File, "jobs")...)
	}
	for i, item := range items {
		path := fmt.Sprintf("jobs[%d]", i)
		j, ok := p.parseJob(f, path, item, channels)
		if j.Name != "" {
			if first, dup := seen[j.Name]; dup {
				p.addf(item, item.KeyLine("name"), path+".name",
					"this name is already used by the service or job in %s at line %d", first.file, first.line)
				continue
			}
			seen[j.Name] = place{f.File, item.KeyLine("name")}
		}
		if !ok {
			continue
		}
		if len(p.services)+len(p.jobs) == maxServices {
			p.addf(item, item.Line(), "jobs", "more than %d services and jobs", maxServices)
			continue
		}
		p.jobs = append(p.jobs, j)
		p.jobWhere = append(p.jobWhere, place{f.File, item.Line()})
	}
}
