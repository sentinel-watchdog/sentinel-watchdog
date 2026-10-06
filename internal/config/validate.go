package config

import (
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	// Embed the IANA time zone database: minimal systems (Alpine,
	// containers) often lack /usr/share/zoneinfo.
	_ "time/tzdata"

	"github.com/sentinel-watchdog/sentinel/internal/scheduler/cronexpr"
	"github.com/sentinel-watchdog/sentinel/pkg/model"
)

var (
	nameRe        = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
	serviceRe     = regexp.MustCompile(`^[A-Za-z0-9:_.\\@-]+\.service$`)
	accountRe     = regexp.MustCompile(`^([a-z_][a-z0-9_.-]{0,31}\$?|[0-9]{1,10})$`)
	headerNameRe  = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")
	logLevels     = []string{"debug", "info", "warn", "error"}
	logFormats    = []LogFormat{LogFormatAuto, LogFormatText, LogFormatJSON, LogFormatJournal}
	httpMethods   = []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"}
	stopSignals   = []string{"SIGTERM", "SIGINT", "SIGQUIT", "SIGHUP", "SIGUSR1", "SIGUSR2"}
	limitActions  = []LimitAction{LimitLog, LimitNotify, LimitRestart, LimitStop, LimitKill}
	backoffs      = []Backoff{BackoffFixed, BackoffExponential}
	outputTypes   = []OutputType{OutputLog, OutputFile, OutputDiscard}
	concurrencies = []ConcurrencyPolicy{ConcurrencyForbid, ConcurrencyAllow, ConcurrencyReplace}
	missedRuns    = []MissedRunPolicy{MissedSkip, MissedRunOnce}
)

// Bounds for numeric settings.
const (
	minInterval    = Duration(time.Second)
	maxInterval    = Duration(24 * time.Hour)
	maxHTTPTimeout = Duration(5 * time.Minute)
	maxUnixPath    = 107 // sun_path is 108 bytes including the NUL
)

// validator collects errors and warnings.
type validator struct {
	errs, warns []Problem
}

func (v *validator) errorf(path, format string, args ...any) {
	v.errs = append(v.errs, Problem{Path: path, Message: fmt.Sprintf(format, args...)})
}

func (v *validator) warnf(path, format string, args ...any) {
	v.warns = append(v.warns, Problem{Path: path, Message: fmt.Sprintf(format, args...)})
}

func validate(c *Config) (warnings, errs []Problem) {
	v := &validator{}
	v.settings(c)
	for i := range c.Notifications {
		v.channel(&c.Notifications[i], i)
	}
	for i := range c.Monitors {
		v.monitor(c, &c.Monitors[i], i)
	}
	return v.warns, v.errs
}

func (v *validator) settings(c *Config) {
	s := &c.Settings
	v.absPath("settings.state_file", s.StateFile, true)
	if v.absPath("settings.socket", s.Socket, true) && len(s.Socket) > maxUnixPath {
		v.errorf("settings.socket", "path is longer than %d bytes (Unix socket limit)", maxUnixPath)
	}
	if s.SocketMode.Perm()&0o002 != 0 {
		v.errorf("settings.socket_mode", "%s is world-writable; any local user could control sentineld", s.SocketMode)
	}
	if s.SocketMode.Perm()&0o600 != 0o600 {
		v.errorf("settings.socket_mode", "%s must grant read and write to the owner", s.SocketMode)
	}
	if s.SocketGroup != "" && !accountRe.MatchString(s.SocketGroup) {
		v.errorf("settings.socket_group", "invalid group name %q", s.SocketGroup)
	}
	oneOf(v, "settings.log_level", s.LogLevel, logLevels)
	oneOf(v, "settings.log_format", s.LogFormat, logFormats)
	v.timezone("settings.timezone", s.Timezone)
	v.durationRange("settings.default_check_interval", s.DefaultCheckInterval, minInterval, maxInterval)
	v.durationRange("settings.default_command_timeout", s.DefaultCommandTimeout, minInterval, Duration(7*24*time.Hour))
	v.durationRange("settings.shutdown_timeout", s.ShutdownTimeout, minInterval, Duration(10*time.Minute))
	v.intRange("settings.history_limit", s.HistoryLimit, 1, 10_000)
	v.intRange("settings.event_limit", s.EventLimit, 1, 100_000)
	v.channelRefs(c, "settings.daemon_notifications", s.DaemonNotifications)
}

