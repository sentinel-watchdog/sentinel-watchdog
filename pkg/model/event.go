package model

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"time"
)

// EventType classifies an event. The values double as notification filter
// names in the configuration (`notifications.events`).
type EventType string

// Supervisor events (systemd, process, http and future check-style supervisors).
const (
	EventSupervisorFailed    EventType = "supervisor_failed"
	EventRecoveryStarted     EventType = "recovery_started"
	EventRecoveryExhausted   EventType = "recovery_exhausted"
	EventSupervisorRecovered EventType = "supervisor_recovered"
)

// Job events (cron).
const (
	EventJobSucceeded EventType = "job_succeeded"
	EventJobFailed    EventType = "job_failed"
	EventJobTimeout   EventType = "job_timeout"
)

// Daemon events, not tied to a supervisor.
const (
	EventConfigurationError EventType = "configuration_error"
	EventDaemonError        EventType = "daemon_error"
)

// EventScope tells which emitter an event type belongs to.
type EventScope string

// Event scopes.
const (
	ScopeSupervisor EventScope = "supervisor"
	ScopeJob        EventScope = "job"
	ScopeDaemon     EventScope = "daemon"
)

var eventScopes = map[EventType]EventScope{
	EventSupervisorFailed:    ScopeSupervisor,
	EventRecoveryStarted:     ScopeSupervisor,
	EventRecoveryExhausted:   ScopeSupervisor,
	EventSupervisorRecovered: ScopeSupervisor,
	EventJobSucceeded:        ScopeJob,
	EventJobFailed:           ScopeJob,
	EventJobTimeout:          ScopeJob,
	EventConfigurationError:  ScopeDaemon,
	EventDaemonError:         ScopeDaemon,
}

// Scope returns the scope of t and false when t is unknown.
func (t EventType) Scope() (EventScope, bool) {
	s, ok := eventScopes[t]
	return s, ok
}

// Validate returns an error when t is not a known event type.
func (t EventType) Validate() error {
	if _, ok := eventScopes[t]; !ok {
		return fmt.Errorf("unknown event type %q", string(t))
	}
	return nil
}

// EventTypesForScope returns the event types of a scope in a stable order.
func EventTypesForScope(scope EventScope) []EventType {
	var out []EventType
	for t, s := range eventScopes {
		if s == scope {
			out = append(out, t)
		}
	}
	slices.Sort(out)
	return out
}

// ScopeForSupervisorType returns the event scope emitted by a supervisor type.
func ScopeForSupervisorType(mt SupervisorType) EventScope {
	if mt == SupervisorCron {
		return ScopeJob
	}
	return ScopeSupervisor
}

// Event is a single occurrence recorded by Sentinel. It is persisted in the
// state file, returned by `sentinelctl events` and sent as the JSON body of
// webhook notifications.
//
// Messages and LastError must already be redacted by the emitter: an Event
// never carries secrets.
type Event struct {
	ID              string            `json:"event_id"`
	Timestamp       time.Time         `json:"timestamp"`
	Hostname        string            `json:"hostname"`
	SentinelVersion string            `json:"sentinel_version"`
	SupervisorName  string            `json:"supervisor_name,omitempty"`
	SupervisorType  SupervisorType    `json:"supervisor_type,omitempty"`
	State           State             `json:"state,omitempty"`
	PreviousState   State             `json:"previous_state,omitempty"`
	Type            EventType         `json:"event_type"`
	Message         string            `json:"message"`
	FailureCount    int               `json:"failure_count"`
	RestartCount    int               `json:"restart_count"`
	LastError       string            `json:"last_error,omitempty"`
	Metadata        map[string]string `json:"metadata,omitempty"`
}

// Validate checks the invariants every event must satisfy.
func (e Event) Validate() error {
	var errs []error
	if e.ID == "" {
		errs = append(errs, errors.New("event_id is empty"))
	}
	if e.Timestamp.IsZero() {
		errs = append(errs, errors.New("timestamp is zero"))
	}
	scope, ok := e.Type.Scope()
	if !ok {
		errs = append(errs, e.Type.Validate())
	}
	if ok && scope != ScopeDaemon && e.SupervisorName == "" {
		errs = append(errs, fmt.Errorf("event %q requires supervisor_name", e.Type))
	}
	if e.State != "" {
		if err := e.State.Validate(); err != nil {
			errs = append(errs, err)
		}
	}
	if e.PreviousState != "" {
		if err := e.PreviousState.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("previous_state: %w", err))
		}
	}
	return errors.Join(errs...)
}

// NewEventID returns a random 128-bit identifier encoded as 32 hex chars.
func NewEventID() string {
	var b [16]byte
	// crypto/rand.Read never returns an error since Go 1.24; it aborts the
	// program if the system random source is unavailable.
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
