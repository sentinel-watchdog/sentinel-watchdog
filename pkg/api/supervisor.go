package api

import "time"

// The supervisor module's commands (D-080 (5)): reading the services'
// state is not an action, so both are read.
var (
	SupervisorList   = Command{Name: "supervisor.list", Tier: TierRead, Summary: "supervised services and their state"}
	SupervisorStatus = Command{Name: "supervisor.status", Tier: TierRead, Summary: "details of one or every supervised service"}
)

// The supervisor's operator commands (D-089): each acts on the service
// its arguments name (SupervisorActionArgs), at tier operate, audited with
// the service as target. None takes a program, an argument, an
// environment or a user: they stop, resume or reset what root configured.
var (
	SupervisorStart = Command{Name: "supervisor.start", Tier: TierOperate, TargetsName: true,
		Summary: "start a process service an operator stopped"}
	SupervisorStop = Command{Name: "supervisor.stop", Tier: TierOperate, TargetsName: true,
		Summary: "stop a process service and keep it stopped until start"}
	SupervisorRestart = Command{Name: "supervisor.restart", Tier: TierOperate, TargetsName: true,
		Summary: "stop and start a process service, or restart a systemd unit"}
	SupervisorEnable = Command{Name: "supervisor.enable", Tier: TierOperate, TargetsName: true,
		Summary: "supervise or schedule again a service or job an operator disabled"}
	SupervisorDisable = Command{Name: "supervisor.disable", Tier: TierOperate, TargetsName: true,
		Summary: "take a service or job out of supervision until enable"}
	SupervisorReset = Command{Name: "supervisor.reset", Tier: TierOperate, TargetsName: true,
		Summary: "clear the recovery budget of a service"}
)

// SupervisorLogs reads a service's or job's recent output (D-099). Output
// may hold what a program was given, so it is operate, and audited, like the
// commands that act (ADR-0012). Its 15 s hold the 10 s of journalctl.
var SupervisorLogs = Command{Name: "supervisor.logs", Tier: TierOperate, TargetsName: true,
	Timeout: 15 * time.Second, Summary: "recent output of a service or job"}

// Bounds of supervisor.logs' limit.
const (
	DefaultLogLines = 50
	MaxLogLines     = 1000
)

// SupervisorLogsArgs are the arguments of supervisor.logs.
type SupervisorLogsArgs struct {
	Name string `json:"name"`
	// Limit is how many of the last lines to return: 1 to MaxLogLines,
	// DefaultLogLines when absent.
	Limit *int `json:"limit,omitzero"`
}

// LogSource says where the lines of supervisor.logs come from.
type LogSource string

// The sources of supervisor.logs.
const (
	LogSourceJournal LogSource = "journal"
	LogSourceFile    LogSource = "file"
)

// LogLine is one line of output. Time is absent for a file, which has none.
// RunID names the job run that wrote the line, for output sent to the journal
// by sentineld (D-100).
type LogLine struct {
	Time  time.Time `json:"time,omitzero"`
	Text  string    `json:"text"`
	RunID string    `json:"run_id,omitempty"`
}

// SupervisorLogsResult is the result of supervisor.logs, oldest line first.
// Truncated: lines that were asked for may be missing, because a bound was
// reached or the source reported a problem. Text comes from outside: a client
// must not print it to a terminal as it is.
type SupervisorLogsResult struct {
	Name      string    `json:"name"`
	Source    LogSource `json:"source"`
	Lines     []LogLine `json:"lines"`
	Truncated bool      `json:"truncated,omitempty"`
}

// SupervisorActionArgs are the arguments of an operator command: the
// service it acts on.
type SupervisorActionArgs struct {
	Name string `json:"name"`
}

// SupervisorActionResult is the result of an operator command. The
// command was accepted; Done tells whether it had finished when the reply
// was due (a stop waits for the process's stop_timeout): if not, the
// service's status shows how it ends.
type SupervisorActionResult struct {
	Name   string `json:"name"`
	Action string `json:"action"`
	// Changed is false when the service was already as asked (a stop of
	// a stopped service).
	Changed bool `json:"changed"`
	Done    bool `json:"done"`
	// NotSaved: the change applies but the state file could not be
	// written, so a restart of sentineld would not remember it. Only a
	// change that takes away (stop, disable) is applied unsaved.
	NotSaved bool `json:"not_saved,omitempty"`
	// Phase is the service's phase when the reply was sent.
	Phase string `json:"phase,omitempty"`
}

