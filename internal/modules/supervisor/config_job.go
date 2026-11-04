package supervisor

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/config"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/cronexpr"
)

// Limits and defaults of a job (D-093 (4), brief 3c-4 Q11).
const (
	defaultJobTimeout = time.Hour
	minJobTimeout     = time.Second
	maxJobTimeout     = 24 * time.Hour

	typeCron = "cron" // a job's only type; also its events' source type

	concurrencyForbid  = "forbid"   // a slot while a run is active is skipped (D-018)
	concurrencyAllow   = "allow"    // runs overlap, up to settings.max_concurrent_runs
	concurrencyReplace = "replace"  // a slot stops the active runs, then launches
	missedSkip         = "skip"     // a missed slot is not run (D-018)
	missedRunOnce      = "run_once" // the latest missed slot runs once
)

// Job is a validated job definition, defaults applied: a program run on a
// cron schedule in a time zone (D-018, D-096).
type Job struct {
	Name     string
	Schedule *cronexpr.Schedule
	Expr     string // the schedule as configured, for status
	// TZ is the job's own time zone, "" when it inherits daemon.timezone;
	// Location is the zone it schedules in, resolved at Configure.
	TZ       string
	Location *time.Location
	// Program is how each run starts; its StartupGrace and Limits are zero.
	Program     Process
	Timeout     time.Duration
	Concurrency string // concurrencyForbid, concurrencyAllow or concurrencyReplace
	Missed      string // missedSkip or missedRunOnce
	Route       config.Route
	// Disabled is `enabled: false`: the job is validated, never scheduled.
	Disabled bool
}

// zoneName is the name of the zone j schedules in.
func (j Job) zoneName() string { return j.Location.String() }

type jobYAML struct {
	Name              string           `yaml:"name"`
	Type              string           `yaml:"type"`
	Schedule          string           `yaml:"schedule"`
	Timezone          *string          `yaml:"timezone"`
	Timeout           *config.Duration `yaml:"timeout"`
	ConcurrencyPolicy *string          `yaml:"concurrency_policy"`
	MissedRuns        *string          `yaml:"missed_runs"`
	Notifications     config.Route     `yaml:"notifications"`
	Enabled           *bool            `yaml:"enabled"`
	Program           programYAML      `yaml:",inline"`
	// Decoded only to be refused with a reason.
	Recovery      any `yaml:"recovery"`
	FailurePolicy any `yaml:"failure_policy"`
	StartupGrace  any `yaml:"startup_grace"`
	Limits        any `yaml:"limits"`
	CheckInterval any `yaml:"check_interval"`
	Restart       any `yaml:"restart"`
	URL           any `yaml:"url"`
	Unit          any `yaml:"unit"`
}

// refusedJobKeys are keys a job does not take, and why.
var refusedJobKeys = []struct{ key, why string }{
	{"recovery", "jobs are not restarted: a failed run is reported and the next slot runs (D-012)"},
	{"failure_policy", "failure_policy is for checked services: every run of a job has its own result"},
	{"startup_grace", "startup_grace is for process services: a job runs to its end"},
	{"limits", "limits are for process services (D-090)"},
	{"check_interval", "check_interval is for checked services: a job runs on its schedule"},
	{"restart", "a job is not restarted: its schedule starts each run"},
	{"url", "url belongs to http services, not to jobs"},
	{"unit", "unit belongs to systemd services, not to jobs"},
}

