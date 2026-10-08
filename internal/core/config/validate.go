package config

import (
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	// Embed the IANA time zone database: minimal systems (Alpine,
	// containers) often lack /usr/share/zoneinfo (D-017).
	_ "time/tzdata"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/logging"
)

var (
	// nameRe is the format of channel names and other user-chosen names (D-011).
	nameRe       = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
	accountRe    = regexp.MustCompile(`^([a-z_][a-z0-9_.-]{0,31}\$?|[0-9]{1,10})$`)
	headerNameRe = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")
	backoffs     = []Backoff{BackoffFixed, BackoffExponential}
	channelVerbs = []string{http.MethodPost, http.MethodPut}
)

// Bounds for numeric settings.
const (
	maxUnixPath       = 107 // sun_path is 108 bytes including the NUL
	maxHTTPTimeout    = Duration(5 * time.Minute)
	minChannelTimeout = Duration(100 * time.Millisecond)
)

// validator collects errors and warnings for one file.
type validator struct {
	file     string
	problems problems
}

func (v *validator) errorf(path, format string, args ...any) {
	v.problems.errorf(v.file, path, format, args...)
}

func (v *validator) warnf(path, format string, args ...any) {
	v.problems.warnf(v.file, path, format, args...)
}

// validateCentral checks the core sections of the central file after
// defaults have been applied.
func validateCentral(file string, c *Config) (warnings, errs []Problem) {
	v := &validator{file: file}
	v.daemon(&c.Daemon)
	seen := map[string]bool{}
	for i := range c.Notifications.Channels {
		ch := &c.Notifications.Channels[i]
		if seen[ch.Name] && ch.Name != "" {
			v.errorf(itemPath("notifications.channels", ch.Name, i), "duplicate channel name %q", ch.Name)
			continue
		}
		seen[ch.Name] = true
		v.channel(ch, i)
	}
	return v.problems.warns, v.problems.errs
}

func (v *validator) daemon(d *Daemon) {
	if v.absPath("daemon.socket", d.Socket) && len(d.Socket) > maxUnixPath {
		v.errorf("daemon.socket", "path is longer than %d bytes (Unix socket limit)", maxUnixPath)
	}
	if d.SocketMode.Perm()&0o002 != 0 {
		v.errorf("daemon.socket_mode", "%s is world-writable; any local user could control sentineld", d.SocketMode)
	}
	if d.SocketMode.Perm()&0o600 != 0o600 {
		v.errorf("daemon.socket_mode", "%s must grant read and write to the owner", d.SocketMode)
	}
	v.account("daemon.socket_group", d.SocketGroup)
	v.absPath("daemon.state_dir", d.StateDir)
	if _, err := time.LoadLocation(d.Timezone); err != nil {
		v.errorf("daemon.timezone", "unknown time zone %q (use an IANA name such as \"Europe/Rome\", or \"Local\")", d.Timezone)
	}
	v.durationRange("daemon.shutdown_timeout", d.ShutdownTimeout, Duration(time.Second), Duration(10*time.Minute))
	if _, err := logging.ParseLevel(d.Log.Level); err != nil {
		v.errorf("daemon.log.level", "%s", err)
	}
	if _, err := logging.ParseFormat(string(d.Log.Format)); err != nil {
		v.errorf("daemon.log.format", "%s", err)
	}
	v.account("daemon.access.operator_group", d.Access.OperatorGroup)
	v.account("daemon.access.admin_group", d.Access.AdminGroup)
	if d.Access.AdminGroup != "" {
		v.warnf("daemon.access.admin_group", "members of %q get admin rights (firewall changes): treat the group as root-equivalent", d.Access.AdminGroup)
	}
}

