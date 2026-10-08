package config

import (
	"slices"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/redact"
)

// Redacted returns a copy of the core configuration that is safe to print
// (`sentinelctl config show`): webhook URLs keep only scheme and host, and
// header values are masked except for well-known safe headers.
//
// Module sections are not included: each module renders its own
// configuration with its own redaction rules.
func (c *Config) Redacted() *Config {
	out := &Config{
		Daemon:      c.Daemon,
		Files:       slices.Clone(c.Files),
		Warnings:    slices.Clone(c.Warnings),
		IgnoredDirs: slices.Clone(c.IgnoredDirs),
	}
	out.Notifications.Core = Route{
		Channels: slices.Clone(c.Notifications.Core.Channels),
		Events:   slices.Clone(c.Notifications.Core.Events),
	}
	out.Notifications.Channels = make([]Channel, len(c.Notifications.Channels))
	for i, ch := range c.Notifications.Channels {
		ch = ch.clone()
		ch.URL = redact.WebhookURL(ch.URL)
		ch.Headers = redact.Headers(ch.Headers)
		out.Notifications.Channels[i] = ch
	}
	return out
}
