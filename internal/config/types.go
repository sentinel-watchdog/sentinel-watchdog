// Package config defines Sentinel's YAML configuration model and loads,
// expands, defaults and validates it.
//
// Loading is a pipeline: read files -> parse YAML nodes -> expand
// ${VARIABLE} references in scalar values -> reject unknown keys -> decode
// -> merge fragments -> apply defaults -> validate. See docs/configuration.md.
package config

import (
	"github.com/sentinel-watchdog/sentinel/pkg/model"
)

// SchemaVersion is the only configuration schema version understood by this
// release.
const SchemaVersion = 1

// Config is the fully loaded, merged and validated configuration.
type Config struct {
	Version       int                   `yaml:"version" json:"version"`
	Settings      Settings              `yaml:"settings" json:"settings"`
	Notifications []NotificationChannel `yaml:"notifications,omitempty" json:"notifications,omitempty"`
	Monitors      []Monitor             `yaml:"monitors,omitempty" json:"monitors,omitempty"`
}

// Monitor returns the monitor with the given name.
func (c *Config) Monitor(name string) (*Monitor, bool) {
	for i := range c.Monitors {
		if c.Monitors[i].Name == name {
			return &c.Monitors[i], true
		}
	}
	return nil, false
}

// Channel returns the notification channel with the given name.
func (c *Config) Channel(name string) (*NotificationChannel, bool) {
	for i := range c.Notifications {
		if c.Notifications[i].Name == name {
			return &c.Notifications[i], true
		}
	}
	return nil, false
}

// LogFormat selects the sentineld log encoding.
type LogFormat string

// Log formats.
const (
	LogFormatAuto    LogFormat = "auto"
	LogFormatText    LogFormat = "text"
	LogFormatJSON    LogFormat = "json"
	LogFormatJournal LogFormat = "journal"
)

// Settings are daemon-wide options. They may only appear in the main file.
type Settings struct {
	StateFile             string    `yaml:"state_file" json:"state_file"`
	Socket                string    `yaml:"socket" json:"socket"`
	SocketMode            FileMode  `yaml:"socket_mode" json:"socket_mode"`
	SocketGroup           string    `yaml:"socket_group,omitempty" json:"socket_group,omitempty"`
	LogLevel              string    `yaml:"log_level" json:"log_level"`
	LogFormat             LogFormat `yaml:"log_format" json:"log_format"`
	Timezone              string    `yaml:"timezone" json:"timezone"`
	DefaultCheckInterval  Duration  `yaml:"default_check_interval" json:"default_check_interval"`
	DefaultCommandTimeout Duration  `yaml:"default_command_timeout" json:"default_command_timeout"`
	ShutdownTimeout       Duration  `yaml:"shutdown_timeout" json:"shutdown_timeout"`
	// HistoryLimit bounds the per-monitor history kept in the state file.
	HistoryLimit int `yaml:"history_limit" json:"history_limit"`
	// EventLimit bounds the global event log kept in the state file.
	EventLimit int `yaml:"event_limit" json:"event_limit"`
	// DaemonNotifications lists channels receiving daemon-scope events
	// (configuration_error, daemon_error).
	DaemonNotifications []string `yaml:"daemon_notifications,omitempty" json:"daemon_notifications,omitempty"`
}

// NotificationType identifies a notification provider.
type NotificationType string

// Notification provider types.
const (
	NotificationWebhook NotificationType = "webhook"
	// Planned providers; rejected with a "not implemented" error.
	NotificationSlack NotificationType = "slack"
	NotificationTeams NotificationType = "teams"
)

// Backoff selects how retry delays grow.
type Backoff string

// Backoff strategies.
const (
	BackoffFixed       Backoff = "fixed"
	BackoffExponential Backoff = "exponential"
)