func (v *validator) channel(n *NotificationChannel, idx int) {
	p := itemPath("notifications", n.Name, idx)
	if !nameRe.MatchString(n.Name) {
		v.errorf(p+".name", "%q must match %s", n.Name, nameRe)
	}
	switch n.Type {
	case NotificationWebhook:
	case NotificationSlack, NotificationTeams:
		v.errorf(p+".type", "%q is planned but not implemented in this release; use type \"webhook\"", n.Type)
		return
	case "":
		v.errorf(p+".type", "is required (webhook)")
		return
	default:
		v.errorf(p+".type", "unknown notification type %q (supported: webhook)", n.Type)
		return
	}
	if v.httpURL(p+".url", n.URL) && strings.HasPrefix(n.URL, "http://") {
		v.warnf(p+".url", "plain HTTP sends notification content unencrypted")
	}
	oneOf(v, p+".method", n.Method, []string{"POST", "PUT"})
	v.durationRange(p+".timeout", n.Timeout, Duration(100*time.Millisecond), maxHTTPTimeout)
	v.headers(p+".headers", n.Headers)
	v.intRange(p+".retry.attempts", n.Retry.Attempts, 1, 10)
	v.retryDelays(p+".retry", n.Retry.Delay, n.Retry.MaxDelay, n.Retry.Backoff)
	v.statusCodes(p+".success_status_codes", n.SuccessStatusCodes)
	v.tls(p+".tls", n.TLS)
}

func (v *validator) monitor(c *Config, m *Monitor, idx int) {
	p := itemPath("monitors", m.Name, idx)
	if !nameRe.MatchString(m.Name) {
		v.errorf(p+".name", "%q must match %s", m.Name, nameRe)
	}
	v.notificationRefs(c, p+".notifications", m)

	switch {
	case m.Systemd != nil:
		v.systemd(p, m.Systemd)
	case m.Process != nil:
		v.process(p, m.Process, len(m.Notifications.Channels) > 0)
	case m.HTTP != nil:
		v.http(p, m.HTTP)
	case m.Cron != nil:
		v.cron(p, m.Cron)
	default:
		v.errorf(p, "monitor has no type-specific configuration")
	}
}

func (v *validator) notificationRefs(c *Config, p string, m *Monitor) {
	refs := m.Notifications
	if len(refs.Events) > 0 && len(refs.Channels) == 0 {
		v.errorf(p+".channels", "events are set but no channel is listed")
	}
	v.channelRefs(c, p+".channels", refs.Channels)
	scope := model.ScopeForMonitorType(m.Type)
	for _, ev := range refs.Events {
		got, ok := ev.Scope()
		switch {
		case !ok:
			v.errorf(p+".events", "unknown event type %q (valid: %s)", ev, joinEvents(model.EventTypesForScope(scope)))
		case got != scope:
			v.errorf(p+".events", "event %q does not apply to %s monitors (valid: %s)", ev, m.Type, joinEvents(model.EventTypesForScope(scope)))
		}
	}
}

func (v *validator) channelRefs(c *Config, p string, names []string) {
	seen := map[string]bool{}
	for _, name := range names {
		if seen[name] {
			v.errorf(p, "channel %q listed twice", name)
			continue
		}
		seen[name] = true
		ch, ok := c.Channel(name)
		if !ok {
			v.errorf(p, "unknown notification channel %q", name)
			continue
		}
		if !ch.IsEnabled() {
			v.warnf(p, "channel %q is disabled; no notifications will be sent through it", name)
		}
	}
}

