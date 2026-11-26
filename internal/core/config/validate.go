package config

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	// Embed the IANA time zone database: minimal systems (Alpine,
	// containers) often lack /usr/share/zoneinfo (D-017).
	_ "time/tzdata"

	"go.yaml.in/yaml/v3"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/logging"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/redact"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/model"
)

var (
	// nameRe is the format of channel names and other user-chosen names (D-011).
	nameRe       = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
	accountRe    = regexp.MustCompile(`^[a-z_][a-z0-9_.-]{0,31}\$?$`) // names; ids are NumericID
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

// MaxShutdownTimeout is the largest daemon.shutdown_timeout: what a
// reload may raise it to bounds the control socket's longest exchange.
const MaxShutdownTimeout = 10 * time.Minute

// validator collects errors and warnings for one file, each located at
// the line of the value it is about (loc).
type validator struct {
	file     string
	problems problems
	// checkCAFile is LoadOptions.CheckCAFile; nil checks nothing more.
	checkCAFile func(path string) error
}

func (v *validator) errorf(l loc, format string, args ...any) {
	v.errorAt(l.line(), l.path, format, args...)
}

// errorAt records an error at an explicit line, for a problem about a key
// rather than its value.
func (v *validator) errorAt(line int, path, format string, args ...any) {
	v.problems.errorf(v.file, path, "%s", atLine(line, fmt.Sprintf(format, args...)))
}

func (v *validator) warnf(l loc, format string, args ...any) {
	v.problems.warnf(v.file, l.path, "%s", atLine(l.line(), fmt.Sprintf(format, args...)))
}

// validateCentral checks the core sections of the central file after
// defaults have been applied. root is the file's root mapping, from which
// c was decoded: it locates each problem. checkCA, if set, is called with
// the CA bundle of every enabled channel that has no other problem
// (LoadOptions.CheckCAFile).
func validateCentral(file string, c *Config, root *yaml.Node, checkCA func(path string) error) (warnings, errs []Problem) {
	v := &validator{file: file, checkCAFile: checkCA}
	at := rootLoc(root)
	v.daemon(at.key("daemon"), &c.Daemon)
	notifications := at.key("notifications")
	core := notifications.key("core")
	for _, p := range c.Notifications.Core.Problems(file, core.path, channelNames(c), model.CoreEventTypes()) {
		p.Message = atLine(core.line(), p.Message)
		v.problems.errs = append(v.problems.errs, p)
	}
	channels := notifications.key("channels")
	items := channels.list()
	seen := map[string]bool{}
	for i := range c.Notifications.Channels {
		ch := &c.Notifications.Channels[i]
		at := items.item(i, itemPath(channels.path, ch.Name, i))
		if seen[ch.Name] && ch.Name != "" {
			v.errorf(at, "duplicate channel name %q", ch.Name)
			continue
		}
		seen[ch.Name] = true
		before := len(v.problems.errs)
		v.channel(at, ch)
		if len(v.problems.errs) == before && ch.IsEnabled() && ch.TLS.CAFile != "" && v.checkCAFile != nil {
			if err := v.checkCAFile(ch.TLS.CAFile); err != nil {
				v.errorf(at.key("tls").key("ca_file"), "%s", err)
			}
		}
	}
	return v.problems.warns, v.problems.errs
}

func (v *validator) daemon(at loc, d *Daemon) {
	socket := at.key("socket")
	if v.absPath(socket, d.Socket) && len(d.Socket) > maxUnixPath {
		v.errorf(socket, "path is longer than %d bytes (Unix socket limit)", maxUnixPath)
	}
	mode := at.key("socket_mode")
	if d.SocketMode.Perm()&0o002 != 0 {
		v.errorf(mode, "%s is world-writable; any local user could control sentineld", d.SocketMode)
	}
	if d.SocketMode.Perm()&0o600 != 0o600 {
		v.errorf(mode, "%s must grant read and write to the owner", d.SocketMode)
	}
	group := at.key("socket_group")
	v.account(group, d.SocketGroup)
	// chown takes an int, 32 bits on some platforms (Daemon.listen).
	if id, numeric, err := NumericID(d.SocketGroup); numeric && err == nil && id > math.MaxInt32 {
		v.errorf(group, "group id %d is out of range for the socket (at most %d)", id, math.MaxInt32)
	}
	v.absPath(at.key("state_dir"), d.StateDir)
	if _, err := time.LoadLocation(d.Timezone); err != nil {
		v.errorf(at.key("timezone"), "unknown time zone %q (use an IANA name such as \"Europe/Rome\", or \"Local\")", d.Timezone)
	}
	v.durationRange(at.key("shutdown_timeout"), d.ShutdownTimeout, Duration(time.Second), Duration(MaxShutdownTimeout))
	log := at.key("log")
	if _, err := logging.ParseLevel(d.Log.Level); err != nil {
		v.errorf(log.key("level"), "%s", err)
	}
	if _, err := logging.ParseFormat(string(d.Log.Format)); err != nil {
		v.errorf(log.key("format"), "%s", err)
	}
	access := at.key("access")
	v.account(access.key("operator_group"), d.Access.OperatorGroup)
	admin := access.key("admin_group")
	v.account(admin, d.Access.AdminGroup)
	if d.Access.AdminGroup != "" {
		v.warnf(admin, "members of %q get admin rights (firewall changes): treat the group as root-equivalent", d.Access.AdminGroup)
	}
}

func (v *validator) channel(at loc, n *Channel) {
	if !nameRe.MatchString(n.Name) {
		v.errorf(at.key("name"), "%q must match %s", n.Name, nameRe)
	}
	typ := at.key("type")
	switch n.Type {
	case ChannelWebhook:
	case ChannelSlack, ChannelTeams:
		v.errorf(typ, "%q is planned but not implemented in this release; use type \"webhook\"", n.Type)
		return
	case "":
		v.errorf(typ, "is required (webhook)")
		return
	default:
		v.errorf(typ, "unknown notification type %q (supported: webhook)", n.Type)
		return
	}
	if u := at.key("url"); v.httpURL(u, n.URL) && strings.HasPrefix(n.URL, "http://") {
		v.warnf(u, "plain HTTP sends notification content unencrypted")
	}
	oneOf(v, at.key("method"), n.Method, channelVerbs)
	v.durationRange(at.key("timeout"), n.Timeout, minChannelTimeout, maxHTTPTimeout)
	v.headers(at.key("headers"), n.Headers)
	retry := at.key("retry")
	v.intRange(retry.key("attempts"), n.Retry.Attempts, 1, 10)
	v.durationRange(retry.key("delay"), n.Retry.Delay, 0, Duration(24*time.Hour))
	v.durationRange(retry.key("max_delay"), n.Retry.MaxDelay, 0, Duration(24*time.Hour))
	if n.Retry.MaxDelay < n.Retry.Delay {
		v.errorf(retry.key("max_delay"), "(%s) must be greater than or equal to delay (%s)", n.Retry.MaxDelay, n.Retry.Delay)
	}
	oneOf(v, retry.key("backoff"), n.Retry.Backoff, backoffs)
	codes := at.key("success_status_codes")
	list := codes.list()
	for i, c := range n.SuccessStatusCodes {
		code := list.item(i, codes.path)
		switch {
		case c < 100 || c > 599:
			v.errorf(code, "%d is not a valid HTTP status code", c)
		case c >= 300 && c < 400:
			v.errorf(code, "%d is a redirect: redirects are never followed, so never a success", c)
		}
	}
	tls := at.key("tls")
	if n.TLS.CAFile != "" {
		v.absPath(tls.key("ca_file"), n.TLS.CAFile)
	}
	v.durationRange(at.key("repeat_interval"), n.RepeatInterval, 0, Duration(24*time.Hour))
	if n.TLS.InsecureSkipVerify {
		v.warnf(tls.key("insecure_skip_verify"), "TLS certificate verification is disabled")
	}
}

// channelNames returns the names of the configured channels.
func channelNames(c *Config) []string {
	names := make([]string, len(c.Notifications.Channels))
	for i, ch := range c.Notifications.Channels {
		names[i] = ch.Name
	}
	return names
}

func (v *validator) account(p loc, name string) {
	if name == "" {
		return
	}
	if _, numeric, err := NumericID(name); numeric {
		if err != nil {
			v.errorf(p, "invalid group id %q: %s", name, err)
		}
		return
	}
	if !accountRe.MatchString(name) {
		v.errorf(p, "invalid group name %q", name)
	}
}

// errBadID reports digits that do not make an account id.
var errBadID = errors.New("not a valid id (at most 4294967294)")

// NumericID reports whether s is all digits (numeric) and, if so, the id it
// names. numeric with a non-nil err means digits that are not a valid id
// (over 32 bits, or 4294967295, the reserved (gid_t)-1): an error, not a
// name. A group is a name or an id, never a name made of digits
// (validation and authz.LookupGroupSystem read it the same way).
func NumericID(s string) (id uint32, numeric bool, err error) {
	if s == "" {
		return 0, false, nil
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false, nil
		}
	}
	n, err := strconv.ParseUint(s, 10, 32)
	if err != nil || n == math.MaxUint32 {
		return 0, true, errBadID
	}
	return uint32(n), true, nil
}

