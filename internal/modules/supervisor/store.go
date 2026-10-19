package supervisor

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"time"
)

// stateVersion is the schema of the supervisor's state file. A change of
// it needs a migration (D-015: another version is refused, never
// overwritten).
const stateVersion = 1

// fileState is the supervisor's state file. It never holds a URL, a
// header or a body (ADR-0011): only what keeps events from repeating
// across restarts.
type fileState struct {
	Services map[string]serviceState `json:"services"`
}

// serviceState is what is kept of one service.
type serviceState struct {
	// Digest names the definition the state belongs to (digestOf): a
	// changed definition starts again from unknown.
	Digest               string    `json:"digest"`
	Health               Health    `json:"health"`
	Since                time.Time `json:"since,omitzero"`
	ConsecutiveFailures  int       `json:"consecutive_failures"`
	ConsecutiveSuccesses int       `json:"consecutive_successes"`
	LastCheck            time.Time `json:"last_check,omitzero"`
	LastFailure          Failure   `json:"last_failure,omitempty"`
	// IncidentID is the service_failed event's id while the service is
	// failed: its service_recovered carries it as correlation id.
	IncidentID string `json:"incident_id,omitempty"`
	Checks     uint64 `json:"checks"`
	Failures   uint64 `json:"failures"`
}

var (
	digestRe   = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	incidentRe = regexp.MustCompile(`^[0-9a-f]{32}$`) // model.NewEventID
)

// Validate checks what a state file from a previous run claims; a file
// that fails it is corrupt and moved aside (state.Load).
func (f *fileState) Validate() error {
	if len(f.Services) > maxServices {
		return fmt.Errorf("more than %d services", maxServices)
	}
	for name, s := range f.Services {
		if !nameRe.MatchString(name) {
			return errors.New("invalid service name")
		}
		switch {
		case !digestRe.MatchString(s.Digest):
			return fmt.Errorf("service %s: invalid digest", name)
		case s.Health != HealthUnknown && s.Health != HealthHealthy && s.Health != HealthFailed:
			return fmt.Errorf("service %s: invalid health", name)
		case !s.LastFailure.known():
			return fmt.Errorf("service %s: invalid failure code", name)
		case s.ConsecutiveFailures < 0 || s.ConsecutiveFailures > maxStreak ||
			s.ConsecutiveSuccesses < 0 || s.ConsecutiveSuccesses > maxStreak:
			return fmt.Errorf("service %s: invalid counters", name)
		case (s.Health == HealthFailed) != incidentRe.MatchString(s.IncidentID) ||
			(s.Health != HealthFailed && s.IncidentID != ""):
			return fmt.Errorf("service %s: an incident belongs to a failed service only", name)
		}
	}
	return nil
}

// digestOf names what a service's state depends on: its type, target,
// timeout, failure policy and, for http, the whole request and what it
// expects (D-082: a changed check starts again from unknown). The check
// interval and the routes are left out, so changing them keeps an open
// incident (D-080 (3)). Secrets in the URL, headers or body are in the
// hash, so a low-entropy one could be guessed from the state file; that
// file is root-only (0600) and the digest is never shown.
func digestOf(s Service) string {
	h := sha256.New()
	// Each field as a netstring (length:bytes), under a version prefix:
	// no value can be mistaken for a separator.
	field := func(v any) {
		str := fmt.Sprint(v)
		fmt.Fprintf(h, "%d:%s,", len(str), str)
	}
	field("v2")
	field(s.Type)
	field(s.URL.String())
	field(int64(s.Timeout))
	field(s.Policy.Failures)
	field(s.Policy.Successes)
	hs := s.HTTP
	field(hs.Method)
	names := slices.Sorted(maps.Keys(hs.Headers))
	field(len(names))
	for _, name := range names {
		field(name)
		field(hs.Headers[name])
	}
	field(hs.Body)
	field(len(hs.SuccessCodes))
	for _, c := range hs.SuccessCodes {
		field(c)
	}
	field(hs.BodyContains)
	field(hs.FollowRedirects)
	field(hs.Address)
	field(hs.TLS.CAFile)
	field(hs.TLS.ServerName)
	field(hs.TLS.InsecureSkipVerify)
	field(hs.TLSMinVersion)
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// reconcile returns the state each configured service starts from: what
// was kept for its definition, or a fresh unknown state. changed reports
// whether the file should be written again (an entry was reset, added or
// removed).
func reconcile(kept *fileState, services []*service) (changed bool) {
	var old map[string]serviceState
	if kept != nil {
		old = kept.Services
	}
	for _, s := range services {
		st, ok := old[s.def.Name]
		if ok && st.Digest == s.digest {
			s.st = st
			continue
		}
		s.st = serviceState{Digest: s.digest, Health: HealthUnknown}
		changed = true
	}
	return changed || len(old) != len(services)
}
