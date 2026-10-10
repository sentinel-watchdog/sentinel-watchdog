package config

import (
	"slices"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/redact"
)

// RedactText masks in s every value of the configuration that may be
// secret: the environment values expanded in any file read, webhook URLs
// and header values. Text from outside the core (a module's error) passes
// through it before it is logged: modules may quote what they were given.
func (c *Config) RedactText(s string) string {
	secrets := slices.Clone(c.secrets)
	for _, ch := range c.Notifications.Channels {
		secrets = append(secrets, ch.URL)
		for _, v := range ch.Headers {
			secrets = append(secrets, v)
		}
	}
	return redact.Text(s, secretForms(secrets)...)
}

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