func (v *validator) systemd(p string, s *SystemdSpec) {
	switch {
	case s.Service == "":
		v.errorf(p+".service", "is required (for example \"nginx.service\")")
	case !serviceRe.MatchString(s.Service) || strings.HasPrefix(s.Service, "-") || len(s.Service) > 255:
		v.errorf(p+".service", "%q is not a valid systemd service unit name (must end in .service)", s.Service)
	}
	v.durationRange(p+".check_interval", s.CheckInterval, minInterval, maxInterval)
	v.durationRange(p+".command_timeout", s.CommandTimeout, minInterval, maxHTTPTimeout)
	if s.JournalLines != nil {
		v.intRange(p+".journal_lines", *s.JournalLines, 0, 1000)
	}
	v.failurePolicy(p+".failure_policy", s.FailurePolicy)
	v.recovery(p+".recovery", s.Recovery)
}

func (v *validator) process(p string, s *ProcessSpec, hasChannels bool) {
	v.exec(p, &s.ExecSpec)
	h := s.Health
	v.durationRange(p+".health.startup_grace_period", h.StartupGracePeriod, 0, Duration(time.Hour))
	v.durationRange(p+".health.check_interval", h.CheckInterval, minInterval, maxInterval)
	oneOf(v, p+".health.stop_signal", h.StopSignal, stopSignals)
	v.durationRange(p+".health.stop_timeout", h.StopTimeout, minInterval, Duration(time.Hour))
	if l := s.Limits; l != nil {
		lp := p + ".limits"
		if l.MaxCPUPercent == 0 && l.MaxMemoryBytes == 0 {
			v.errorf(lp, "set max_cpu_percent and/or max_memory_bytes, or remove the limits block")
		}
		if l.MaxCPUPercent < 0 || l.MaxCPUPercent > 100*1024 {
			v.errorf(lp+".max_cpu_percent", "%g is out of range (0-%d, 100 = one CPU)", l.MaxCPUPercent, 100*1024)
		}
		v.durationRange(lp+".sustained_for", l.SustainedFor, 0, Duration(24*time.Hour))
		oneOf(v, lp+".action", l.Action, limitActions)
		if l.Action == LimitNotify && !hasChannels {
			v.warnf(lp+".action", "action is notify but the monitor has no notification channels")
		}
	}
	v.recovery(p+".recovery", s.Recovery)
}

func (v *validator) http(p string, s *HTTPSpec) {
	v.httpURL(p+".url", s.URL)
	oneOf(v, p+".method", s.Method, httpMethods)
	v.headers(p+".headers", s.Headers)
	v.durationRange(p+".timeout", s.Timeout, Duration(100*time.Millisecond), maxHTTPTimeout)
	v.durationRange(p+".check_interval", s.CheckInterval, minInterval, maxInterval)
	if s.Timeout >= s.CheckInterval {
		v.warnf(p+".timeout", "timeout (%s) is not shorter than check_interval (%s)", s.Timeout, s.CheckInterval)
	}
	if s.FollowRedirects {
		v.intRange(p+".max_redirects", s.MaxRedirects, 1, 50)
	}
	v.tls(p+".tls", s.TLS)
	v.statusCodes(p+".expect.status_codes", s.Expect.StatusCodes)
	if s.Expect.BodyMaxBytes > 64<<20 {
		v.errorf(p+".expect.body_max_bytes", "must not exceed 64MiB")
	}
	if uint64(len(s.Expect.BodyContains)) > uint64(s.Expect.BodyMaxBytes) {
		v.errorf(p+".expect.body_contains", "is longer than body_max_bytes")
	}
	v.failurePolicy(p+".failure_policy", s.FailurePolicy)
}

