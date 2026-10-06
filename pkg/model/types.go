package model

import (
	"fmt"
	"slices"
)

// MonitorType identifies the kind of target a monitor watches.
type MonitorType string

// Monitor types implemented in the MVP.
const (
	MonitorSystemd MonitorType = "systemd"
	MonitorProcess MonitorType = "process"
	MonitorHTTP    MonitorType = "http"
	MonitorCron    MonitorType = "cron"
)

// Monitor types reserved for later releases. Configurations using them are
// rejected with an explicit "not implemented" error.
const (
	MonitorPort         MonitorType = "port"
	MonitorMount        MonitorType = "mount"
	MonitorResource     MonitorType = "resource"
	MonitorLog          MonitorType = "log"
	MonitorOpenRC       MonitorType = "openrc"
	MonitorProcessGroup MonitorType = "process_group"
)

// ImplementedMonitorTypes lists the monitor types accepted by this release.
func ImplementedMonitorTypes() []MonitorType {
	return []MonitorType{MonitorSystemd, MonitorProcess, MonitorHTTP, MonitorCron}
}

// PlannedMonitorTypes lists reserved monitor types that are not implemented yet.
func PlannedMonitorTypes() []MonitorType {
	return []MonitorType{MonitorPort, MonitorMount, MonitorResource, MonitorLog, MonitorOpenRC, MonitorProcessGroup}
}

// IsImplemented reports whether t is usable in this release.
func (t MonitorType) IsImplemented() bool {
	return slices.Contains(ImplementedMonitorTypes(), t)
}

// IsPlanned reports whether t is reserved for a future release.
func (t MonitorType) IsPlanned() bool {
	return slices.Contains(PlannedMonitorTypes(), t)
}

// State is the lifecycle state of a monitor as reported by status commands
// and notifications.
type State string

// Monitor states.
const (
	// StateUnknown: not checked yet, or the last check could not decide.
	StateUnknown State = "unknown"
	// StateStarting: a supervised process is inside its startup grace period.
	StateStarting State = "starting"
	// StateRunning: a scheduled job is currently executing.
	StateRunning State = "running"
	// StateHealthy: the last check (or job run) succeeded.
	StateHealthy State = "healthy"
	// StateFailing: checks fail but the failure threshold is not reached yet.
	StateFailing State = "failing"
	// StateFailed: the failure threshold is reached; recovery may start.
	StateFailed State = "failed"
	// StateRecovering: a recovery action is scheduled or in progress.
	StateRecovering State = "recovering"
	// StateExhausted: recovery attempts are exhausted; operator action or
	// cooldown is required.
	StateExhausted State = "exhausted"
	// StateStopped: stopped on operator request.
	StateStopped State = "stopped"
	// StateDisabled: disabled by configuration or by the operator.
	StateDisabled State = "disabled"
)

var allStates = []State{
	StateUnknown, StateStarting, StateRunning, StateHealthy, StateFailing,
	StateFailed, StateRecovering, StateExhausted, StateStopped, StateDisabled,
}

// Validate returns an error when s is not a known state.
func (s State) Validate() error {
	if !slices.Contains(allStates, s) {
		return fmt.Errorf("unknown monitor state %q", string(s))
	}
	return nil
}

// CapabilityStatus describes whether a feature (systemd access, resource
// sampling, a limit) can be used on this host.
type CapabilityStatus string

// Capability statuses.
const (
	CapabilitySupported        CapabilityStatus = "supported"
	CapabilityUnsupported      CapabilityStatus = "unsupported"
	CapabilityUnavailable      CapabilityStatus = "unavailable"
	CapabilityPermissionDenied CapabilityStatus = "permission_denied"
	CapabilityError            CapabilityStatus = "error"
)

var allCapabilityStatuses = []CapabilityStatus{
	CapabilitySupported, CapabilityUnsupported, CapabilityUnavailable,
	CapabilityPermissionDenied, CapabilityError,
}

// Validate returns an error when c is not a known capability status.
func (c CapabilityStatus) Validate() error {
	if !slices.Contains(allCapabilityStatuses, c) {
		return fmt.Errorf("unknown capability status %q", string(c))
	}
	return nil
}
