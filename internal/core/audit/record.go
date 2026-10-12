package audit

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"regexp"
	"time"
	"unicode/utf8"
)

// Bounds of one record.
const (
	// MaxLineBytes bounds one record, newline included; a longer line is
	// refused by the writer and treated as tampering by the reader.
	MaxLineBytes    = 8 << 10
	maxTargetBytes  = 256
	maxDetailKeys   = 16
	maxDetailValue  = 512
	hashPrefix      = "sha256:"
	hashHexLen      = sha256.Size * 2
	hashMember      = `,"hash":"`
	hashSuffixBytes = len(hashMember) + len(hashPrefix) + hashHexLen + len(`"}`)
)

// genesisHash is the prev_hash of the first record of a chain.
var genesisHash = hashPrefix + string(bytes.Repeat([]byte("0"), hashHexLen))

// Result says what a record is about.
type Result string

// Results (ADR-0011).
const (
	ResultIntent Result = "intent" // allowed; the action is about to run
	ResultOK     Result = "ok"     // the action ran and succeeded
	ResultFailed Result = "failed" // the action ran, or was refused by the daemon, and failed
	ResultDenied Result = "denied" // the peer's tier was too low; nothing ran
)

// ActorKind says who asked.
type ActorKind string

// Actor kinds.
const (
	ActorSocket ActorKind = "socket" // a control-socket peer, from its kernel credentials
	ActorSignal ActorKind = "signal" // a signal: the sender is not known
	ActorDaemon ActorKind = "daemon" // sentineld itself
)

// Actor identifies who asked. UID and GID are absent when unknown (a
// signal); PID is 0 when unknown (a signal, or macOS peers).
type Actor struct {
	Kind ActorKind `json:"kind"`
	UID  *uint32   `json:"uid,omitzero"`
	GID  *uint32   `json:"gid,omitzero"`
	PID  int32     `json:"pid,omitzero"`
}

// Entry is what a caller records; the log adds the sequence number, the
// time and the chain.
type Entry struct {
	Actor Actor
	// Action is a command or operation name, such as "core.reload"; never
	// text chosen by a client.
	Action string
	// Target names what the action applies to, if anything.
	Target string
	Result Result
	// CorrelationID links the records of one request or operation.
	CorrelationID string
	// Detail holds a few short, already redacted values: never secrets,
	// never raw client input or error text.
	Detail map[string]string
}

// Record is one line of the audit log. Hash is computed over the line's
// bytes without the hash member and is written last.
type Record struct {
	Seq           uint64            `json:"seq"`
	Time          time.Time         `json:"time"`
	Actor         Actor             `json:"actor"`
	Action        string            `json:"action"`
	Target        string            `json:"target,omitzero"`
	Result        Result            `json:"result"`
	CorrelationID string            `json:"correlation_id,omitzero"`
	Detail        map[string]string `json:"detail,omitzero"`
	PrevHash      string            `json:"prev_hash"`
	Hash          string            `json:"-"`
}

var (
	actionRe      = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}\.[a-z][a-z0-9_]{0,63}$`)
	detailKeyRe   = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	correlationRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

// validate checks the bounds of an entry, so that a record always fits a
// line and never carries arbitrary text.
func (e Entry) validate() error {
	switch e.Actor.Kind {
	case ActorSocket, ActorSignal, ActorDaemon:
	default:
		return fmt.Errorf("audit: invalid actor kind %q", e.Actor.Kind)
	}
	if !actionRe.MatchString(e.Action) {
		return fmt.Errorf("audit: invalid action %q", e.Action)
	}
	switch e.Result {
	case ResultIntent, ResultOK, ResultFailed, ResultDenied:
	default:
		return fmt.Errorf("audit: invalid result %q", e.Result)
	}
	if len(e.Target) > maxTargetBytes || !utf8.ValidString(e.Target) {
		return errors.New("audit: target is too long or not UTF-8")
	}
	if e.CorrelationID != "" && !correlationRe.MatchString(e.CorrelationID) {
		return errors.New("audit: invalid correlation id")
	}
	if len(e.Detail) > maxDetailKeys {
		return fmt.Errorf("audit: more than %d detail keys", maxDetailKeys)
	}
	for k, v := range e.Detail {
		if !detailKeyRe.MatchString(k) {
			return fmt.Errorf("audit: invalid detail key %q", k)
		}
		if len(v) > maxDetailValue || !utf8.ValidString(v) {
			return fmt.Errorf("audit: detail %q is too long or not UTF-8", k)
		}
	}
	return nil
}

func validHash(h string) bool {
	if len(h) != len(hashPrefix)+hashHexLen || h[:len(hashPrefix)] != hashPrefix {
		return false
	}
	for _, c := range []byte(h[len(hashPrefix):]) {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// encode returns the line of r, newline included, and its hash. The hash
// covers the exact bytes written before the hash member, so verifying a
// line never depends on how a later Go release would encode it.
func encode(r Record) ([]byte, string, error) {
	body, err := json.Marshal(r, json.Deterministic(true))
	if err != nil {
		return nil, "", fmt.Errorf("audit: encode record: %w", err)
	}
	sum := sha256.Sum256(body)
	hash := hashPrefix + hex.EncodeToString(sum[:])
	line := make([]byte, 0, len(body)+hashSuffixBytes)
	line = append(line, body[:len(body)-1]...) // without the closing brace
	line = append(line, hashMember...)
	line = append(line, hash...)
	line = append(line, "\"}\n"...)
	if len(line) > MaxLineBytes {
		return nil, "", fmt.Errorf("audit: record has %d bytes, more than %d", len(line), MaxLineBytes)
	}
	return line, hash, nil
}

// parse checks one line, without its newline, and returns its record:
// the hash member must close the line, the hash must match the bytes
// before it, and those bytes must be exactly one record with known
// members, no duplicate, valid UTF-8 and valid fields. The line comes from
// a file that root, or an attacker who became root, may have edited.
func parse(line []byte) (Record, error) {
	if len(line) >= MaxLineBytes {
		return Record{}, errors.New("line too long")
	}
	if len(line) < hashSuffixBytes+2 {
		return Record{}, errors.New("line too short")
	}
	cut := len(line) - hashSuffixBytes
	suffix := line[cut:]
	hash := string(suffix[len(hashMember) : len(suffix)-2])
	if !bytes.HasPrefix(suffix, []byte(hashMember)) || !bytes.HasSuffix(suffix, []byte(`"}`)) || !validHash(hash) {
		return Record{}, errors.New("the line does not end with the hash member")
	}
	body := make([]byte, 0, cut+1)
	body = append(append(body, line[:cut]...), '}')
	sum := sha256.Sum256(body)
	if hash != hashPrefix+hex.EncodeToString(sum[:]) {
		return Record{}, errors.New("hash does not match the record")
	}
	var r Record
	if err := json.Unmarshal(body, &r, json.RejectUnknownMembers(true)); err != nil {
		return Record{}, fmt.Errorf("invalid record: %w", err)
	}
	entry := Entry{Actor: r.Actor, Action: r.Action, Target: r.Target, Result: r.Result,
		CorrelationID: r.CorrelationID, Detail: r.Detail}
	if err := entry.validate(); err != nil {
		return Record{}, err
	}
	switch {
	case r.Seq == 0:
		return Record{}, errors.New("invalid record: seq 0")
	case r.Time.IsZero():
		return Record{}, errors.New("invalid record: no time")
	case !validHash(r.PrevHash):
		return Record{}, errors.New("invalid record: invalid prev_hash")
	}
	r.Hash = hash
	return r, nil
}
