package supervisor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"maps"
	"regexp"
	"slices"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/state"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/platform/executor"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/api"
)

// stateVersion is the schema of the supervisor's state file. A change of
// it needs a migration (D-015: another version is refused, never
// overwritten). Version 2 (D-086) added the process groups and the
// process counters, with optional fields added by the later 3b
// sub-phases; v0.2.0 is the first release that writes it.
const stateVersion = 2

// migrations bring older state files to stateVersion. Version 2 only
// adds optional fields: version 1 data is valid version 2 data.
var migrations = map[int]state.Migration{
	1: func(data json.RawMessage) (json.RawMessage, error) { return data, nil },
}

// fileState is the supervisor's state file. It never holds a URL, a
// header, a body, a command, its arguments or its environment
// (ADR-0011): only what keeps events from repeating across restarts, and
// what a later run needs to kill a process group left behind.
type fileState struct {
	Services map[string]serviceState `json:"services"`
	// Groups are the process groups launched and not yet seen to end, by
	// service name. They are kept apart from Services, whose entries
	// follow the configuration: a removed or changed service keeps its
	// record until the group is known to be gone (D-084 (2)).
	Groups map[string]groupRecord `json:"groups,omitempty"`
}

// groupRecord is executor.Group as stored.
type groupRecord struct {
	Pgid  int    `json:"pgid"`
	Sid   int    `json:"sid"`
	Start uint64 `json:"start"`
	Boot  string `json:"boot"`
}

func recordOf(g executor.Group) groupRecord {
	return groupRecord{Pgid: g.Pgid, Sid: g.Sid, Start: g.Start, Boot: g.Boot}
}

func (r groupRecord) group() executor.Group {
	return executor.Group{Pgid: r.Pgid, Sid: r.Sid, Start: r.Start, Boot: r.Boot}
}

// exitRecord is how a process last ended.
type exitRecord struct {
	Code   int       `json:"code"`
	Signal string    `json:"signal,omitempty"`
	At     time.Time `json:"at"`
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
	// Process services: how many times the process was started, and how
	// it last ended.
	Starts   uint64      `json:"starts,omitempty"`
	LastExit *exitRecord `json:"last_exit,omitempty"`
	// Recovery of process services (D-014): the attempts in the window
	// and when the budget was spent (budget); how many automatic
	// restarts were launched.
	Attempts    []time.Time `json:"attempts,omitempty"`
	ExhaustedAt time.Time   `json:"exhausted_at,omitzero"`
	Restarts    uint64      `json:"restarts,omitempty"`
	// What operators asked (D-020, D-089): the service is not run or
	// checked (supervisor disable), or its process is held stopped
	// (supervisor stop). They follow the name, not the definition.
	Disabled bool `json:"disabled,omitempty"`
	Stopped  bool `json:"stopped,omitempty"`
}

func (st serviceState) budget() budget {
	return budget{Attempts: st.Attempts, ExhaustedAt: st.ExhaustedAt}
}

// setBudget stores b, whose slice must be its own: trim and reserve
// return fresh ones, and save clones it (C6).
func (st *serviceState) setBudget(b budget) {
	st.Attempts, st.ExhaustedAt = b.Attempts, b.ExhaustedAt
}

var (
	digestRe   = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	incidentRe = regexp.MustCompile(`^[0-9a-f]{32}$`) // model.NewEventID
	bootRe     = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	// signalRe is a signal name as the executor reports it.
	signalRe = regexp.MustCompile(`^(SIG[A-Z0-9]{2,8}|signal [1-9][0-9]?)$`)
)

// Bounds of a group record: PID_MAX_LIMIT on 64-bit Linux, and pid 1's
// group is never one sentineld started.
const (
	minPgid = 2
	maxPgid = 4 << 20
)