func (v *validator) httpURL(p loc, raw string) bool {
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
	// url.Parse accepts any digits as a port; a delivery to port 0 or
	// 70000 would fail only when an event is sent (B-027). The port is
	// not quoted: it is part of a URL that may come from the environment.
	if port := u.Port(); port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			v.errorf(p, "port is out of range (1 - 65535)")
			return false
		}
	}
	return true
}

// headers checks the names and values of a channel's headers. Names are
// written in the file, never expanded from the environment, so they can
// be quoted; values cannot.
func (v *validator) headers(p loc, h map[string]string) {
	canonical := map[string]string{} // canonical name -> first name seen
	for _, k := range sortedKeys(h) {
		line := p.keyLine(k)
		if !headerNameRe.MatchString(k) {
			v.errorAt(line, p.path, "invalid header name %q", k)
		} else if first, dup := canonical[http.CanonicalHeaderKey(k)]; dup {
			// Both would be sent, or one would replace the other: which
			// one depends on the HTTP library, not on the file (B-027).
			v.errorAt(line, p.path, "header %q repeats %q: header names are case-insensitive; keep one", k, first)
		} else {
			canonical[http.CanonicalHeaderKey(k)] = k
		}
		value := p.key(k)
		if hasControl(h[k]) {
			v.errorf(value, "value contains a control character (only TAB is allowed)")
		}
		if redact.TooShort(h[k]) && redact.IsSensitiveKey(k) {
			v.warnf(value, "this value is shorter than %d bytes: it is too short to be masked in logs", redact.MinSecretLen)
		}
	}
}

