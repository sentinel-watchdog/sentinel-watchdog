// Package config loads Sentinel's configuration: the central file
// (sentinel.yaml) and one directory per enabled module (ADR-0015).
//
// Loading is a pipeline: check ownership and modes -> read files -> parse
// YAML nodes -> expand ${VARIABLE} references in scalar values -> reject
// unknown keys -> decode -> apply defaults -> validate. The core decodes
// and validates its own sections (daemon, notifications, module switches);
// every module receives its part as a ModuleConfig and validates it. This
// package never imports a module. See docs/configuration.md.
package config

import (
	"strconv"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/logging"
)

// SchemaVersion is the only configuration schema version understood by this
// release. Every file, central or module, declares it.
const SchemaVersion = 1

// Config is the loaded and validated central configuration, plus the raw
// sections of every module named under `modules`.
type Config struct {
	Daemon        Daemon        `yaml:"daemon" json:"daemon"`
	Notifications Notifications `yaml:"notifications" json:"notifications"`
	// Modules lists every module named in the central file, sorted by
	// name, enabled or not.
	Modules []ModuleConfig `yaml:"-" json:"-"`

	// Files lists the files read, in load order.
	Files []string `yaml:"-" json:"-"`
	// Warnings are non-fatal findings (insecure but legal settings).
	Warnings []Problem `yaml:"-" json:"-"`
	// IgnoredDirs lists existing directories of known modules that were
	// not read because the module is disabled or not available.
	IgnoredDirs []string `yaml:"-" json:"-"`
}

// Module returns the configuration of the named module, if it is named in
// the central file.
func (c *Config) Module(name string) (ModuleConfig, bool) {
	for _, m := range c.Modules {
		if m.Name == name {
			return m, true
		}
	}
	return ModuleConfig{}, false
}

// Channel returns a copy of the notification channel with the given name,
// like Module: callers cannot change the loaded configuration.
func (c *Config) Channel(name string) (Channel, bool) {
	for _, ch := range c.Notifications.Channels {
		if ch.Name == name {
			return ch, true
		}
	}
	return Channel{}, false
}

// Daemon holds daemon-wide settings. Only the central file sets them.
type Daemon struct {
	// Socket is the path of the control socket used by sentinelctl.
	Socket string `yaml:"socket" json:"socket"`
	// SocketMode and SocketGroup decide who may connect; connecting grants
	// the read tier (ADR-0012).
	SocketMode  FileMode `yaml:"socket_mode" json:"socket_mode"`
	SocketGroup string   `yaml:"socket_group,omitempty" json:"socket_group,omitempty"`
	// StateDir holds the state files of the core and of every module.
	StateDir string `yaml:"state_dir" json:"state_dir"`
	// Timezone is an IANA name (or "Local") used for schedules and reports.
	Timezone        string   `yaml:"timezone" json:"timezone"`
	ShutdownTimeout Duration `yaml:"shutdown_timeout" json:"shutdown_timeout"`
	Log             Log      `yaml:"log" json:"log"`
	Access          Access   `yaml:"access" json:"access"`
}

// Log configures sentineld's own logging. The logging package owns the
// valid levels and formats; the configuration accepts exactly those.
type Log struct {
	Level  string         `yaml:"level" json:"level"`
	Format logging.Format `yaml:"format" json:"format"`
}

// Access configures the authorization tiers above read (ADR-0012). Empty
// groups mean "root only".
type Access struct {
	OperatorGroup string `yaml:"operator_group,omitempty" json:"operator_group,omitempty"`
	AdminGroup    string `yaml:"admin_group,omitempty" json:"admin_group,omitempty"`
}

// Notifications holds the delivery channels shared by every module.
type Notifications struct {
	Channels []Channel `yaml:"channels,omitempty" json:"channels,omitempty"`
}

// ChannelType names a notification provider.
type ChannelType string

