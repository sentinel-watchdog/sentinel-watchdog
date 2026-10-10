package state

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/clock"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/fstrust"
)

const (
	fileName = "state.json"
	// maxFileBytes bounds a state file; a larger one is treated as corrupt.
	maxFileBytes = 16 << 20
)

var (
	// ErrNewerSchema reports a file written by a newer Sentinel. It is
	// left untouched, so a downgrade cannot destroy it.
	ErrNewerSchema = errors.New("state file was written by a newer version of sentinel")
	// ErrOlderSchema reports a file with an older schema version. No
	// module has changed its schema yet, so there is no migration; the
	// file is left untouched.
	ErrOlderSchema = errors.New("state file has an older schema version and there is no migration")
)

// Store reads and atomically writes one module's state file. It is safe
// for concurrent use.
type Store struct {
	root    *os.Root // the module's directory
	display string   // path used in errors
	module  string
	version int
	euid    int
	clock   clock.Clock
	mu      sync.Mutex // serialises loads and saves
}

// Report describes what Load found.
type Report struct {
	// FirstRun: there was no state file.
	FirstRun bool
	// Recovered: the file was corrupt and has been moved to QuarantinedTo;
	// Reason says why.
	Recovered     bool
	QuarantinedTo string
	Reason        string
}

// envelope is the JSON form of a state file.
type envelope struct {
	SchemaVersion *int            `json:"schema_version"`
	UpdatedAt     time.Time       `json:"updated_at"`
	Data          json.RawMessage `json:"data"`
}

// validator is implemented by state types that check their own
// invariants after decoding.
type validator interface {
	Validate() error
}

// Load reads the module's state into a new T:
//
//   - no file: the zero T, FirstRun;
//   - corrupt file (unreadable JSON, missing or invalid schema_version,
//     data that does not decode into T or fails T's Validate method,
//     larger than 16 MiB): the file is moved aside as
//     state.json.corrupt-<UTC time>, and Load returns the zero T and
//     Recovered;
//   - newer or older schema_version: ErrNewerSchema or ErrOlderSchema,
//     file untouched;
//   - a file owned by another user or writable by group or others, and
//     I/O errors: an error, file untouched.
func Load[T any](s *Store) (T, Report, error) {
	// Held from reading to quarantine: a Save in between would otherwise
	// be moved aside with the corrupt file it replaced.
	s.mu.Lock()
	defer s.mu.Unlock()
	var zero T
	data, err := s.read()
	if errors.Is(err, os.ErrNotExist) {
		return zero, Report{FirstRun: true}, nil
	}
	if err != nil && !errors.Is(err, errTooLarge) {
		return zero, Report{}, err
	}
	reason := ""
	var value T
	if err != nil {
		reason = err.Error()
	} else if reason, err = s.decode(data, &value); err != nil {
		return zero, Report{}, err
	}
	if reason == "" {
		return value, Report{}, nil
	}
	dest, err := s.quarantine()
	if err != nil {
		return zero, Report{}, fmt.Errorf("state: %s is corrupt (%s) and could not be moved aside: %w", s.display, reason, err)
	}
	return zero, Report{Recovered: true, QuarantinedTo: s.display + ".corrupt-" + dest, Reason: reason}, nil
}

var errTooLarge = fmt.Errorf("file exceeds %d bytes", maxFileBytes)

// read returns the file's bytes after checking the opened file.
func (s *Store) read() ([]byte, error) {
	f, err := s.root.OpenFile(fileName, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("state: %s: %w", s.display, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("state: %s is not a regular file", s.display)
	}
	if err := fstrust.CheckOwnership(info, s.euid); err != nil {
		return nil, fmt.Errorf("state: %s %w", s.display, err)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("state: read %s: %w", s.display, err)
	}
	if len(data) > maxFileBytes {
		return nil, errTooLarge
	}
	return data, nil
}

// decode fills value from data. It returns a corruption reason, or an
// error that must leave the file untouched (schema version mismatch).
func (s *Store) decode(data []byte, value any) (string, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return "file is empty", nil
	}
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return "invalid JSON: " + err.Error(), nil
	}
	switch {
	case env.SchemaVersion == nil:
		return "schema_version is missing", nil
	case *env.SchemaVersion < 1:
		return fmt.Sprintf("invalid schema_version %d", *env.SchemaVersion), nil
	}
	if err := s.checkVersion(*env.SchemaVersion); err != nil {
		return "", err
	}
	if len(env.Data) == 0 {
		return "data is missing", nil
	}
	if err := json.Unmarshal(env.Data, value); err != nil {
		return "invalid data: " + err.Error(), nil
	}
	if v, ok := value.(validator); ok {
		if err := v.Validate(); err != nil {
			return "invalid data: " + err.Error(), nil
		}
	}
	return "", nil
}