// NotificationChannel is a configured notification destination.
type NotificationChannel struct {
	Name    string            `yaml:"name" json:"name"`
	Type    NotificationType  `yaml:"type" json:"type"`
	Enabled *bool             `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	URL     string            `yaml:"url" json:"url"`
	Method  string            `yaml:"method,omitempty" json:"method,omitempty"`
	Timeout Duration          `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`
	Retry   RetryPolicy       `yaml:"retry,omitempty" json:"retry"`
	// SuccessStatusCodes lists HTTP status codes treated as delivered.
	// Empty means any 2xx.
	SuccessStatusCodes []int `yaml:"success_status_codes,omitempty" json:"success_status_codes,omitempty"`
	// TLS settings for HTTPS endpoints.
	TLS TLSConfig `yaml:"tls,omitempty" json:"tls"`
}

// IsEnabled reports whether the channel is enabled (default true).
func (n *NotificationChannel) IsEnabled() bool { return boolOr(n.Enabled, true) }

// RetryPolicy configures notification delivery retries.
type RetryPolicy struct {
	// Attempts is the total number of delivery attempts, including the first.
	Attempts int      `yaml:"attempts,omitempty" json:"attempts"`
	Delay    Duration `yaml:"delay,omitempty" json:"delay"`
	Backoff  Backoff  `yaml:"backoff,omitempty" json:"backoff"`
	MaxDelay Duration `yaml:"max_delay,omitempty" json:"max_delay"`
}

// TLSConfig configures outbound TLS for HTTP monitors and webhooks.
type TLSConfig struct {
	// CAFile is a PEM bundle used instead of the system roots.
	CAFile     string `yaml:"ca_file,omitempty" json:"ca_file,omitempty"`
	ServerName string `yaml:"server_name,omitempty" json:"server_name,omitempty"`
	// InsecureSkipVerify disables certificate verification. Never default.
	InsecureSkipVerify bool `yaml:"insecure_skip_verify,omitempty" json:"insecure_skip_verify,omitempty"`
}

// MonitorCommon holds the fields shared by every monitor type.
type MonitorCommon struct {
	Name          string            `yaml:"name" json:"name"`
	Type          model.MonitorType `yaml:"type" json:"type"`
	Enabled       *bool             `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	Description   string            `yaml:"description,omitempty" json:"description,omitempty"`
	Notifications NotificationRefs  `yaml:"notifications,omitempty" json:"notifications"`
}

// Monitor is one monitored target. Exactly one of the type-specific specs
// is non-nil, matching Type.
type Monitor struct {
	MonitorCommon

	Systemd *SystemdSpec `yaml:"-" json:"systemd,omitempty"`
	Process *ProcessSpec `yaml:"-" json:"process,omitempty"`
	HTTP    *HTTPSpec    `yaml:"-" json:"http,omitempty"`
	Cron    *CronSpec    `yaml:"-" json:"cron,omitempty"`
}

// IsEnabled reports whether the monitor is enabled in configuration
// (default true).
func (m *Monitor) IsEnabled() bool { return boolOr(m.Enabled, true) }

// NotificationRefs selects the channels and event types a monitor notifies.
//
// YAML accepts a short form (a list of channel names, default events) or a
// long form: {channels: [...], events: [...]}.
type NotificationRefs struct {
	Channels []string          `yaml:"channels,omitempty" json:"channels,omitempty"`
	Events   []model.EventType `yaml:"events,omitempty" json:"events,omitempty"`
}

// RecoveryAction is what Sentinel does when a monitor fails.
type RecoveryAction string

// Recovery actions.
const (
	RecoveryNone    RecoveryAction = "none"
	RecoveryRestart RecoveryAction = "restart"
	// RecoveryExecute is planned and rejected by validation for now.
	RecoveryExecute RecoveryAction = "execute"
)

