// Package state defines the persistent runtime state of sentineld and
// stores it as a versioned JSON file with atomic replacement.
//
// The state file never contains configuration or secrets: only counters,
// timestamps, states and already-redacted event messages.
package state

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/model"
)

// SchemaVersion is the state file schema written by this release.
const SchemaVersion = 1

// File is the root document of the state file.
type File struct {
	SchemaVersion int                 `json:"schema_version"`
	UpdatedAt     time.Time           `json:"updated_at,omitzero"`
	Monitors      map[string]*Monitor `json:"monitors"`
	// Events is the global, bounded event log (newest last).
	Events []model.Event `json:"events"`
}

// New returns an empty state at the current schema version.
func New() *File {
	return &File{SchemaVersion: SchemaVersion, Monitors: map[string]*Monitor{}, Events: []model.Event{}}
}

// Monitor is the persisted state of one monitor.
type Monitor struct {
	Name         string            `json:"monitor_name"`
	Type         model.MonitorType `json:"monitor_type"`
	CurrentState model.State       `json:"current_state"`
	// Enabled is the effective value: configuration enabled and not
	// disabled by the operator.
	Enabled bool `json:"enabled"`
	// OperatorDisabled records `sentinelctl disable`; it survives restarts.
	OperatorDisabled bool `json:"operator_disabled,omitempty"`

	// RestartCount: restarts performed by Sentinel (lifetime).
	RestartCount int `json:"restart_count"`
	// FailureCount: failures detected (lifetime).
	FailureCount         int `json:"failure_count"`
	ConsecutiveFailures  int `json:"consecutive_failures"`
	ConsecutiveSuccesses int `json:"consecutive_successes"`
	// TotalRecoveries: transitions back to healthy after a failure.
	TotalRecoveries int `json:"total_recoveries"`
	// RestartAttempts holds the timestamps of recovery attempts inside the
	// current recovery window, so restart-loop protection survives daemon
	// restarts.
	RestartAttempts []time.Time `json:"restart_attempts,omitempty"`

	LastCheck      time.Time `json:"last_check,omitzero"`
	LastTransition time.Time `json:"last_transition,omitzero"`
	LastFailure    time.Time `json:"last_failure,omitzero"`
	LastRecovery   time.Time `json:"last_recovery,omitzero"`
	ExhaustedAt    time.Time `json:"exhausted_at,omitzero"`
	LastError      string    `json:"last_error,omitempty"`
	LastExitCode   *int      `json:"last_exit_code,omitempty"`
	LastEvent      *Summary  `json:"last_event,omitempty"`

	// Job is set for cron monitors.
	Job *Job `json:"job,omitempty"`

	// History is the bounded per-monitor event history (newest last).
	History []Summary `json:"history,omitempty"`
}

// Summary is a compact event record.
type Summary struct {
	EventID       string          `json:"event_id"`
	Time          time.Time       `json:"time"`
	Type          model.EventType `json:"event_type"`
	State         model.State     `json:"state,omitempty"`
	PreviousState model.State     `json:"previous_state,omitempty"`
	Message       string          `json:"message,omitempty"`
}

// JobOutcome is the result of a cron job run.
type JobOutcome string

// Job outcomes.
const (
	JobSucceeded JobOutcome = "succeeded"
	JobFailed    JobOutcome = "failed"
	JobTimeout   JobOutcome = "timeout"
)

// Job is the persisted state of a scheduled job.
type Job struct {
	// LastScheduled is the last schedule slot that was handled (run or
	// skipped). It prevents duplicate runs after a daemon restart.
	LastScheduled  time.Time  `json:"last_scheduled,omitzero"`
	LastStarted    time.Time  `json:"last_started,omitzero"`
	LastFinished   time.Time  `json:"last_finished,omitzero"`
	LastDurationMS int64      `json:"last_duration_ms,omitempty"`
	LastOutcome    JobOutcome `json:"last_outcome,omitempty"`
	Runs           int        `json:"runs"`
	Failures       int        `json:"failures"`
}

// Retention bounds the state file size.
type Retention struct {
	// History is the maximum number of entries per monitor.
	History int
	// Events is the maximum number of entries in the global event log.
	Events int
}

// Monitor returns the state of the named monitor, creating it in the
// unknown state if absent.
func (f *File) Monitor(name string, typ model.MonitorType) *Monitor {
	if f.Monitors == nil {
		f.Monitors = map[string]*Monitor{}
	}
	m, ok := f.Monitors[name]
	if !ok {
		m = &Monitor{Name: name, Type: typ, CurrentState: model.StateUnknown, Enabled: true}
		f.Monitors[name] = m
	}
	return m
}

// AppendEvent records ev in the global log and, for monitor events, in the
// monitor history and LastEvent, then applies retention.
func (f *File) AppendEvent(ev model.Event, r Retention) {
	f.Events = append(f.Events, ev)
	if ev.MonitorName != "" {
		m := f.Monitor(ev.MonitorName, ev.MonitorType)
		s := Summary{
			EventID: ev.ID, Time: ev.Timestamp, Type: ev.Type,
			State: ev.State, PreviousState: ev.PreviousState, Message: ev.Message,
		}
		m.LastEvent = &s
		m.History = append(m.History, s)
	}
	f.Trim(r)
}

// Trim drops the oldest history and event entries beyond r. Zero limits
// mean "keep nothing".
func (f *File) Trim(r Retention) {
	f.Events = keepLast(f.Events, r.Events)
	for _, m := range f.Monitors {
		m.History = keepLast(m.History, r.History)
	}
}

// Prune removes monitors for which keep returns false, e.g. monitors that
// no longer exist in the configuration. Global events are kept.
func (f *File) Prune(keep func(name string) bool) []string {
	var removed []string
	for name := range f.Monitors {
		if !keep(name) {
			delete(f.Monitors, name)
			removed = append(removed, name)
		}
	}
	slices.Sort(removed)
	return removed
}

// Validate checks structural invariants after decoding.
func (f *File) Validate() error {
	var errs []error
	if f.SchemaVersion != SchemaVersion {
		errs = append(errs, fmt.Errorf("schema_version %d, expected %d", f.SchemaVersion, SchemaVersion))
	}
	for key, m := range f.Monitors {
		if m == nil {
			errs = append(errs, fmt.Errorf("monitor %q: null entry", key))
			continue
		}
		if m.Name != key {
			errs = append(errs, fmt.Errorf("monitor %q: monitor_name is %q", key, m.Name))
		}
		if err := m.CurrentState.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("monitor %q: %w", key, err))
		}
		if m.RestartCount < 0 || m.FailureCount < 0 || m.ConsecutiveFailures < 0 ||
			m.ConsecutiveSuccesses < 0 || m.TotalRecoveries < 0 {
			errs = append(errs, fmt.Errorf("monitor %q: negative counter", key))
		}
	}
	return errors.Join(errs...)
}

func keepLast[T any](s []T, n int) []T {
	if n < 0 {
		n = 0
	}
	if len(s) <= n {
		return s
	}
	// Copy so the dropped prefix can be garbage collected.
	return slices.Clone(s[len(s)-n:])
}