func (v *validator) channel(n *Channel, idx int) {
	p := itemPath("notifications.channels", n.Name, idx)
	if !nameRe.MatchString(n.Name) {
		v.errorf(p+".name", "%q must match %s", n.Name, nameRe)
	}
	switch n.Type {
	case ChannelWebhook:
	case ChannelSlack, ChannelTeams:
		v.errorf(p+".type", "%q is planned but not implemented in this release; use type \"webhook\"", n.Type)
		return
	case "":
		v.errorf(p+".type", "is required (webhook)")
		return
	default:
		v.errorf(p+".type", "unknown notification type %q (supported: webhook)", n.Type)
		return
	}
	if v.httpURL(p+".url", n.URL) && strings.HasPrefix(n.URL, "http://") {
		v.warnf(p+".url", "plain HTTP sends notification content unencrypted")
	}
	oneOf(v, p+".method", n.Method, channelVerbs)
	v.durationRange(p+".timeout", n.Timeout, minChannelTimeout, maxHTTPTimeout)
	v.headers(p+".headers", n.Headers)
	v.intRange(p+".retry.attempts", n.Retry.Attempts, 1, 10)
	v.durationRange(p+".retry.delay", n.Retry.Delay, 0, Duration(24*time.Hour))
	v.durationRange(p+".retry.max_delay", n.Retry.MaxDelay, 0, Duration(24*time.Hour))
	if n.Retry.MaxDelay < n.Retry.Delay {
		v.errorf(p+".retry.max_delay", "(%s) must be greater than or equal to delay (%s)", n.Retry.MaxDelay, n.Retry.Delay)
	}
	oneOf(v, p+".retry.backoff", n.Retry.Backoff, backoffs)
	for _, c := range n.SuccessStatusCodes {
		if c < 100 || c > 599 {
			v.errorf(p+".success_status_codes", "%d is not a valid HTTP status code", c)
		}
	}
	if n.TLS.CAFile != "" {
		v.absPath(p+".tls.ca_file", n.TLS.CAFile)
	}
	if n.TLS.InsecureSkipVerify {
		v.warnf(p+".tls.insecure_skip_verify", "TLS certificate verification is disabled")
	}
}

func (v *validator) account(p, name string) {
	if name != "" && !accountRe.MatchString(name) {
		v.errorf(p, "invalid group name %q", name)
	}
}

func (v *validator) httpURL(p, raw string) bool {
	if raw == "" {
		v.errorf(p, "is required")
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		// url errors echo the input, which may contain secrets.
		v.errorf(p, "is not a valid URL")
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		v.errorf(p, "scheme must be http or https")
		return false
	}
	if u.Host == "" {
		v.errorf(p, "host is missing")
		return false
	}
	return true
}

func (v *validator) headers(p string, h map[string]string) {
	for _, k := range sortedKeys(h) {
		if !headerNameRe.MatchString(k) {
			v.errorf(p, "invalid header name %q", k)
		}
		if strings.ContainsAny(h[k], "\r\n\x00") {
			v.errorf(p+"."+k, "value contains CR, LF or NUL")
		}
	}
}

func (v *validator) absPath(p, path string) bool {
	switch {
	case path == "":
		v.errorf(p, "is required")
	case strings.ContainsRune(path, 0):
		v.errorf(p, "contains a NUL byte")
	case !filepath.IsAbs(path):
		v.errorf(p, "%q must be an absolute path", path)
	case filepath.Clean(path) != path:
		v.errorf(p, "%q must be a clean path (no '..', '.', '//' or trailing '/'); use %q", path, filepath.Clean(path))
	default:
		return true
	}
	return false
}

func (v *validator) durationRange(p string, d, minD, maxD Duration) {
	if d < minD || d > maxD {
		v.errorf(p, "%s is out of range (%s - %s)", d, minD, maxD)
	}
}

func (v *validator) intRange(p string, n, minN, maxN int) {
	if n < minN || n > maxN {
		v.errorf(p, "%d is out of range (%d - %d)", n, minN, maxN)
	}
}

func oneOf[T ~string](v *validator, p string, got T, allowed []T) {
	if slices.Contains(allowed, got) {
		return
	}
	names := make([]string, len(allowed))
	for i, a := range allowed {
		names[i] = string(a)
	}
	v.errorf(p, "%q is not valid (allowed: %s)", string(got), strings.Join(names, ", "))
}
