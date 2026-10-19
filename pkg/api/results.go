package api

import (
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/model"
)

// Status is the result of core.status.
type Status struct {
	Version   string    `json:"version"`
	StartedAt time.Time `json:"started_at"`
	// UptimeSeconds is measured on the daemon's clock.
	UptimeSeconds int64        `json:"uptime_seconds"`
	Modules       []ModuleInfo `json:"modules"`
	// NotifyDropped and LogDropped count events lost by the notification
	// and logging subscribers because their queue was full.
	NotifyDropped uint64         `json:"notify_dropped"`
	LogDropped    uint64         `json:"log_dropped"`
	Channels      []ChannelStats `json:"channels"`
	// IgnoredDirs are module directories not read because their module is
	// disabled or not available (ADR-0015).
	IgnoredDirs []string    `json:"ignored_dirs,omitempty"`
	Audit       AuditStatus `json:"audit"`
	// ConfigDigest names the configuration running (config.Config.Digest).
	ConfigDigest string `json:"config_digest"`
	// NotifyAbandoned counts deliveries queued when a reload replaced the
	// notification channels: they were not sent (D-077).
	NotifyAbandoned uint64 `json:"notify_abandoned"`
	// LastReload is the outcome of the last reload, if any.
	LastReload *ReloadInfo `json:"last_reload,omitempty"`
}

// ReloadInfo summarises a reload for status.
type ReloadInfo struct {
	At time.Time `json:"at"`
	// Result is "ok", "partial" (applied, some modules failed) or
	// "rejected" (nothing changed).
	Result string `json:"result"`
	Digest string `json:"digest,omitempty"`
}

// ReloadResult is the result of core.reload.
type ReloadResult struct {
	// Digest names the configuration now running; OldDigest the one before.
	Digest    string `json:"digest"`
	OldDigest string `json:"old_digest"`
	// Restarted, Started and Stopped list the modules the reload acted on;
	// Failed those that did not start or stop.
	Restarted []string `json:"restarted,omitempty"`
	Started   []string `json:"started,omitempty"`
	Stopped   []string `json:"stopped,omitempty"`
	Failed    []string `json:"failed,omitempty"`
	// Warnings are changes that need a restart of sentineld and modules
	// that could not be restarted.
	Warnings []string `json:"warnings,omitempty"`
}

// AuditStatus is the integrity of the audit chain. While it is "broken",
// operate and admin requests are refused.
type AuditStatus struct {
	// Integrity is "ok" or "broken".
	Integrity string `json:"integrity"`
	// Reason is a fixed code ("bad_record", "head_ahead", "write_failed",
	// ...) and Where names the file and line, when broken.
	Reason string `json:"reason,omitempty"`
	Where  string `json:"where,omitempty"`
	// Seq is the sequence number of the last record written.
	Seq uint64 `json:"seq"`
}

// AuditResult is the result of core.audit.
type AuditResult struct {
	Audit AuditStatus `json:"audit"`
	// Records are the last records, oldest first.
	Records []AuditRecord `json:"records"`
	// Skipped counts lines that are not valid records (a broken chain).
	Skipped int `json:"skipped,omitempty"`
}

// AuditRecord is one record of the audit log, in its file format
// (ADR-0011, docs/security.md).
type AuditRecord struct {
	Seq           uint64            `json:"seq"`
	Time          time.Time         `json:"time"`
	Actor         AuditActor        `json:"actor"`
	Action        string            `json:"action"`
	Target        string            `json:"target,omitempty"`
	Result        string            `json:"result"`
	CorrelationID string            `json:"correlation_id,omitempty"`
	Detail        map[string]string `json:"detail,omitempty"`
	PrevHash      string            `json:"prev_hash"`
	Hash          string            `json:"hash"`
}

// AuditActor is who asked: kind "socket", "signal" or "daemon"; uid, gid
// and pid when known.
type AuditActor struct {
	Kind string  `json:"kind"`
	UID  *uint32 `json:"uid,omitempty"`
	GID  *uint32 `json:"gid,omitempty"`
	PID  int32   `json:"pid,omitempty"`
}

// ModuleInfo describes one module known to the binary; core.modules
// returns them sorted by name.
type ModuleInfo struct {
	Name string `json:"name"`
	// Availability is "available", "planned" or "not built".
	Availability string `json:"availability"`
	Enabled      bool   `json:"enabled"`
	// State is the lifecycle state of a configured module ("running",
	// "failed", ...); empty when the module is not configured.
	State string `json:"state,omitempty"`
}

// ChannelStats are the counters of one notification channel.
type ChannelStats struct {
	Name           string `json:"name"`
	Delivered      uint64 `json:"delivered"`
	Failed         uint64 `json:"failed"`
	Dropped        uint64 `json:"dropped"`
	Suppressed     uint64 `json:"suppressed"`
	SuppressedLost uint64 `json:"suppressed_lost"`
}

// Bounds of core.events' limit. MaxEventsLimit is also how many events
// sentineld keeps (D-083).
const (
	DefaultEventsLimit = 50
	MaxEventsLimit     = 256
)

// EventsArgs are the arguments of core.events.
type EventsArgs struct {
	// Limit is how many of the most recent events to return: 1 to
	// MaxEventsLimit, DefaultEventsLimit when absent.
	Limit *int `json:"limit,omitzero"`
}

// EventsResult is the result of core.events.
type EventsResult struct {
	// Events are the most recent events, newest first. They are kept in
	// memory only: a restart of sentineld empties them.
	Events []model.Event `json:"events"`
	// Dropped counts, since sentineld started, the events that never
	// reached the history (nor the log) because its queue was full; events
	// that left the history because newer ones arrived are not counted.
	Dropped uint64 `json:"dropped"`
}
