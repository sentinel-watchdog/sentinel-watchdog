package model

import (
	"fmt"
	"slices"
)

// SupervisorType identifies the kind of target a supervisor watches.
type SupervisorType string

// Supervisor types implemented in the MVP.
const (
	SupervisorSystemd SupervisorType = "systemd"
	SupervisorProcess SupervisorType = "process"
	SupervisorHTTP    SupervisorType = "http"
	SupervisorCron    SupervisorType = "cron"
)

// Supervisor types reserved for later releases. Configurations using them are
// rejected with an explicit "not implemented" error.
const (
	SupervisorPort         SupervisorType = "port"
	SupervisorMount        SupervisorType = "mount"
	SupervisorResource     SupervisorType = "resource"
	SupervisorLog          SupervisorType = "log"
	SupervisorOpenRC       SupervisorType = "openrc"
	SupervisorProcessGroup SupervisorType = "process_group"
)

// ImplementedSupervisorTypes lists the supervisor types accepted by this release.
func ImplementedSupervisorTypes() []SupervisorType {
	return []SupervisorType{SupervisorSystemd, SupervisorProcess, SupervisorHTTP, SupervisorCron}
}

// PlannedSupervisorTypes lists reserved supervisor types that are not implemented yet.
func PlannedSupervisorTypes() []SupervisorType {
	return []SupervisorType{SupervisorPort, SupervisorMount, SupervisorResource, SupervisorLog, SupervisorOpenRC, SupervisorProcessGroup}
}

// IsImplemented reports whether t is usable in this release.
func (t SupervisorType) IsImplemented() bool {
	return slices.Contains(ImplementedSupervisorTypes(), t)
}

// IsPlanned reports whether t is reserved for a future release.
func (t SupervisorType) IsPlanned() bool {
	return slices.Contains(PlannedSupervisorTypes(), t)
}

// State is the lifecycle state of a supervisor as reported by status commands
// and notifications.
type State string

// Supervisor states.
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
		return fmt.Errorf("unknown supervisor state %q", string(s))
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
