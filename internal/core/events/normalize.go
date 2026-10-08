package events

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/model"
)

// Limits applied to every event at the bus boundary (ADR-0002, T-18).
// Text can come from outside the host (container labels, HTTP bodies,
// CrowdSec scenarios), so it is cleaned and shortened, never trusted.
const (
	// DefaultMaxEventBytes caps the JSON encoding of an event. Above it,
	// attributes are replaced by {"attributes_truncated": true}.
	DefaultMaxEventBytes = 16 << 10
	// minMaxEventBytes is the smallest cap a Bus accepts: it holds the
	// fields that are never shortened (identifiers at their limits, even
	// fully escaped), so fit can always succeed.
	minMaxEventBytes = 8 << 10

	maxIdentifierBytes = 256  // source, state, previous_state, correlation_id
	maxMessageBytes    = 2048 // message
	maxValueBytes      = 1024 // metadata values and attribute strings
	maxMapEntries      = 32   // metadata, attributes, nested attributes
	maxListItems       = 64   // attribute lists
)

// truncatedKey marks an event whose attributes were dropped to fit.
const truncatedKey = "attributes_truncated"

// keyRe matches metadata and attribute keys.
var keyRe = regexp.MustCompile(`^[a-z0-9_.]{1,64}$`)

// normalize validates e against the registry and returns a cleaned copy:
// strings without control characters or invalid UTF-8, shortened to their
// limits, and attributes reduced to the allowed shapes. Problems that only
// the emitting code can cause (unregistered type, wrong module, bad key or
// attribute type) are errors; problems in the content are corrected.
func normalize(e model.Event, reg *Registry, maxBytes int) (model.Event, error) {
	owner, ok := reg.lookup(e.Type)
	if !ok {
		return model.Event{}, fmt.Errorf("events: event type %q is not registered", e.Type)
	}
	var errs []error
	if e.Module != owner.module {
		errs = append(errs, fmt.Errorf("events: event type %q belongs to module %q, not %q", e.Type, owner.module, e.Module))
	}
	if e.Severity == "" {
		e.Severity = owner.severity
	} else if e.Severity.Rank() == 0 {
		errs = append(errs, fmt.Errorf("events: invalid severity %q", e.Severity))
	}
	if !typeNameRe.MatchString(string(e.SourceType)) {
		errs = append(errs, fmt.Errorf("events: invalid source_type %q", e.SourceType))
	}
	e.Source = clean(e.Source, maxIdentifierBytes)
	if e.Source == "" {
		errs = append(errs, errors.New("events: source is empty"))
	}
	e.State = clean(e.State, maxIdentifierBytes)
	e.PreviousState = clean(e.PreviousState, maxIdentifierBytes)
	e.CorrelationID = clean(e.CorrelationID, maxIdentifierBytes)
	e.Message = clean(e.Message, maxMessageBytes)

	var err error
	if e.Metadata, err = normalizeMetadata(e.Metadata); err != nil {
		errs = append(errs, err)
	}
	if e.Attributes, err = normalizeAttributes(e.Attributes, 0); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return model.Event{}, errors.Join(errs...)
	}
	return fit(e, maxBytes)
}

// fit shrinks an event whose JSON form exceeds maxBytes: first its
// attributes are replaced by {"attributes_truncated": true}, then its
// metadata is dropped, then its message is shortened. Escaping can make
// JSON up to six times longer than the text, so the field limits alone do
// not guarantee the cap.
func fit(e model.Event, maxBytes int) (model.Event, error) {
	fits := func() (bool, error) {
		data, err := json.Marshal(e)
		if err != nil {
			return false, fmt.Errorf("events: encode event: %w", err)
		}
		return len(data) <= maxBytes, nil
	}
	ok, err := fits()
	if ok || err != nil {
		return e, err
	}
	e.Attributes = map[string]any{truncatedKey: true}
	if ok, err = fits(); ok || err != nil {
		return e, err
	}
	e.Metadata = nil
	for {
		if ok, err = fits(); ok || err != nil {
			return e, err
		}
		if e.Message == "" {
			return model.Event{}, fmt.Errorf("events: event does not fit in %d bytes", maxBytes)
		}
		e.Message = clean(e.Message, len(e.Message)/2)
	}
}

func normalizeMetadata(m map[string]string) (map[string]string, error) {
	if len(m) == 0 {
		return nil, nil
	}
	if len(m) > maxMapEntries {
		return nil, fmt.Errorf("events: metadata has %d entries (at most %d)", len(m), maxMapEntries)
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		if !keyRe.MatchString(k) {
			return nil, fmt.Errorf("events: invalid metadata key %q", k)
		}
		out[k] = clean(v, maxValueBytes)
	}
	return out, nil
}

// normalizeAttributes accepts JSON scalars, []string and, at depth 0, one
// nested map of the same. It returns a new map: the emitter's map is never
// shared with consumers.
func normalizeAttributes(m map[string]any, depth int) (map[string]any, error) {
	if len(m) == 0 {
		return nil, nil
	}
	if len(m) > maxMapEntries {
		return nil, fmt.Errorf("events: attributes have %d entries (at most %d)", len(m), maxMapEntries)
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		if !keyRe.MatchString(k) || k == truncatedKey {
			return nil, fmt.Errorf("events: invalid attribute key %q", k)
		}
		nv, err := normalizeValue(v, depth)
		if err != nil {
			return nil, fmt.Errorf("events: attribute %q: %w", k, err)
		}
		out[k] = nv
	}
	return out, nil
}

func normalizeValue(v any, depth int) (any, error) {
	switch x := v.(type) {
	case nil, bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return x, nil
	case float32:
		return finite(float64(x))
	case float64:
		return finite(x)
	case string:
		return clean(x, maxValueBytes), nil
	case []string:
		if len(x) > maxListItems {
			return nil, fmt.Errorf("list has %d items (at most %d)", len(x), maxListItems)
		}
		out := make([]string, len(x))
		for i, s := range x {
			out[i] = clean(s, maxValueBytes)
		}
		return out, nil
	case map[string]any:
		if depth > 0 {
			return nil, errors.New("only one level of nesting is allowed")
		}
		return normalizeAttributes(x, depth+1)
	default:
		return nil, fmt.Errorf("unsupported type %T", v)
	}
}

// finite rejects NaN and infinities, which JSON cannot encode.
func finite(f float64) (any, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil, errors.New("NaN or infinite number")
	}
	return f, nil
}

// cleanWork bounds the input clean looks at, as a multiple of the output
// limit. Text after that point is dropped even if the part examined was
// mostly control characters that clean removes: a field made only of such
// characters ends up empty (an empty source makes the event invalid).
const cleanWork = 4

// clean returns s as valid UTF-8 without control characters, cut to at
// most maxBytes on a character boundary. It looks at no more than
// cleanWork*maxBytes bytes of s, and a shortened result is a copy, so a
// large input is neither processed whole nor kept in memory.
func clean(s string, maxBytes int) string {
	long := len(s) > maxBytes
	if len(s) > cleanWork*maxBytes {
		cut := cleanWork * maxBytes
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut-- // never split a character
		}
		s = s[:cut]
	}
	s = strings.ToValidUTF8(s, "\uFFFD")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	if len(s) > maxBytes {
		cut := maxBytes
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut]
	}
	if long {
		return strings.Clone(s) // do not keep the input's memory
	}
	return s
}