// hasControl reports whether s has an ASCII control byte other than TAB:
// CR and LF would split the header, the others are refused by HTTP
// (RFC 9110, section 5.5) and fail at delivery (B-027).
func hasControl(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return r != '\t' && (r < 0x20 || r == 0x7f) })
}

// absPath checks a path that names a file or a directory of sentineld's
// own: absolute, clean and not "/", since sentineld would then own, lock
// or read the root directory itself (B-027).
func (v *validator) absPath(p loc, path string) bool {
	switch {
	case path == "":
		v.errorf(p, "is required")
	case strings.ContainsRune(path, 0):
		v.errorf(p, "contains a NUL byte")
	case !filepath.IsAbs(path):
		v.errorf(p, "%q must be an absolute path", path)
	case filepath.Clean(path) != path:
		v.errorf(p, "%q must be a clean path (no '..', '.', '//' or trailing '/'); use %q", path, filepath.Clean(path))
	case path == "/":
		v.errorf(p, "is the root directory; name a file or directory of its own")
	default:
		return true
	}
	return false
}

func (v *validator) durationRange(p loc, d, minD, maxD Duration) {
	if d < minD || d > maxD {
		v.errorf(p, "%s is out of range (%s - %s)", d, minD, maxD)
	}
}

func (v *validator) intRange(p loc, n, minN, maxN int) {
	if n < minN || n > maxN {
		v.errorf(p, "%d is out of range (%d - %d)", n, minN, maxN)
	}
}

func oneOf[T ~string](v *validator, p loc, got T, allowed []T) {
	if slices.Contains(allowed, got) {
		return
	}
	names := make([]string, len(allowed))
	for i, a := range allowed {
		names[i] = string(a)
	}
	v.errorf(p, "%q is not valid (allowed: %s)", string(got), strings.Join(names, ", "))
}
