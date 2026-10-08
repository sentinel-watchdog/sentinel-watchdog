// Package redact removes secrets from values before they reach logs, the
// state file, CLI output or notification payloads.
package redact

import (
	"net/url"
	"strings"
)

// Placeholder replaces every redacted value.
const Placeholder = "[REDACTED]"

// sensitiveKeyParts are matched case-insensitively against key names after
// removing '-' and '_' (so "API-Key", "api_key" and "apikey" all match).
var sensitiveKeyParts = []string{
	"password", "passwd", "secret", "token", "authorization",
	"apikey", "accesskey", "privatekey", "credential", "cookie",
	"signature", "session",
}

// IsSensitiveKey reports whether a header, environment variable, log
// attribute or query parameter name is likely to carry a secret.
func IsSensitiveKey(key string) bool {
	k := strings.ToLower(strings.NewReplacer("-", "", "_", "").Replace(key))
	for _, part := range sensitiveKeyParts {
		if strings.Contains(k, part) {
			return true
		}
	}
	return false
}

// safeHeaders never carry secrets and stay readable in redacted output.
var safeHeaders = map[string]bool{
	"accept":          true,
	"accept-encoding": true,
	"accept-language": true,
	"cache-control":   true,
	"content-type":    true,
	"user-agent":      true,
}

// Headers returns a copy of h where every value is redacted except for a
// small allow-list of well-known non-secret headers. Unknown custom headers
// (X-Api-Key, X-Auth, ...) are treated as secrets by default.
func Headers(h map[string]string) map[string]string {
	if h == nil {
		return nil
	}
	out := make(map[string]string, len(h))
	for k, v := range h {
		if safeHeaders[strings.ToLower(k)] {
			out[k] = v
			continue
		}
		out[k] = Placeholder
	}
	return out
}

// Environment returns a copy of env with values of sensitive keys redacted.
func Environment(env map[string]string) map[string]string {
	if env == nil {
		return nil
	}
	out := make(map[string]string, len(env))
	for k, v := range env {
		if IsSensitiveKey(k) {
			v = Placeholder
		}
		out[k] = v
	}
	return out
}

// URL removes the password from user info (or the user name when it comes
// alone, as tokens often do) and redacts the values of all query
// parameters. Path segments are kept: use WebhookURL for endpoints
// that embed tokens in the path. Unparseable input is fully redacted.
func URL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return Placeholder
	}
	redactURL(u)
	// url.URL escapes the brackets of the placeholder; keep it readable.
	return strings.ReplaceAll(u.String(), url.QueryEscape(Placeholder), Placeholder)
}

// WebhookURL keeps only the scheme and host of raw. Webhook services
// (Slack, Teams, many others) put credentials in the path.
func WebhookURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return Placeholder
	}
	out := u.Scheme + "://" + u.Host
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
		out += "/" + Placeholder
	}
	return out
}

func redactURL(u *url.URL) {
	if u.User != nil {
		if _, ok := u.User.Password(); ok {
			u.User = url.UserPassword(u.User.Username(), Placeholder)
		} else {
			u.User = url.User(Placeholder) // https://<token>@host
		}
	}
	if u.RawQuery == "" {
		return
	}
	q := u.Query()
	for k := range q {
		q[k] = []string{Placeholder}
	}
	u.RawQuery = q.Encode()
}

// Text replaces every occurrence of the given secret values inside s. Empty
// and very short secrets (< 4 bytes) are ignored to avoid shredding
// unrelated text.
func Text(s string, secrets ...string) string {
	for _, sec := range secrets {
		if len(sec) < 4 {
			continue
		}
		s = strings.ReplaceAll(s, sec, Placeholder)
	}
	return s
}