// Validate checks what a state file from a previous run claims; a file
// that fails it is corrupt and moved aside (state.Load).
func (f *fileState) Validate() error {
	if len(f.Services) > maxServices {
		return fmt.Errorf("more than %d services", maxServices)
	}
	for name, s := range f.Services {
		if !api.ValidName(name) {
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
		case s.LastExit != nil && (s.LastExit.Code < -1 || s.LastExit.Code > 255 ||
			(s.LastExit.Signal != "" && !signalRe.MatchString(s.LastExit.Signal))):
			return fmt.Errorf("service %s: invalid last exit", name)
		case !validAttempts(s.Attempts):
			return fmt.Errorf("service %s: invalid recovery attempts", name)
		case !s.ExhaustedAt.IsZero() && s.Health != HealthFailed:
			return fmt.Errorf("service %s: only a failed service is exhausted", name)
		case s.Restarts > s.Starts:
			return fmt.Errorf("service %s: more restarts than starts", name)
		}
	}
	if len(f.Groups) > maxGroups {
		return fmt.Errorf("more than %d process groups", maxGroups)
	}
	for name, g := range f.Groups {
		switch {
		case !api.ValidName(name):
			return errors.New("invalid service name of a process group")
		case g.Pgid < minPgid || g.Pgid > maxPgid || g.Sid < 1 || g.Sid > maxPgid || g.Start == 0 || !bootRe.MatchString(g.Boot):
			return fmt.Errorf("service %s: invalid process group", name)
		}
	}
	return nil
}

// validAttempts reports whether attempts are a stored budget: at most
// maxMaxAttempts, none zero, in non-decreasing order (equal times are
// valid: a clamped clock and a recovered state produce them).
func validAttempts(attempts []time.Time) bool {
	if len(attempts) > maxMaxAttempts {
		return false
	}
	for i, t := range attempts {
		if t.IsZero() || (i > 0 && t.Before(attempts[i-1])) {
			return false
		}
	}
	return true
}

// maxGroups bounds the group records: one per configured service, and as
// many of services removed while their group could not be cleaned up.
const maxGroups = 2 * maxServices

// digestOf names what a service's state depends on, by type: a changed
// definition starts again from unknown (D-082). The routes are left out,
// so changing them keeps an open incident (D-080 (3)). Secrets in the
// definition (URL, headers, body, environment) are in the hash, so a
// low-entropy one could be guessed from the state file; that file is
// root-only (0600) and the digest is never shown.
func digestOf(s Service) string {
	if s.Type == typeProcess {
		return digestProcess(s)
	}
	return digestHTTP(s)
}

// digester writes each field as a netstring (length:bytes), so that no
// value can be mistaken for a separator.
type digester struct{ h hash.Hash }

func newDigester() digester { return digester{sha256.New()} }

func (d digester) field(v any) {
	str := fmt.Sprint(v)
	fmt.Fprintf(d.h, "%d:%s,", len(str), str)
}

func (d digester) sum() string { return "sha256:" + hex.EncodeToString(d.h.Sum(nil)) }

// digestProcess covers how the process runs: program, arguments, identity
// (as configured and as resolved), directory, environment, output and the
// stop and startup settings.
func digestProcess(s Service) string {
	d := newDigester()
	d.field("process-v1")
	pr := s.Process
	d.field(pr.Command)
	d.field(pr.Script)
	d.field(len(pr.Args))
	for _, a := range pr.Args {
		d.field(a)
	}
	d.field(pr.User)
	d.field(pr.Group)
	if pr.Cred != nil {
		d.field(fmt.Sprintf("%d:%d:%v", pr.Cred.UID, pr.Cred.GID, pr.Cred.Groups))
	} else {
		d.field("-")
	}
	d.field(pr.WorkingDir)
	d.field(len(pr.Env))
	for _, kv := range pr.Env {
		d.field(kv)
	}
	d.field(pr.Output.Kind)
	d.field(pr.Output.File)
	d.field(uint32(pr.Output.Mode))
	d.field(int(pr.StopSignal))
	d.field(int64(pr.StopTimeout))
	d.field(int64(pr.StartupGrace))
	return d.sum()
}

// digestHTTP covers an http service's target, timeout, failure policy and
// the whole request and what it expects; the check interval is left out.
// Its fields and their order are those of v0.1.0, so an upgrade keeps the
// state of every http service.
func digestHTTP(s Service) string {
	d := newDigester()
	field := d.field
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
	return d.sum()
}

// reconcile returns the state each configured service starts from: what
// was kept for its definition, or a fresh unknown state that keeps what
// operators asked of the name (review 3b-5 C5: a changed command does not
// start a service an operator stopped). changed reports whether the file
// should be written again (an entry was reset, added or removed). The
// group records are not touched: they outlive the definitions.
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
		s.st = serviceState{Digest: s.digest, Health: HealthUnknown, Disabled: st.Disabled,
			Stopped: st.Stopped && s.def.Type == typeProcess}
		changed = true
	}
	return changed || len(old) != len(services)
}