// parseJob decodes and checks the job in item at path; ok is false when
// it has problems. Problems name keys and lines, never values: the
// schedule, arguments and environment may hold expanded secrets.
func (p *parsed) parseJob(f config.Section, path string, item config.Section, channels []string) (Job, bool) {
	before := len(p.problems)
	var y jobYAML
	bad := p.decode(item, &y, path)
	name := p.serviceName(item, path, y.Name)
	if item.Has("type") && !bad[item.KeyLine("type")] && y.Type != typeCron {
		p.addf(item, item.KeyLine("type"), path+".type", "a job's type is cron")
	}
	for _, r := range refusedJobKeys {
		if item.Has(r.key) {
			p.addf(item, item.KeyLine(r.key), path+"."+r.key, "%s", r.why)
		}
	}
	j := Job{Name: name, Timeout: defaultJobTimeout, Concurrency: concurrencyForbid, Missed: missedSkip,
		Route: y.Notifications, Disabled: y.Enabled != nil && !*y.Enabled}
	j.Schedule, j.Expr = p.schedule(item, bad, path, y.Schedule)
	if item.Has("timezone") && !bad[item.KeyLine("timezone")] {
		j.TZ = p.zone(item, path, y.Timezone)
	}
	j.Timeout = p.duration(item, bad, path, "timeout", y.Timeout, defaultJobTimeout, minJobTimeout, maxJobTimeout)
	j.Concurrency = p.policyName(item, bad, path, "concurrency_policy", y.ConcurrencyPolicy, concurrencyForbid,
		concurrencyForbid, concurrencyAllow, concurrencyReplace)
	j.Missed = p.policyName(item, bad, path, "missed_runs", y.MissedRuns, missedSkip,
		missedSkip, missedRunOnce)
	j.Program = p.programSpec(item, bad, path, y.Program, "a job")
	p.route(f, item, path, y.Notifications, channels, jobEventTypes)
	return j, len(p.problems) == before
}

// schedule parses a job's schedule. Its problems are fixed texts: the
// parser's own messages quote what they reject (review 3c-4a C9).
func (p *parsed) schedule(item config.Section, bad map[int]bool, path, expr string) (*cronexpr.Schedule, string) {
	line := item.KeyLine("schedule")
	switch {
	case bad[line]:
		return nil, ""
	case !item.Has("schedule"):
		p.addf(item, item.Line(), path+".schedule", "a job needs a schedule (5 fields, or a macro such as @daily)")
		return nil, ""
	}
	s, err := cronexpr.Parse(expr)
	switch {
	case errors.Is(err, cronexpr.ErrImpossible):
		p.addf(item, line, path+".schedule", "schedule can never run: its day-of-month does not occur in its months")
	case err != nil:
		p.addf(item, line, path+".schedule", "schedule is not a valid cron expression (5 fields: minute hour day-of-month month day-of-week, or a macro such as @daily)")
	default:
		return s, s.String()
	}
	return nil, ""
}

// zone checks a job's own time zone and returns its name.
func (p *parsed) zone(item config.Section, path string, v *string) string {
	if v == nil || *v == "" {
		p.addf(item, item.KeyLine("timezone"), path+".timezone", "unknown time zone (use an IANA name such as \"Europe/Rome\", or \"Local\"; remove the key to use daemon.timezone)")
		return ""
	}
	if _, err := time.LoadLocation(*v); err != nil {
		p.addf(item, item.KeyLine("timezone"), path+".timezone", "unknown time zone (use an IANA name such as \"Europe/Rome\", or \"Local\")")
		return ""
	}
	return *v
}

// policyName returns the value of a policy key, def when it is absent or
// wrong; a wrong one is a problem naming the values, never quoting it.
func (p *parsed) policyName(item config.Section, bad map[int]bool, path, key string, v *string, def string, values ...string) string {
	line := item.KeyLine(key)
	switch {
	case !item.Has(key) || bad[line]:
		return def
	case v != nil && slices.Contains(values, *v):
		return *v
	}
	p.addf(item, line, path+"."+key, "%s must be one of %s", key, strings.Join(values, ", "))
	return def
}

// resolveJob completes a job at Configure: its program as for a process
// service (identity, trust of the program and of the output directory,
// D-086), and the zone it schedules in, its own or tz (daemon.timezone).
func (m *Module) resolveJob(j *Job, tz string) string {
	name := j.TZ
	if name == "" {
		name = tz
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return fmt.Sprintf("time zone %q cannot be loaded", name) // a validated zone name: never a secret
	}
	j.Location = loc
	return m.resolveProcess(&j.Program)
}
