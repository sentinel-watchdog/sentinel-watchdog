package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/model"
)

// DefaultMaxFileSize caps the state file read at startup.
const DefaultMaxFileSize = 64 << 20

// File permissions: the state is private to the daemon user.
const (
	fileMode fs.FileMode = 0o600
	dirMode  fs.FileMode = 0o750
)

// ErrNewerSchema is returned when the state file was written by a newer
// Sentinel. The file is left untouched so a downgrade cannot destroy it.
var ErrNewerSchema = errors.New("state file was written by a newer version of sentinel")

var errTooLarge = errors.New("state file exceeds the size limit")

// LoadReport describes what Load found.
type LoadReport struct {
	// FirstRun: no state file existed.
	FirstRun bool
	// Recovered: the file was unreadable and has been moved aside.
	Recovered bool
	// QuarantinedTo is the new path of the corrupt file.
	QuarantinedTo string
	// Reason explains why the file was considered corrupt.
	Reason string
}

// Store reads and atomically writes the state file.
type Store struct {
	path    string
	maxSize int64
	now     func() time.Time
	mu      sync.Mutex // serialises Save and quarantine renames
}

// Option configures a Store.
type Option func(*Store)

// WithClock overrides the time source (tests).
func WithClock(now func() time.Time) Option { return func(s *Store) { s.now = now } }

// WithMaxFileSize overrides DefaultMaxFileSize.
func WithMaxFileSize(n int64) Option { return func(s *Store) { s.maxSize = n } }

// NewStore returns a store for the state file at path.
func NewStore(path string, opts ...Option) *Store {
	s := &Store{path: path, maxSize: DefaultMaxFileSize, now: time.Now}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Path returns the state file path.
func (s *Store) Path() string { return s.path }

// Load reads the state file.
//
//   - missing file: empty state, FirstRun;
//   - corrupt file (invalid JSON, missing or invalid schema_version, failed
//     invariants, larger than the size limit): renamed to <path>.corrupt-<UTC timestamp>, empty state,
//     Recovered;
//   - newer schema_version: ErrNewerSchema, file untouched;
//   - I/O and permission errors: returned (check with errors.Is and
//     fs.ErrPermission).
func (s *Store) Load() (*File, LoadReport, error) {
	data, err := readLimited(s.path, s.maxSize)
	if errors.Is(err, fs.ErrNotExist) {
		return New(), LoadReport{FirstRun: true}, nil
	}
	var (
		f      *File
		reason string
	)
	switch {
	case errors.Is(err, errTooLarge):
		reason = err.Error()
	case err != nil:
		return nil, LoadReport{}, fmt.Errorf("read state file %s: %w", s.path, err)
	default:
		if f, reason, err = decode(data); err != nil {
			return nil, LoadReport{}, err
		}
	}
	if reason == "" {
		return f, LoadReport{}, nil
	}

	dest, qerr := s.quarantine()
	if qerr != nil {
		return nil, LoadReport{}, fmt.Errorf("state file %s is corrupt (%s) and could not be moved aside: %w", s.path, reason, qerr)
	}
	return New(), LoadReport{Recovered: true, QuarantinedTo: dest, Reason: reason}, nil
}

// decode returns the parsed file, or a non-empty corruption reason, or an
// error that must stop startup.
func decode(data []byte) (*File, string, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, "file is empty", nil
	}
	var head struct {
		SchemaVersion *int `json:"schema_version"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, "invalid JSON: " + err.Error(), nil
	}
	switch {
	case head.SchemaVersion == nil:
		return nil, "schema_version is missing", nil
	case *head.SchemaVersion > SchemaVersion:
		return nil, "", fmt.Errorf("%w (file schema %d, supported %d)", ErrNewerSchema, *head.SchemaVersion, SchemaVersion)
	case *head.SchemaVersion < 1:
		return nil, fmt.Sprintf("invalid schema_version %d", *head.SchemaVersion), nil
	}
	// Older schemas would be migrated here once SchemaVersion > 1.

	f := New()
	if err := json.Unmarshal(data, f); err != nil {
		return nil, "invalid content: " + err.Error(), nil
	}
	if f.Monitors == nil {
		f.Monitors = map[string]*Monitor{}
	}
	if f.Events == nil {
		f.Events = []model.Event{}
	}
	if err := f.Validate(); err != nil {
		return nil, "invalid content: " + err.Error(), nil
	}
	return f, "", nil
}

func (s *Store) quarantine() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dest := fmt.Sprintf("%s.corrupt-%s", s.path, s.now().UTC().Format("20060102T150405.000000000Z"))
	if err := os.Rename(s.path, dest); err != nil {
		return "", err
	}
	return dest, nil
}

// Save writes f atomically: temp file in the same directory, fsync,
// rename over the target, fsync of the directory. The file mode is 0600.
func (s *Store) Save(f *File) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	f.SchemaVersion = SchemaVersion
	f.UpdatedAt = s.now().UTC()
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	data = append(data, '\n')
	return writeAtomic(s.path, data)
}

func writeAtomic(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return fmt.Errorf("create state directory %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary state file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()

	if err = tmp.Chmod(fileMode); err != nil {
		return fmt.Errorf("chmod temporary state file: %w", err)
	}
	if _, err = tmp.Write(data); err != nil {
		return fmt.Errorf("write temporary state file: %w", err)
	}
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("sync temporary state file: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("close temporary state file: %w", err)
	}
	if err = os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace state file: %w", err)
	}
	if err = syncDir(dir); err != nil {
		return fmt.Errorf("sync state directory: %w", err)
	}
	return nil
}

// syncDir makes the rename durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func readLimited(path string, maxSize int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxSize+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxSize {
		return nil, errTooLarge
	}
	return data, nil
}