// RecoveryPolicy bounds automatic recovery.
type RecoveryPolicy struct {
	Enabled *bool          `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	Action  RecoveryAction `yaml:"action,omitempty" json:"action"`
	// MaxAttempts is the number of recovery attempts allowed inside Window.
	MaxAttempts int      `yaml:"max_attempts,omitempty" json:"max_attempts"`
	Window      Duration `yaml:"window,omitempty" json:"window"`
	// Delay before the first attempt; grows according to Backoff.
	Delay    Duration `yaml:"delay,omitempty" json:"delay"`
	Backoff  Backoff  `yaml:"backoff,omitempty" json:"backoff"`
	MaxDelay Duration `yaml:"max_delay,omitempty" json:"max_delay"`
	// StableAfter resets attempt counters once the target stays healthy
	// for this long.
	StableAfter Duration `yaml:"stable_after,omitempty" json:"stable_after"`
	// Cooldown, when non-zero, leaves the exhausted state automatically
	// after this period and allows a new series of attempts. Zero keeps
	// the monitor exhausted until `sentinelctl reset`.
	Cooldown Duration `yaml:"cooldown,omitempty" json:"cooldown"`
}

// IsEnabled reports whether automatic recovery is active. A present
// recovery block defaults to enabled; a missing one is disabled.
func (r *RecoveryPolicy) IsEnabled() bool {
	return r != nil && boolOr(r.Enabled, true) && r.Action != RecoveryNone
}

// FailurePolicy debounces flapping checks.
type FailurePolicy struct {
	// ConsecutiveFailures failed checks are required before the monitor
	// enters the failed state.
	ConsecutiveFailures int `yaml:"consecutive_failures,omitempty" json:"consecutive_failures"`
	// RecoveryAfterSuccesses successful checks are required before a failed
	// monitor is considered recovered.
	RecoveryAfterSuccesses int `yaml:"recovery_after_successes,omitempty" json:"recovery_after_successes"`
}

// SystemdSpec watches an existing systemd service. Sentinel never creates,
// edits, enables or disables units.
type SystemdSpec struct {
	Service        string   `yaml:"service" json:"service"`
	CheckInterval  Duration `yaml:"check_interval,omitempty" json:"check_interval"`
	CommandTimeout Duration `yaml:"command_timeout,omitempty" json:"command_timeout"`
	// JournalLines is the number of journal lines attached to failure
	// events as diagnostics. 0 disables journal collection.
	JournalLines  *int            `yaml:"journal_lines,omitempty" json:"journal_lines,omitempty"`
	FailurePolicy FailurePolicy   `yaml:"failure_policy,omitempty" json:"failure_policy"`
	Recovery      *RecoveryPolicy `yaml:"recovery,omitempty" json:"recovery,omitempty"`
}

// OutputType selects where a supervised command's stdout/stderr go.
type OutputType string

// Output types.
const (
	// OutputLog forwards each line to the sentineld logger (journald when
	// running under systemd). Default.
	OutputLog OutputType = "log"
	// OutputFile appends to a file.
	OutputFile OutputType = "file"
	// OutputDiscard drops the stream; must be chosen explicitly.
	OutputDiscard OutputType = "discard"
)

// Output configures one output stream of a supervised command.
type Output struct {
	Type OutputType `yaml:"type,omitempty" json:"type"`
	Path string     `yaml:"path,omitempty" json:"path,omitempty"`
	// Mode applies when Sentinel creates the file (default 0640).
	Mode FileMode `yaml:"mode,omitempty" json:"mode,omitempty"`
}

// ExecSpec describes how to run a command. Exactly one of Command or Script
// is set: Command is executed directly (no shell, no $PATH lookup); Script
// is passed to Shell with "-c".
type ExecSpec struct {
	Command          string            `yaml:"command,omitempty" json:"command,omitempty"`
	Args             []string          `yaml:"args,omitempty" json:"args,omitempty"`
	Script           string            `yaml:"script,omitempty" json:"script,omitempty"`
	Shell            string            `yaml:"shell,omitempty" json:"shell,omitempty"`
	User             string            `yaml:"user,omitempty" json:"user,omitempty"`
	Group            string            `yaml:"group,omitempty" json:"group,omitempty"`
	WorkingDirectory string            `yaml:"working_directory,omitempty" json:"working_directory,omitempty"`
	Environment      map[string]string `yaml:"environment,omitempty" json:"environment,omitempty"`
	Stdout           Output            `yaml:"stdout,omitempty" json:"stdout"`
	Stderr           Output            `yaml:"stderr,omitempty" json:"stderr"`
}

// ProcessSpec supervises a long-running foreground process.
type ProcessSpec struct {
	ExecSpec `yaml:",inline"`

	Health   ProcessHealth   `yaml:"health,omitempty" json:"health"`
	Limits   *Limits         `yaml:"limits,omitempty" json:"limits,omitempty"`
	Recovery *RecoveryPolicy `yaml:"recovery,omitempty" json:"recovery,omitempty"`
}

// ProcessHealth tunes supervision timing.
type ProcessHealth struct {
	// StartupGracePeriod: a process must stay up this long to be healthy;
	// exits inside it count as failed starts.
	StartupGracePeriod Duration `yaml:"startup_grace_period,omitempty" json:"startup_grace_period"`
	// CheckInterval: resource sampling interval.
	CheckInterval Duration `yaml:"check_interval,omitempty" json:"check_interval"`
	// StopSignal is sent first on stop; SIGKILL follows after StopTimeout.
	StopSignal  string   `yaml:"stop_signal,omitempty" json:"stop_signal"`
	StopTimeout Duration `yaml:"stop_timeout,omitempty" json:"stop_timeout"`
}

// LimitAction is applied when a resource limit is exceeded.
type LimitAction string

// Limit actions.
const (
	LimitLog     LimitAction = "log"
	LimitNotify  LimitAction = "notify"
	LimitRestart LimitAction = "restart"
	LimitStop    LimitAction = "stop"
	LimitKill    LimitAction = "kill"
)

// Limits models CPU/memory thresholds for supervised processes. They are
// enforced by sampling (not by cgroups); see docs/configuration.md.
type Limits struct {
	// MaxCPUPercent is relative to one CPU (200 = two full cores).
	MaxCPUPercent  float64  `yaml:"max_cpu_percent,omitempty" json:"max_cpu_percent,omitempty"`
	MaxMemoryBytes ByteSize `yaml:"max_memory_bytes,omitempty" json:"max_memory_bytes,omitempty"`
	// SustainedFor: the limit must be exceeded continuously this long.
	SustainedFor Duration    `yaml:"sustained_for,omitempty" json:"sustained_for"`
	Action       LimitAction `yaml:"action,omitempty" json:"action"`
}

// HTTPSpec probes an HTTP(S) endpoint.
type HTTPSpec struct {
	URL             string            `yaml:"url" json:"url"`
	Method          string            `yaml:"method,omitempty" json:"method"`
	Headers         map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`
	Body            string            `yaml:"body,omitempty" json:"body,omitempty"`
	Timeout         Duration          `yaml:"timeout,omitempty" json:"timeout"`
	CheckInterval   Duration          `yaml:"check_interval,omitempty" json:"check_interval"`
	FollowRedirects bool              `yaml:"follow_redirects,omitempty" json:"follow_redirects"`
	MaxRedirects    int               `yaml:"max_redirects,omitempty" json:"max_redirects"`
	TLS             TLSConfig         `yaml:"tls,omitempty" json:"tls"`
	Expect          HTTPExpect        `yaml:"expect,omitempty" json:"expect"`
	FailurePolicy   FailurePolicy     `yaml:"failure_policy,omitempty" json:"failure_policy"`
}

