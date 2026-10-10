package api

import "time"

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
	IgnoredDirs []string `json:"ignored_dirs,omitempty"`
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