func (v *validator) cron(p string, s *CronSpec) {
	v.exec(p, &s.ExecSpec)
	if s.Schedule == "" {
		v.errorf(p+".schedule", "is required (5-field cron expression, for example \"0 2 * * *\")")
	} else if _, err := cronexpr.Parse(s.Schedule); err != nil {
		v.errorf(p+".schedule", "%v", err)
	}
	v.timezone(p+".timezone", s.Timezone)
	v.durationRange(p+".timeout", s.Timeout, minInterval, Duration(7*24*time.Hour))
	oneOf(v, p+".concurrency_policy", s.ConcurrencyPolicy, concurrencies)
	oneOf(v, p+".missed_runs", s.MissedRuns, missedRuns)
	v.durationRange(p+".stop_timeout", s.StopTimeout, minInterval, Duration(time.Hour))
}

func (v *validator) exec(p string, e *ExecSpec) {
	switch {
	case e.Command != "" && e.Script != "":
		v.errorf(p, "command and script are mutually exclusive")
	case e.Command == "" && e.Script == "":
		v.errorf(p, "one of command (executable path) or script (shell text) is required")
	case e.Command != "":
		v.absPath(p+".command", e.Command, true)
		if e.Shell != "" {
			v.errorf(p+".shell", "is only valid with script; command is executed directly without a shell")
		}
	case e.Script != "":
		if len(e.Args) > 0 {
			v.errorf(p+".args", "is not allowed with script; pass values inside the script text")
		}
		v.absPath(p+".shell", e.Shell, true)
	}
	if strings.ContainsRune(e.Script, 0) {
		v.errorf(p+".script", "contains a NUL byte")
	}
	for i, a := range e.Args {
		if strings.ContainsRune(a, 0) {
			v.errorf(fmt.Sprintf("%s.args[%d]", p, i), "contains a NUL byte")
		}
	}
	if e.User != "" && !accountRe.MatchString(e.User) {
		v.errorf(p+".user", "invalid user %q (name or numeric uid)", e.User)
	}
	if e.Group != "" && !accountRe.MatchString(e.Group) {
		v.errorf(p+".group", "invalid group %q (name or numeric gid)", e.Group)
	}
	if e.WorkingDirectory != "" {
		v.absPath(p+".working_directory", e.WorkingDirectory, false)
	}
	for _, k := range sortedKeys(e.Environment) {
		if !isEnvName(k) {
			v.errorf(p+".environment", "invalid variable name %q", k)
		}
		if strings.ContainsRune(e.Environment[k], 0) {
			v.errorf(p+".environment."+k, "contains a NUL byte")
		}
	}
	v.output(p+".stdout", e.Stdout)
	v.output(p+".stderr", e.Stderr)
	if e.Stdout.Type == OutputFile && e.Stdout.Path == e.Stderr.Path && e.Stderr.Type == OutputFile {
		v.errorf(p+".stderr.path", "stdout and stderr must use different files")
	}
}

func (v *validator) output(p string, o Output) {
	if !oneOf(v, p+".type", o.Type, outputTypes) {
		return
	}
	if o.Type != OutputFile {
		if o.Path != "" {
			v.errorf(p+".path", "is only valid with type file")
		}
		return
	}
	v.absPath(p+".path", o.Path, true)
	if o.Mode.Perm()&0o002 != 0 {
		v.errorf(p+".mode", "%s is world-writable", o.Mode)
	}
}

func (v *validator) recovery(p string, r *RecoveryPolicy) {
	if r == nil {
		return
	}
	switch r.Action {
	case RecoveryNone, RecoveryRestart:
	case RecoveryExecute:
		v.errorf(p+".action", "\"execute\" is planned but not implemented in this release (supported: none, restart)")
	default:
		v.errorf(p+".action", "unknown recovery action %q (supported: none, restart)", r.Action)
	}
	if !r.IsEnabled() {
		return
	}
	v.intRange(p+".max_attempts", r.MaxAttempts, 1, 1000)
	v.durationRange(p+".window", r.Window, minInterval, Duration(30*24*time.Hour))
	v.retryDelays(p, r.Delay, r.MaxDelay, r.Backoff)
	v.durationRange(p+".stable_after", r.StableAfter, 0, Duration(30*24*time.Hour))
	v.durationRange(p+".cooldown", r.Cooldown, 0, Duration(30*24*time.Hour))
}