// SupervisorStatusArgs are the arguments of supervisor.status: one
// service by name, or every service when Name is empty.
type SupervisorStatusArgs struct {
	Name string `json:"name,omitempty"`
}

// SupervisorListResult is the result of supervisor.list, sorted by name.
type SupervisorListResult struct {
	Services []ServiceSummary `json:"services"`
	// Jobs are the configured cron jobs (Phase 3c-4), sorted by name.
	Jobs []JobSummary `json:"jobs"`
}

// JobSummary is one job line of supervisor.list.
type JobSummary struct {
	Name     string `json:"name"`
	Schedule string `json:"schedule"`
	Timezone string `json:"timezone"`
	// Phase is "scheduled", "running", "blocked", "not_armed" or "disabled".
	Phase   string    `json:"phase"`
	NextRun time.Time `json:"next_run,omitzero"`
	// LastResult is how the last run ended: "succeeded", "failed" or
	// "timed_out"; empty before the first.
	LastResult string `json:"last_result,omitempty"`
}

// ServiceSummary is one line of supervisor.list.
type ServiceSummary struct {
	Name string `json:"name"`
	Type string `json:"type"`
	// State is "unknown", "healthy" or "failed".
	State string `json:"state"`
	// Since is when the service entered State; zero while unknown.
	Since time.Time `json:"since,omitzero"`
	// Disabled is set while the service is not run or checked: by its
	// configuration (enabled: false) or by an operator (D-020).
	Disabled bool `json:"disabled,omitempty"`
}

// SupervisorStatusResult is the result of supervisor.status.
type SupervisorStatusResult struct {
	MaxConcurrentChecks int `json:"max_concurrent_checks"`
	// MaxConcurrentRuns bounds the runs of jobs under concurrency_policy:
	// allow, in the whole module.
	MaxConcurrentRuns int `json:"max_concurrent_runs"`
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
	StateRecovered bool `json:"state_recovered,omitempty"`
	// OutputDropped counts the lines of programs' log output that a full
	// journal socket refused since the module started (D-100). Lines journald
	// takes and then loses or rate-limits are not counted: delivery to the
	// journal is best effort.
	OutputDropped uint64          `json:"output_dropped,omitempty"`
	Services      []ServiceStatus `json:"services"`
	// Jobs are the configured cron jobs (Phase 3c-4), sorted by name.
	Jobs []JobStatus `json:"jobs"`
}

// JobStatus is the status of one cron job.
type JobStatus struct {
	Name     string `json:"name"`
	Schedule string `json:"schedule"`
	Timezone string `json:"timezone"`
	Timeout  string `json:"timeout"`
	// ConcurrencyPolicy is forbid, allow or replace; MissedRuns skip or
	// run_once.
	ConcurrencyPolicy string `json:"concurrency_policy"`
	MissedRuns        string `json:"missed_runs"`
	// Phase is as in JobSummary; BlockedReason says why a launch was
	// refused (audit_unavailable, state_unavailable, records_full,
	// reload_stop_failed), that the last slot was skipped at
	// max_concurrent_runs (runs_full), or that the clock is behind the
	// watermark (clock_behind_watermark). The last two do not change the
	// phase.
	Phase         string    `json:"phase"`
	BlockedReason string    `json:"blocked_reason,omitempty"`
	NextRun       time.Time `json:"next_run,omitzero"`
	// CatchUpPending: under run_once, a run for the slots missed is due and
	// waits for its turn; NextRun is then the first of those slots.
	CatchUpPending bool `json:"catch_up_pending,omitempty"`
	// Watermark: every slot at or before it is consumed and never runs
	// again. Ahead of the clock (clock_behind_watermark) the job waits for
	// the clock; more than a day ahead it is reset at the next reload or
	// start.
	Watermark time.Time `json:"watermark,omitzero"`
	// ActiveRuns are the runs not over yet: running, or kept until their
	// process group is confirmed empty.
	ActiveRuns []JobRun     `json:"active_runs"`
	Last       *JobRunEnded `json:"last,omitempty"`
	// Runs counts the launches; Skipped the slots not run because a run or
	// record of the job was not over, at max_concurrent_runs, or that
	// passed during a replace's stop; Missed the catch-ups, each of which
	// covers every slot older than a minute (after downtime or a forward
	// clock jump), the last one between LastMissedFrom and LastMissedTo:
	// under missed_runs: skip they are not run, under run_once the latest
	// runs.
	Runs           uint64    `json:"runs"`
	Succeeded      uint64    `json:"succeeded"`
	Failed         uint64    `json:"failed"`
	TimedOut       uint64    `json:"timed_out"`
	Skipped        uint64    `json:"skipped"`
	Missed         uint64    `json:"missed"`
	LastMissedFrom time.Time `json:"last_missed_from,omitzero"`
	LastMissedTo   time.Time `json:"last_missed_to,omitzero"`
}

