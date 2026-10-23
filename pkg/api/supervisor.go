package api

import "time"

// The supervisor module's commands (D-080 (5)): reading the services'
// state is not an action, so both are read.
var (
	SupervisorList   = Command{Name: "supervisor.list", Tier: TierRead, Summary: "supervised services and their state"}
	SupervisorStatus = Command{Name: "supervisor.status", Tier: TierRead, Summary: "details of one or every supervised service"}
)

// SupervisorStatusArgs are the arguments of supervisor.status: one
// service by name, or every service when Name is empty.
type SupervisorStatusArgs struct {
	Name string `json:"name,omitempty"`
}

// SupervisorListResult is the result of supervisor.list, sorted by name.
type SupervisorListResult struct {
	Services []ServiceSummary `json:"services"`
}

// ServiceSummary is one line of supervisor.list.
type ServiceSummary struct {
	Name string `json:"name"`
	Type string `json:"type"`
	// State is "unknown", "healthy" or "failed".
	State string `json:"state"`
	// Since is when the service entered State; zero while unknown.
	Since time.Time `json:"since,omitzero"`
}

// SupervisorStatusResult is the result of supervisor.status.
type SupervisorStatusResult struct {
	MaxConcurrentChecks int `json:"max_concurrent_checks"`
	// ChecksInFlight counts the checks running now, stalled ones included.
	ChecksInFlight int `json:"checks_in_flight"`
	// StateNotSaved is set while the last save of the state file failed:
	// supervision goes on, but a restart would forget what changed since.
	StateNotSaved bool `json:"state_not_saved,omitempty"`
	// ProcessesArmed reports modules.supervisor.processes: without it,
	// process services are validated but not started.
	ProcessesArmed bool `json:"processes_armed"`
	// PendingCleanups counts the process groups left by a previous run
	// that have not been cleaned up yet.
	PendingCleanups int `json:"pending_cleanups,omitempty"`
	// StateRecovered is set when the state file was corrupt at start: it
	// was moved aside and the recovery budgets of process services were
	// treated as spent for one window.
	StateRecovered bool            `json:"state_recovered,omitempty"`
	Services       []ServiceStatus `json:"services"`
}

// ServiceStatus is the state of one service. Its endpoint is the scheme,
// host and port of its URL only: a path or a query may hold a secret.
type ServiceStatus struct {
	Name          string `json:"name"`
	Type          string `json:"type"`
	Endpoint      string `json:"endpoint"`
	State         string `json:"state"`
	CheckInterval string `json:"check_interval"`
	Timeout       string `json:"timeout"`
	// Since is when the service entered State.
	Since     time.Time `json:"since,omitzero"`
	LastCheck time.Time `json:"last_check,omitzero"`
	// LastFailure is the fixed code of the last failed check, if any.
	LastFailure          string `json:"last_failure,omitempty"`
	ConsecutiveFailures  int    `json:"consecutive_failures"`
	ConsecutiveSuccesses int    `json:"consecutive_successes"`
	// IncidentID correlates the service_failed event of the open incident
	// with its service_recovered.
	IncidentID string `json:"incident_id,omitempty"`
	Checks     uint64 `json:"checks"`
	Failures   uint64 `json:"failures"`
	// LastDurationMS is how long the last check took once it had a slot;
	// LastSlotWaitMS how long it waited for one (max_concurrent_checks).
	LastDurationMS int64 `json:"last_duration_ms"`
	LastSlotWaitMS int64 `json:"last_slot_wait_ms"`
	// WaitingSince is set while the service waits for a slot: other checks
	// hold them all. Waiting is not a failure of the endpoint.
	WaitingSince time.Time `json:"waiting_since,omitzero"`
	// Stalled is set while a check that timed out has not returned yet:
	// no new check starts for the service until it does.
	Stalled bool `json:"stalled,omitempty"`

	// Process services only. Phase is not_armed, starting, running,
	// exited, start_failed, restarting, exhausted, blocked, stopping or
	// stopped; Pid and StartedAt are set
	// while the process runs. Neither the program, its arguments, its
	// environment nor its user are shown: status reaches every read peer.
	Phase     string      `json:"phase,omitempty"`
	Pid       int         `json:"pid,omitempty"`
	StartedAt time.Time   `json:"started_at,omitzero"`
	Starts    uint64      `json:"starts,omitempty"`
	LastExit  *ExitStatus `json:"last_exit,omitempty"`
	// OutputLost is set when the process's output could not be written
	// (the output file failed): the rest of it was discarded.
	OutputLost bool `json:"output_lost,omitempty"`
	// Recovery: automatic restarts launched so far; the attempts in the
	// current window out of MaxAttempts; while restarting or blocked, the
	// next launch; while exhausted, since when and when the cooldown ends.
	// Phase is also restarting, exhausted or blocked; BlockedReason is
	// audit_unavailable or state_unavailable.
	Restarts      uint64    `json:"restarts,omitempty"`
	Attempts      int       `json:"attempts,omitempty"`
	MaxAttempts   int       `json:"max_attempts,omitempty"`
	NextAttemptAt time.Time `json:"next_attempt_at,omitzero"`
	ExhaustedAt   time.Time `json:"exhausted_at,omitzero"`
	ResumeAt      time.Time `json:"resume_at,omitzero"`
	BlockedReason string    `json:"blocked_reason,omitempty"`
}

// ExitStatus is how a process ended: Code is -1 when Signal ended it.
type ExitStatus struct {
	Code   int       `json:"code"`
	Signal string    `json:"signal,omitempty"`
	At     time.Time `json:"at"`
}