// HTTPExpect defines a successful HTTP response.
type HTTPExpect struct {
	// StatusCodes accepted; empty means any 2xx.
	StatusCodes  []int  `yaml:"status_codes,omitempty" json:"status_codes,omitempty"`
	BodyContains string `yaml:"body_contains,omitempty" json:"body_contains,omitempty"`
	// BodyMaxBytes caps how much of the body is read.
	BodyMaxBytes ByteSize `yaml:"body_max_bytes,omitempty" json:"body_max_bytes"`
}

// ConcurrencyPolicy decides what happens when a cron job is due while the
// previous run is still active.
type ConcurrencyPolicy string

// Concurrency policies.
const (
	ConcurrencyForbid  ConcurrencyPolicy = "forbid"
	ConcurrencyAllow   ConcurrencyPolicy = "allow"
	ConcurrencyReplace ConcurrencyPolicy = "replace"
)

// MissedRunPolicy decides what happens to runs missed while sentineld was
// not running.
type MissedRunPolicy string

// Missed run policies.
const (
	MissedSkip    MissedRunPolicy = "skip"
	MissedRunOnce MissedRunPolicy = "run_once"
)

// CronSpec runs a command on a cron schedule.
type CronSpec struct {
	ExecSpec `yaml:",inline"`

	Schedule          string            `yaml:"schedule" json:"schedule"`
	Timezone          string            `yaml:"timezone,omitempty" json:"timezone,omitempty"`
	Timeout           Duration          `yaml:"timeout,omitempty" json:"timeout"`
	ConcurrencyPolicy ConcurrencyPolicy `yaml:"concurrency_policy,omitempty" json:"concurrency_policy"`
	MissedRuns        MissedRunPolicy   `yaml:"missed_runs,omitempty" json:"missed_runs"`
	// StopTimeout: grace period between SIGTERM and SIGKILL on timeout.
	StopTimeout Duration `yaml:"stop_timeout,omitempty" json:"stop_timeout"`
}

func boolOr(b *bool, def bool) bool {
	if b == nil {
		return def
	}
	return *b
}