// Notification provider types.
const (
	ChannelWebhook ChannelType = "webhook"
	// Planned providers; rejected with a "not implemented" error.
	ChannelSlack ChannelType = "slack"
	ChannelTeams ChannelType = "teams"
)

// Channel is one notification destination.
type Channel struct {
	Name    string            `yaml:"name" json:"name"`
	Type    ChannelType       `yaml:"type" json:"type"`
	Enabled *bool             `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	URL     string            `yaml:"url" json:"url"`
	Method  string            `yaml:"method,omitempty" json:"method,omitempty"`
	Timeout Duration          `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`
	Retry   RetryPolicy       `yaml:"retry,omitempty" json:"retry"`
	// SuccessStatusCodes lists HTTP status codes treated as delivered.
	// Empty means any 2xx.
	SuccessStatusCodes []int     `yaml:"success_status_codes,omitempty" json:"success_status_codes,omitempty"`
	TLS                TLSConfig `yaml:"tls,omitempty" json:"tls"`
}

// IsEnabled reports whether the channel delivers notifications (default true).
func (c Channel) IsEnabled() bool { return c.Enabled == nil || *c.Enabled }

// Backoff selects how retry delays grow.
type Backoff string

// Backoff strategies.
const (
	BackoffFixed       Backoff = "fixed"
	BackoffExponential Backoff = "exponential"
)

// RetryPolicy bounds delivery retries.
type RetryPolicy struct {
	// Attempts is the total number of delivery attempts, including the first.
	Attempts int      `yaml:"attempts,omitempty" json:"attempts"`
	Delay    Duration `yaml:"delay,omitempty" json:"delay"`
	Backoff  Backoff  `yaml:"backoff,omitempty" json:"backoff"`
	MaxDelay Duration `yaml:"max_delay,omitempty" json:"max_delay"`
}

// TLSConfig tunes certificate verification for HTTPS endpoints.
type TLSConfig struct {
	// CAFile is a PEM bundle used instead of the system roots.
	CAFile     string `yaml:"ca_file,omitempty" json:"ca_file,omitempty"`
	ServerName string `yaml:"server_name,omitempty" json:"server_name,omitempty"`
	// InsecureSkipVerify disables certificate verification. Never default.
	InsecureSkipVerify bool `yaml:"insecure_skip_verify,omitempty" json:"insecure_skip_verify,omitempty"`
}

// ModuleAvailability tells the loader what this binary can do with a
// module name. The daemon derives it from its module registry.
type ModuleAvailability int

// Module availabilities.
const (
	// ModuleAvailable: built in and implemented; its directory is read
	// when enabled.
	ModuleAvailable ModuleAvailability = iota + 1
	// ModulePlanned: named in the roadmap, not implemented in this release.
	ModulePlanned
	// ModuleNotBuilt: implemented but excluded from this binary with a
	// build tag.
	ModuleNotBuilt
)

func (a ModuleAvailability) String() string {
	switch a {
	case ModuleAvailable:
		return "available"
	case ModulePlanned:
		return "planned"
	case ModuleNotBuilt:
		return "not built"
	}
	return "unknown"
}

// ModuleConfig is everything the configuration says about one module. The
// module validates it in its Configure method.
type ModuleConfig struct {
	Name string
	// Enabled is the module switch from the central file (default false).
	Enabled bool
	// Availability of the module in this binary.
	Availability ModuleAvailability
	// Central is the module's block in the central file without the
	// `enabled` key: its safety gates and switches (ADR-0015 rule 1).
	Central Section
	// Dir is the module directory, next to the central file.
	Dir string
	// DirExists reports whether Dir exists. Only read directories are
	// reported: a disabled module's directory is not read.
	DirExists bool
	// Files holds one section per file in Dir, in lexical order, without
	// the `version` key.
	Files []Section
}

// itemPath returns "list[name]" for valid names, "list[index]" otherwise.
func itemPath(list, name string, idx int) string {
	if nameRe.MatchString(name) {
		return list + "[" + name + "]"
	}
	return list + "[" + strconv.Itoa(idx) + "]"
}
