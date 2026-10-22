package state

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
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
	// ErrOlderSchema reports a file with an older schema version that the
	// module cannot migrate (LoadMigrating); the file is left untouched.
	ErrOlderSchema = errors.New("state file has an older schema version and there is no migration")
)

// Store reads and atomically writes one module's state file. It is safe
// for concurrent use.
type Store struct {
	root    *os.Root // the module's directory
	display string   // path used in errors
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
	// MigratedFrom is the schema version the file had when LoadMigrating
	// migrated it (0: no migration). The file now has the current one.
	MigratedFrom int
}

// Migration turns the data of one schema version into the data of the
// next one. It sees the file's data only, never the envelope.
type Migration func(data json.RawMessage) (json.RawMessage, error)

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
//     JSON null when T is a pointer, larger than 16 MiB): the file is moved aside as
//     state.json.corrupt-<UTC time>, and Load returns the zero T and
//     Recovered;
//   - newer or older schema_version: ErrNewerSchema or ErrOlderSchema,
//     file untouched;
//   - a file owned by another user or writable by group or others, and
//     I/O errors: an error, file untouched.
func Load[T any](s *Store) (T, Report, error) { return LoadMigrating[T](s, nil) }

// LoadMigrating is Load for a module whose schema changed: steps[v]
// migrates the data of version v to version v+1. A file of an older
// version is migrated step by step to the current one, decoded and
// validated, then written back atomically at the current version before
// LoadMigrating returns (Report.MigratedFrom), so that a downgrade finds a
// newer file and leaves it alone (D-015). A missing step is
// ErrOlderSchema with the file untouched; a step that fails, or migrated
// data that does not decode or validate, makes the file corrupt.
func LoadMigrating[T any](s *Store, steps map[int]Migration) (T, Report, error) {
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
	var mig migrated
	if err != nil {
		reason = err.Error()
	} else if reason, mig, err = s.decode(data, &value, func() error { return validate(&value) }, steps); err != nil {
		return zero, Report{}, err
	}
	if reason == "" {
		if mig.from == 0 {
			return value, Report{}, nil
		}
		if err := s.write(mig.data); err != nil {
			return zero, Report{}, fmt.Errorf("state: write %s migrated from schema %d: %w", s.display, mig.from, err)
		}
		return value, Report{MigratedFrom: mig.from}, nil
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
		return nil, fmt.Errorf("state: open %s: %w", s.display, err) // errors.Is still finds ErrNotExist
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

// migrated is the outcome of a migration: the version the file had (0:
// none) and the data at the current version.
type migrated struct {
	from int
	data json.RawMessage
}

// decode fills value from data, migrating it first when its version is
// older and steps cover it, then runs check on it. It returns a
// corruption reason, or an error that must leave the file untouched
// (schema version mismatch).
func (s *Store) decode(data []byte, value any, check func() error, steps map[int]Migration) (string, migrated, error) {
	var none migrated
	if len(bytes.TrimSpace(data)) == 0 {
		return "file is empty", none, nil
	}
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return "invalid JSON: " + err.Error(), none, nil
	}
	switch {
	case env.SchemaVersion == nil:
		return "schema_version is missing", none, nil
	case *env.SchemaVersion < 1:
		return fmt.Sprintf("invalid schema_version %d", *env.SchemaVersion), none, nil
	}
	if len(env.Data) == 0 {
		if err := s.checkVersion(*env.SchemaVersion); err != nil {
			return "", none, err
		}
		return "data is missing", none, nil
	}
	body, mig := env.Data, none
	for v := *env.SchemaVersion; v < s.version; v++ {
		step, ok := steps[v]
		if !ok {
			return "", none, s.checkVersion(*env.SchemaVersion)
		}
		next, err := step(body)
		if err != nil {
			return fmt.Sprintf("migration from schema %d: %v", v, err), none, nil
		}
		body, mig = next, migrated{from: *env.SchemaVersion}
	}
	if err := s.checkVersion(max(*env.SchemaVersion, s.version)); err != nil {
		return "", none, err // newer
	}
	if err := json.Unmarshal(body, value); err != nil {
		return "invalid data: " + err.Error(), none, nil
	}
	if err := check(); err != nil {
		return err.Error(), none, nil
	}
	mig.data = body
	return "", mig, nil
}

// errNull is the corruption of a pointer state decoded from JSON null.
var errNull = errors.New("data is null")

// validate runs the Validate method of the state in v, whether T is the
// state type (its method may have a pointer receiver, so *T is asked) or a
// pointer to it (then the pointer itself is asked, and must not be nil).
func validate[T any](v *T) error {
	if val, ok := any(v).(validator); ok {
		return invalid(val.Validate())
	}
	if rv := reflect.ValueOf(*v); rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return errNull
		}
		if val, ok := any(*v).(validator); ok {
			return invalid(val.Validate())
		}
	}
	return nil
}

func invalid(err error) error {
	if err != nil {
		return fmt.Errorf("invalid data: %w", err)
	}
	return nil
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
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkReplaceable(); err != nil {
		return err
	}
	return s.write(data)
}

// write replaces the file with data at the current schema version. The
// caller holds s.mu and has checked that the file may be replaced.
func (s *Store) write(data json.RawMessage) error {
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