func (v *validator) failurePolicy(p string, f FailurePolicy) {
	v.intRange(p+".consecutive_failures", f.ConsecutiveFailures, 1, 1000)
	v.intRange(p+".recovery_after_successes", f.RecoveryAfterSuccesses, 1, 1000)
}

func (v *validator) retryDelays(p string, delay, maxDelay Duration, backoff Backoff) {
	v.durationRange(p+".delay", delay, 0, Duration(24*time.Hour))
	v.durationRange(p+".max_delay", maxDelay, 0, Duration(24*time.Hour))
	if maxDelay < delay {
		v.errorf(p+".max_delay", "(%s) must be greater than or equal to delay (%s)", maxDelay, delay)
	}
	oneOf(v, p+".backoff", backoff, backoffs)
}

func (v *validator) httpURL(p, raw string) bool {
	if raw == "" {
		v.errorf(p, "is required")
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		// url errors echo the input, which may contain secrets.
		v.errorf(p, "is not a valid URL")
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		v.errorf(p, "scheme must be http or https")
		return false
	}
	if u.Host == "" {
		v.errorf(p, "host is missing")
		return false
	}
	return true
}

func (v *validator) headers(p string, h map[string]string) {
	for _, k := range sortedKeys(h) {
		if !headerNameRe.MatchString(k) {
			v.errorf(p, "invalid header name %q", k)
		}
		if strings.ContainsAny(h[k], "\r\n\x00") {
			v.errorf(p+"."+k, "value contains CR, LF or NUL")
		}
	}
}

func (v *validator) tls(p string, t TLSConfig) {
	if t.CAFile != "" {
		v.absPath(p+".ca_file", t.CAFile, true)
	}
	if t.InsecureSkipVerify {
		v.warnf(p+".insecure_skip_verify", "TLS certificate verification is disabled")
	}
}

func (v *validator) statusCodes(p string, codes []int) {
	for _, c := range codes {
		if c < 100 || c > 599 {
			v.errorf(p, "%d is not a valid HTTP status code", c)
		}
	}
}

func (v *validator) timezone(p, tz string) {
	if _, err := time.LoadLocation(tz); err != nil {
		v.errorf(p, "unknown time zone %q", tz)
	}
}

// absPath requires a clean absolute path; it reports whether p is valid.
func (v *validator) absPath(p, path string, required bool) bool {
	switch {
	case path == "":
		if required {
			v.errorf(p, "is required")
		}
		return false
	case strings.ContainsRune(path, 0):
		v.errorf(p, "contains a NUL byte")
	case !filepath.IsAbs(path):
		v.errorf(p, "%q must be an absolute path", path)
	case filepath.Clean(path) != path:
		v.errorf(p, "%q must be a clean path (no '..', '.', '//' or trailing '/'); use %q", path, filepath.Clean(path))
	default:
		return true
	}
	return false
}

func (v *validator) durationRange(p string, d, minD, maxD Duration) {
	if d < minD || d > maxD {
		v.errorf(p, "%s is out of range (%s - %s)", d, minD, maxD)
	}
}

func (v *validator) intRange(p string, n, minN, maxN int) {
	if n < minN || n > maxN {
		v.errorf(p, "%d is out of range (%d - %d)", n, minN, maxN)
	}
}

// oneOf reports whether got is one of allowed, recording an error if not.
func oneOf[T ~string](v *validator, p string, got T, allowed []T) bool {
	if slices.Contains(allowed, got) {
		return true
	}
	v.errorf(p, "%q is not valid (allowed: %s)", string(got), strings.Join(stringsOf(allowed), ", "))
	return false
}

func stringsOf[T ~string](in []T) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = string(s)
	}
	return out
}

func joinEvents(evs []model.EventType) string { return strings.Join(stringsOf(evs), ", ") }

// itemPath identifies a list element by name when it has a valid one.
func itemPath(list, name string, idx int) string {
	if nameRe.MatchString(name) {
		return fmt.Sprintf("%s[%s]", list, name)
	}
	return fmt.Sprintf("%s[%d]", list, idx)
}
