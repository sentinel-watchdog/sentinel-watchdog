package model

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// Event is something that happened on the host, emitted by a module and
// delivered to the state, notification and audit consumers (ADR-0002 as
// amended by ADR-0015). Its JSON form is the `event` object of the webhook
// payload (docs/notifications.md).
type Event struct {
	ID              string    `json:"event_id"`
	Timestamp       time.Time `json:"timestamp"`
	Hostname        string    `json:"hostname"`
	SentinelVersion string    `json:"sentinel_version"`
	// Module is the module that emitted the event ("core", "supervisor").
	Module string `json:"module"`
	// Source names the emitter inside the module: a service name, a
	// provider name, or "daemon".
	Source     string     `json:"source"`
	SourceType SourceType `json:"source_type"`
	Type       EventType  `json:"event_type"`
	Severity   Severity   `json:"severity"`
	// State and PreviousState are the emitter's state, in the vocabulary
	// of its source type.
	State         string `json:"state,omitempty"`
	PreviousState string `json:"previous_state,omitempty"`
	Message       string `json:"message"`
	// CorrelationID groups the events of one incident or operation.
	CorrelationID string `json:"correlation_id,omitempty"`
	// Metadata holds small string labels, usable by filters.
	Metadata map[string]string `json:"metadata,omitempty"`
	// Attributes holds structured details: JSON scalars, lists of strings
	// and one nested level of the same (limits in internal/core/events).
	Attributes map[string]any `json:"attributes,omitempty"`
}

// EventType names what happened ("service_failed"). Every module registers
// the types it emits; an unregistered type is rejected.
type EventType string

// SourceType names the kind of emitter ("systemd", "cron", "daemon").
type SourceType string

// ModuleCore is the Module of the events the core itself emits.
const ModuleCore = "core"

// SourceTypeDaemon is the SourceType of the events the core emits.
const SourceTypeDaemon SourceType = "daemon"

// Events of the core.
const (
	// EventDaemonError reports a failure of sentineld itself.
	EventDaemonError EventType = "daemon_error"
	// EventConfigurationError reports a configuration that could not be
	// loaded or applied.
	EventConfigurationError EventType = "configuration_error"
)

// CoreEventTypes lists the event types of the core, in a stable order.
func CoreEventTypes() []EventType {
	return []EventType{EventConfigurationError, EventDaemonError}
}

// Severity grades an event. The emitter sets it; configuration cannot
// lower it.
type Severity string

// Severities, from the least to the most severe.
const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityError    Severity = "error"
	SeverityCritical Severity = "critical"
)

// Rank orders severities: 1 for info up to 4 for critical, 0 for a value
// that is not a severity.
func (s Severity) Rank() int {
	switch s {
	case SeverityInfo:
		return 1
	case SeverityWarning:
		return 2
	case SeverityError:
		return 3
	case SeverityCritical:
		return 4
	}
	return 0
}

// NewEventID returns a random 128-bit identifier encoded as 32 hex digits.
func NewEventID() string {
	var b [16]byte
	// crypto/rand.Read never returns an error since Go 1.24: it aborts the
	// program if the system random source is unavailable.
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
