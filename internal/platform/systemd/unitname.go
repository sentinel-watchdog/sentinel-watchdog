package systemd

import (
	"errors"
	"fmt"
	"strings"
)

// maxUnitName is systemd's UNIT_NAME_MAX less its terminating NUL.
const maxUnitName = 255

// TypeService is the type of a service unit, the only one the supervisor
// observes in Phase 3c.
const TypeService = "service"

// unitTypes are the suffixes systemd knows (unit-name.c, v239 and later).
var unitTypes = map[string]bool{
	TypeService: true, "socket": true, "target": true, "device": true, "mount": true,
	"automount": true, "swap": true, "timer": true, "path": true, "slice": true, "scope": true,
}

// UnitName is a validated unit name: Prefix[@Instance].Type. It is the
// one place that reads a name's suffix as its type, so a caller that
// accepts only some types (the supervisor: services) decides on Type, and
// a later caller (timers) needs no other parser.
type UnitName struct {
	Prefix   string
	Instance string // empty for a plain unit
	Type     string // the suffix: service, timer, …
}

// String returns the name as systemd writes it.
func (n UnitName) String() string {
	if n.Instance == "" {
		return n.Prefix + "." + n.Type
	}
	return n.Prefix + "@" + n.Instance + "." + n.Type
}

// ParseUnitName validates s with systemd's own grammar (unit_name_is_valid
// for plain and instance names): at most 255 bytes of [A-Za-z0-9:_.\-\\]
// and '@', a known suffix after the last '.', a non-empty prefix before the
// first '@' and, after it, a non-empty instance that may hold further '@'.
// A template ("foo@.service") is refused: it names no unit to observe.
// The error never quotes s, which may come from the environment.
func ParseUnitName(s string) (UnitName, error) {
	if s == "" {
		return UnitName{}, errors.New("unit name is empty")
	}
	if len(s) > maxUnitName {
		return UnitName{}, fmt.Errorf("unit name is longer than %d bytes", maxUnitName)
	}
	dot := strings.LastIndexByte(s, '.')
	if dot <= 0 {
		return UnitName{}, errors.New("unit name has no type suffix (such as .service)")
	}
	typ, base := s[dot+1:], s[:dot]
	if !unitTypes[typ] {
		return UnitName{}, errors.New("unit name has an unknown type suffix")
	}
	prefix, instance, templated := strings.Cut(base, "@")
	switch {
	case prefix == "":
		return UnitName{}, errors.New("unit name has an empty prefix")
	case !validChars(prefix, false):
		return UnitName{}, errors.New("unit name has a character systemd does not allow")
	case templated && instance == "":
		return UnitName{}, errors.New("unit name is a template (name@.type): name an instance")
	case !validChars(instance, true):
		return UnitName{}, errors.New("unit name has a character systemd does not allow")
	}
	return UnitName{Prefix: prefix, Instance: instance, Type: typ}, nil
}

// validChars reports whether s holds only the characters of systemd's
// VALID_CHARS, plus '@' when at is set (VALID_CHARS_WITH_AT, instances).
func validChars(s string, at bool) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == ':', c == '-', c == '_', c == '.', c == '\\':
		case c == '@' && at:
		default:
			return false
		}
	}
	return true
}
