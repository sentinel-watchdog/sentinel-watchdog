package config

import (
	"github.com/sentinel-watchdog/sentinel/internal/redact"
)

// Redacted returns a deep-enough copy of c that is safe to print: webhook
// URLs keep only scheme and host, header values are masked except for
// well-known safe headers, URL credentials and query values are removed,
// and sensitive environment variables are masked.
//
// Expanded ${VARIABLE} values can appear anywhere; fields that commonly
// carry secrets are covered here. Scripts and arguments are shown as
// written because they are needed for troubleshooting: keep secrets out of
// them and pass them through environment variables instead.
func (c *Config) Redacted() *Config {
	out := *c
	out.Notifications = make([]NotificationChannel, len(c.Notifications))
	for i, n := range c.Notifications {
		n.URL = redact.WebhookURL(n.URL)
		n.Headers = redact.Headers(n.Headers)
		out.Notifications[i] = n
	}
	out.Monitors = make([]Monitor, len(c.Monitors))
	for i, m := range c.Monitors {
		switch {
		case m.Process != nil:
			p := *m.Process
			p.ExecSpec = redactExec(p.ExecSpec)
			m.Process = &p
		case m.Cron != nil:
			cr := *m.Cron
			cr.ExecSpec = redactExec(cr.ExecSpec)
			m.Cron = &cr
		case m.HTTP != nil:
			h := *m.HTTP
			h.URL = redact.URL(h.URL)
			h.Headers = redact.Headers(h.Headers)
			if h.Body != "" {
				h.Body = redact.Placeholder
			}
			m.HTTP = &h
		}
		out.Monitors[i] = m
	}
	return &out
}

func redactExec(e ExecSpec) ExecSpec {
	e.Environment = redact.Environment(e.Environment)
	return e
}