// quarantine renames the state file aside and returns the new name's
// suffix (the UTC time). The caller holds s.mu.
func (s *Store) quarantine() (string, error) {
	stamp := s.clock.Now().UTC().Format("20060102T150405.000000000Z")
	if err := s.root.Rename(fileName, fileName+".corrupt-"+stamp); err != nil {
		return "", err
	}
	return stamp, nil
}

// Save writes value as the module's state: temporary file, fsync, rename
// over state.json, fsync of the directory. The file mode is 0600. value
// must not hold secrets (ADR-0011). A file with another schema version is
// never replaced: Save returns ErrNewerSchema or ErrOlderSchema, as Load
// does, even if the caller went on after Load's error (D-015).
func (s *Store) Save(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("state: encode %s: %w", s.display, err)
	}
	version := s.version
	doc, err := json.MarshalIndent(envelope{SchemaVersion: &version, UpdatedAt: s.clock.Now().UTC(), Data: data}, "", "  ")
	if err != nil {
		return fmt.Errorf("state: encode %s: %w", s.display, err)
	}
	doc = append(doc, '\n')
	if len(doc) > maxFileBytes {
		// Load would treat it as corrupt: keep the current file instead.
		return fmt.Errorf("state: %s: encoded state has %d bytes, more than %d", s.display, len(doc), maxFileBytes)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkReplaceable(); err != nil {
		return err
	}
	if err := s.writeAtomic(doc); err != nil {
		return fmt.Errorf("state: write %s: %w", s.display, err)
	}
	return nil
}

// checkReplaceable reports whether Save may replace the current file: it
// may if there is none, if it is corrupt (unreadable JSON, invalid or
// missing schema_version, too large), or if it has this schema version.
// The caller holds s.mu.
func (s *Store) checkReplaceable() error {
	data, err := s.read()
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, errTooLarge) {
		return nil
	}
	if err != nil {
		return err
	}
	var env struct {
		SchemaVersion *int `json:"schema_version"`
	}
	if json.Unmarshal(data, &env) != nil || env.SchemaVersion == nil || *env.SchemaVersion < 1 {
		return nil
	}
	return s.checkVersion(*env.SchemaVersion)
}

// checkVersion returns ErrNewerSchema or ErrOlderSchema when a file's
// schema version is not the one this binary uses.
func (s *Store) checkVersion(v int) error {
	switch {
	case v > s.version:
		return fmt.Errorf("state: %s: %w (file %d, supported %d)", s.display, ErrNewerSchema, v, s.version)
	case v < s.version:
		return fmt.Errorf("state: %s: %w (file %d, current %d)", s.display, ErrOlderSchema, v, s.version)
	}
	return nil
}

func (s *Store) writeAtomic(data []byte) (err error) {
	tmp := "." + fileName + "." + rand.Text() + ".tmp"
	f, err := s.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = f.Close()          // already failing; the first error is reported
			_ = s.root.Remove(tmp) // best effort: a leftover .tmp is harmless
		}
	}()
	// The mode passed to OpenFile is reduced by the umask: set it exactly.
	if err = f.Chmod(fileMode); err != nil {
		return err
	}
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = s.root.Rename(tmp, fileName); err != nil {
		return err
	}
	return syncDir(s.root)
}

// syncDir makes the rename durable.
func syncDir(r *os.Root) error {
	d, err := r.Open(".")
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
