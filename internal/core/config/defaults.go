package config

import (
	"maps"
	"net/http"
	"strings"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/logging"
)

// Default values. A zero or omitted field takes the default; validation
// runs afterwards, so explicit invalid values are still rejected.
const (
	DefaultMainFile                 = "/etc/sentinel/sentinel.yaml"
	DefaultSocket                   = "/run/sentinel/sentinel.sock"
	DefaultSocketMode      FileMode = 0o660
	DefaultStateDir                 = "/var/lib/sentinel"
	DefaultTimezone                 = "Local"
	DefaultLogLevel                 = "info"
	DefaultShutdownTimeout          = Duration(30 * time.Second)

	defaultChannelMethod   = http.MethodPost
	defaultChannelTimeout  = Duration(10 * time.Second)
	defaultRetryAttempts   = 3
	defaultRetryDelay      = Duration(5 * time.Second)
	defaultRetryMaxDelay   = Duration(time.Minute)
	defaultContentTypeJSON = "application/json"
)

func applyDefaults(c *Config) {
	d := &c.Daemon
	setStr(&d.Socket, DefaultSocket)
	if d.SocketMode == 0 {
		d.SocketMode = DefaultSocketMode
	}
	setStr(&d.StateDir, DefaultStateDir)
	setStr(&d.Timezone, DefaultTimezone)
	if d.ShutdownTimeout == 0 {
		d.ShutdownTimeout = DefaultShutdownTimeout
	}
	setStr(&d.Log.Level, DefaultLogLevel)
	if d.Log.Format == "" {
		d.Log.Format = logging.FormatAuto
	}
	for i := range c.Notifications.Channels {
		defaultChannel(&c.Notifications.Channels[i])
	}
}

func defaultChannel(n *Channel) {
	setStr(&n.Method, defaultChannelMethod)
	n.Method = strings.ToUpper(n.Method)
	if n.Timeout == 0 {
		n.Timeout = defaultChannelTimeout
	}
	if n.Retry.Attempts == 0 {
		n.Retry.Attempts = defaultRetryAttempts
	}
	if n.Retry.Delay == 0 {
		n.Retry.Delay = defaultRetryDelay
	}
	if n.Retry.Backoff == "" {
		n.Retry.Backoff = BackoffExponential
	}
	if n.Retry.MaxDelay == 0 {
		n.Retry.MaxDelay = defaultRetryMaxDelay
	}
	if !hasHeader(n.Headers, "Content-Type") {
		h := make(map[string]string, len(n.Headers)+1)
		maps.Copy(h, n.Headers)
		h["Content-Type"] = defaultContentTypeJSON
		n.Headers = h
	}
}

func setStr(p *string, def string) {
	if *p == "" {
		*p = def
	}
}

func hasHeader(h map[string]string, name string) bool {
	for k := range h {
		if strings.EqualFold(k, name) {
			return true
		}
	}
	return false
}
