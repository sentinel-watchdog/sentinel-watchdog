// Package api is version 1 of the protocol between sentinelctl and
// sentineld over the control socket (ADR-0012).
//
// A client connects, writes one request and reads one response; each is a
// JSON object on one line, ended by "\n". Every command belongs to a
// namespace ("core.status") and declares the tier it needs; the daemon
// checks the tier against the peer's kernel credentials before it runs the
// command. The client is never trusted: it cannot name its own tier.
package api

import (
	"encoding/json/jsontext"
	"regexp"
)

// Version is the protocol version of this package.
const Version = 1

// Size limits of one message, newline included.
const (
	MaxRequestBytes  = 64 << 10
	MaxResponseBytes = 16 << 20
)

// Tier is the authorization level a command needs and a peer has.
type Tier string

// Tiers, from the lowest (ADR-0012).
const (
	TierRead    Tier = "read"    // look: status, lists, redacted configuration
	TierOperate Tier = "operate" // act on services: restart, reload
	TierAdmin   Tier = "admin"   // change the firewall: root-equivalent
)

func (t Tier) rank() int {
	switch t {
	case TierRead:
		return 1
	case TierOperate:
		return 2
	case TierAdmin:
		return 3
	}
	return 0
}

// Allows reports whether a peer of tier t may run a command that needs
// need. An unknown tier, on either side, allows nothing.
func (t Tier) Allows(need Tier) bool {
	return t.rank() > 0 && need.rank() > 0 && t.rank() >= need.rank()
}

// Code classifies an error response.
type Code string

// Error codes.
const (
	CodeBadRequest         Code = "bad_request"         // not a valid request line
	CodeUnsupportedVersion Code = "unsupported_version" // another protocol version
	CodeUnknownCommand     Code = "unknown_command"
	CodePermissionDenied   Code = "permission_denied" // the peer's tier is too low
	CodeUnavailable        Code = "unavailable"       // the daemon cannot serve it now
	CodeUnsupported        Code = "unsupported"       // known, not implemented yet
	CodeInternal           Code = "internal"
)

// Error is the error part of a response.
type Error struct {
	Code    Code   `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	return string(e.Code) + ": " + e.Message
}

// Request is one call.
type Request struct {
	Version int    `json:"version"`
	Command string `json:"command"`
	// Args are the command's arguments, a JSON object or absent.
	Args jsontext.Value `json:"args,omitzero"`
}

// Response is the answer to a request: a result or an error, never both.
type Response struct {
	Version int            `json:"version"`
	Result  jsontext.Value `json:"result,omitzero"`
	Error   *Error         `json:"error,omitempty"`
}

// Command describes a command of the protocol.
type Command struct {
	Name    string
	Tier    Tier
	Summary string
}

// commandNameRe matches "<namespace>.<command>": the namespace is "core"
// or a module name.
var commandNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}\.[a-z][a-z0-9_]{0,63}$`)

// ValidCommandName reports whether name has the form of a command name.
func ValidCommandName(name string) bool { return commandNameRe.MatchString(name) }

// The core's commands. Every one is read-only (tier read); commands that
// change something arrive with the audit log (Phase 2c-3).
var (
	CoreStatus     = Command{Name: "core.status", Tier: TierRead, Summary: "daemon status: version, uptime, modules, counters"}
	CoreModules    = Command{Name: "core.modules", Tier: TierRead, Summary: "known modules: availability, switch, lifecycle state"}
	CoreConfigShow = Command{Name: "core.config_show", Tier: TierRead, Summary: "the core configuration, secrets redacted"}
	CoreEvents     = Command{Name: "core.events", Tier: TierRead, Summary: "recent events (from Phase 3a)"}
)