// JobRun is a run of a job that is not over.
type JobRun struct {
	RunID     string    `json:"run_id"`
	Scheduled time.Time `json:"scheduled"`
	Started   time.Time `json:"started,omitzero"`
	Pid       int       `json:"pid,omitempty"`
	// Phase is "reserved", "running" or "cleanup_pending".
	Phase string `json:"phase"`
}

// JobRunEnded is how a run ended.
type JobRunEnded struct {
	RunID     string    `json:"run_id"`
	Scheduled time.Time `json:"scheduled"`
	Started   time.Time `json:"started,omitzero"`
	Ended     time.Time `json:"ended"`
	Result    string    `json:"result"`
	Failure   string    `json:"failure,omitempty"`
	ExitCode  int       `json:"exit_code"`
	Signal    string    `json:"signal,omitempty"`
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
	// Disabled says who took the service out of supervision:
	// "configuration" (enabled: false, which wins, D-020) or "operator"
	// (supervisor disable). StoppedBy is "operator" while a process service
	// is held stopped by supervisor stop. Both last across restarts.
	Disabled  string `json:"disabled,omitempty"`
	StoppedBy string `json:"stopped_by,omitempty"`

	// Phase is not_armed, starting, running, exited, start_failed,
	// restarting, exhausted, blocked, stopping, stopped or disabled for a
	// process service, disabled, blocked or empty for an http one, and
	// also unavailable or transition for a systemd one. The rest is
	// for process services only: Pid and StartedAt are set
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
	// MemoryLimit is limits.memory in bytes (D-090). With a limit, while
	// the process runs: MemoryRSS is its leader's resident memory sampled
	// at MemorySampledAt, and MemoryUnknown is set while samples fail (the
	// process keeps running, its limit unchecked).
	MemoryLimit     uint64    `json:"memory_limit,omitempty"`
	MemoryRSS       uint64    `json:"memory_rss,omitempty"`
	MemorySampledAt time.Time `json:"memory_sampled_at,omitzero"`
	MemoryUnknown   bool      `json:"memory_unknown,omitempty"`
	// Recovery: automatic restarts launched so far; the attempts in the
	// current window out of MaxAttempts; while restarting or blocked, the
	// next launch; while exhausted, since when and when the cooldown ends.
	// Phase is also restarting, exhausted or blocked; BlockedReason is
	// audit_unavailable or state_unavailable, or reload_stop_failed when
	// the old instance of the service did not stop for a reload and the
	// new definition waits until sentineld restarts (D-088; also set on an
	// http service), or operator_stop_failed when it did not stop for an
	// operator's command (D-089), with the same wait. ResumeAt is zero
	// while exhausted with a cooldown of 0: until supervisor reset.
	Restarts      uint64    `json:"restarts,omitempty"`
	Attempts      int       `json:"attempts,omitempty"`
	MaxAttempts   int       `json:"max_attempts,omitempty"`
	NextAttemptAt time.Time `json:"next_attempt_at,omitzero"`
	ExhaustedAt   time.Time `json:"exhausted_at,omitzero"`
	ResumeAt      time.Time `json:"resume_at,omitzero"`
	BlockedReason string    `json:"blocked_reason,omitempty"`

	// systemd services (D-093): the unit observed and its last state
	// read; the restarts systemd made that sentineld saw between two
	// checks; and why the last check decided nothing (Phase unavailable or
	// transition): systemd_unavailable, unit_changing, unit_invalid or
	// unit_transition.
	Unit         string `json:"unit,omitempty"`
	ActiveState  string `json:"active_state,omitempty"`
	SubState     string `json:"sub_state,omitempty"`
	UnitRestarts uint64 `json:"unit_restarts,omitempty"`
	Undecided    string `json:"undecided,omitempty"`
}

// ExitStatus is how a process ended: Code is -1 when Signal ended it.
type ExitStatus struct {
	Code   int       `json:"code"`
	Signal string    `json:"signal,omitempty"`
	At     time.Time `json:"at"`
}
