package config

import (
	"strings"
	"time"

	"github.com/sentinel-watchdog/sentinel/pkg/model"
)

// Default values. A zero or omitted field takes the default; validation
// runs afterwards, so explicit invalid values (negative durations, unknown
// enums) are still rejected.
const (
	DefaultStateFile             = "/var/lib/sentinel/state.json"
	DefaultSocket                = "/run/sentinel/sentinel.sock"
	DefaultSocketMode   FileMode = 0o660
	DefaultLogLevel              = "info"
	DefaultTimezone              = "Local"
	DefaultHistoryLimit          = 50
	DefaultEventLimit            = 500

	DefaultCheckInterval   = Duration(15 * time.Second)
	DefaultCommandTimeout  = Duration(5 * time.Minute)
	DefaultShutdownTimeout = Duration(30 * time.Second)

	DefaultOutputFileMode FileMode = 0o640
)

func applyDefaults(c *Config) {
	c.Version = SchemaVersion
	s := &c.Settings
	setStr(&s.StateFile, DefaultStateFile)
	setStr(&s.Socket, DefaultSocket)
	if s.SocketMode == 0 {
		s.SocketMode = DefaultSocketMode
	}
	setStr(&s.LogLevel, DefaultLogLevel)
	if s.LogFormat == "" {
		s.LogFormat = LogFormatAuto
	}
	setStr(&s.Timezone, DefaultTimezone)
	setDur(&s.DefaultCheckInterval, DefaultCheckInterval)
	setDur(&s.DefaultCommandTimeout, DefaultCommandTimeout)
	setDur(&s.ShutdownTimeout, DefaultShutdownTimeout)
	setInt(&s.HistoryLimit, DefaultHistoryLimit)
	setInt(&s.EventLimit, DefaultEventLimit)

	for i := range c.Notifications {
		defaultChannel(&c.Notifications[i])
	}
	for i := range c.Monitors {
		defaultMonitor(&c.Monitors[i], s)
	}
}

func defaultChannel(n *NotificationChannel) {
	setStr(&n.Method, "POST")
	n.Method = strings.ToUpper(n.Method)
	setDur(&n.Timeout, Duration(10*time.Second))
	setInt(&n.Retry.Attempts, 3)
	setDur(&n.Retry.Delay, Duration(5*time.Second))
	if n.Retry.Backoff == "" {
		n.Retry.Backoff = BackoffExponential
	}
	setDur(&n.Retry.MaxDelay, Duration(time.Minute))
	if !hasHeader(n.Headers, "Content-Type") {
		h := make(map[string]string, len(n.Headers)+1)
		for k, v := range n.Headers {
			h[k] = v
		}
		h["Content-Type"] = "application/json"
		n.Headers = h
	}
}

// DefaultEvents returns the events notified when a monitor lists channels
// without an explicit event filter.
func DefaultEvents(t model.MonitorType) []model.EventType {
	if model.ScopeForMonitorType(t) == model.ScopeJob {
		return []model.EventType{model.EventJobFailed, model.EventJobTimeout}
	}
	return []model.EventType{model.EventMonitorFailed, model.EventRecoveryExhausted, model.EventMonitorRecovered}
}

func defaultMonitor(m *Monitor, s *Settings) {
	if len(m.Notifications.Channels) > 0 && len(m.Notifications.Events) == 0 {
		m.Notifications.Events = DefaultEvents(m.Type)
	}
	switch {
	case m.Systemd != nil:
		sp := m.Systemd
		setDur(&sp.CheckInterval, s.DefaultCheckInterval)
		setDur(&sp.CommandTimeout, Duration(30*time.Second))
		if sp.JournalLines == nil {
			n := 20
			sp.JournalLines = &n
		}
		defaultFailurePolicy(&sp.FailurePolicy, 1, 1)
		defaultRecovery(sp.Recovery)
	case m.Process != nil:
		sp := m.Process
		defaultExec(&sp.ExecSpec)
		setDur(&sp.Health.StartupGracePeriod, Duration(10*time.Second))
		setDur(&sp.Health.CheckInterval, s.DefaultCheckInterval)
		setStr(&sp.Health.StopSignal, "SIGTERM")
		sp.Health.StopSignal = normalizeSignal(sp.Health.StopSignal)
		setDur(&sp.Health.StopTimeout, Duration(30*time.Second))
		if sp.Limits != nil {
			setDur(&sp.Limits.SustainedFor, Duration(30*time.Second))
			if sp.Limits.Action == "" {
				sp.Limits.Action = LimitLog
			}
		}
		defaultRecovery(sp.Recovery)
	case m.HTTP != nil:
		sp := m.HTTP
		setStr(&sp.Method, "GET")
		sp.Method = strings.ToUpper(sp.Method)
		setDur(&sp.Timeout, Duration(10*time.Second))
		setDur(&sp.CheckInterval, s.DefaultCheckInterval)
		setInt(&sp.MaxRedirects, 10)
		if sp.Expect.BodyMaxBytes == 0 {
			sp.Expect.BodyMaxBytes = 1 << 20
		}
		defaultFailurePolicy(&sp.FailurePolicy, 3, 1)
	case m.Cron != nil:
		sp := m.Cron
		defaultExec(&sp.ExecSpec)
		setStr(&sp.Timezone, s.Timezone)
		setDur(&sp.Timeout, s.DefaultCommandTimeout)
		if sp.ConcurrencyPolicy == "" {
			sp.ConcurrencyPolicy = ConcurrencyForbid
		}
		if sp.MissedRuns == "" {
			sp.MissedRuns = MissedSkip
		}
		setDur(&sp.StopTimeout, Duration(30*time.Second))
	}
}

func defaultExec(e *ExecSpec) {
	if e.Script != "" {
		setStr(&e.Shell, "/bin/sh")
	}
	for _, o := range []*Output{&e.Stdout, &e.Stderr} {
		if o.Type == "" {
			o.Type = OutputLog
		}
		if o.Type == OutputFile && o.Mode == 0 {
			o.Mode = DefaultOutputFileMode
		}
	}
}

func defaultRecovery(r *RecoveryPolicy) {
	if r == nil {
		return
	}
	if r.Action == "" {
		r.Action = RecoveryRestart
	}
	setInt(&r.MaxAttempts, 5)
	setDur(&r.Window, Duration(10*time.Minute))
	setDur(&r.Delay, Duration(5*time.Second))
	if r.Backoff == "" {
		r.Backoff = BackoffExponential
	}
	setDur(&r.MaxDelay, Duration(5*time.Minute))
	setDur(&r.StableAfter, Duration(15*time.Minute))
}

func defaultFailurePolicy(f *FailurePolicy, failures, successes int) {
	setInt(&f.ConsecutiveFailures, failures)
	setInt(&f.RecoveryAfterSuccesses, successes)
}

// normalizeSignal accepts "TERM", "sigterm" or "SIGTERM".
func normalizeSignal(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	if !strings.HasPrefix(s, "SIG") {
		s = "SIG" + s
	}
	return s
}

func hasHeader(h map[string]string, name string) bool {
	for k := range h {
		if strings.EqualFold(k, name) {
			return true
		}
	}
	return false
}

func setStr(p *string, def string) {
	if *p == "" {
		*p = def
	}
}

func setInt(p *int, def int) {
	if *p == 0 {
		*p = def
	}
}

func setDur(p *Duration, def Duration) {
	if *p == 0 {
		*p = def
	}
}
