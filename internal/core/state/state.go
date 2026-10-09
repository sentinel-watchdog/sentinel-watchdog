// Package state keeps the persistent state of the core and of every module
// under daemon.state_dir: one directory per module, holding a state.json
// with the module's own schema_version (ADR-0011 as amended by ADR-0015).
//
// Writes are atomic (temporary file, fsync, rename, fsync of the
// directory). A corrupt file is moved aside and the module starts fresh; a
// file written by a newer Sentinel is never overwritten (D-015). Every
// file and directory is resolved inside the opened state directory and
// checked like configuration (internal/core/fstrust): the daemon must not
// trust state another user could have written.
package state

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sync"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/clock"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/fstrust"
)

// File and directory modes: state is private to the daemon user.
const (
	dirMode       fs.FileMode = 0o750 // the state directory
	moduleDirMode fs.FileMode = 0o700 // one directory per module
	fileMode      fs.FileMode = 0o600
)

// moduleNameRe matches module names (internal/core/module uses the same format).
var moduleNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// Dir is the opened state directory.
type Dir struct {
	root  *os.Root
	path  string
	euid  int
	clock clock.Clock

	mu     sync.Mutex
	stores map[string]*Store // one per module, so its lock is shared
}

// OpenDir creates the state directory if it is missing, checks every
// directory on its path and the directory itself (owned by root or the
// daemon user, not writable by group or others), and opens it. path must
// be absolute. Close it when the daemon stops.
func OpenDir(path string, c clock.Clock) (*Dir, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("state: %s is not an absolute path", path)
	}
	path = filepath.Clean(path)
	euid := os.Geteuid()
	if err := create(path, euid); err != nil {
		return nil, err
	}
	resolved, err := fstrust.Resolve(path, euid)
	if err != nil {
		return nil, fmt.Errorf("state: %s: %w", path, err)
	}
	root, err := os.OpenRoot(resolved)
	if err != nil {
		return nil, fmt.Errorf("state: open %s: %w", path, err)
	}
	if err := checkOpened(root, path, euid); err != nil {
		_ = root.Close() // the check error is the one to report
		return nil, err
	}
	return &Dir{root: root, path: path, euid: euid, clock: c, stores: map[string]*Store{}}, nil
}

// Close releases the directory and its stores.
func (d *Dir) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	var errs []error
	for _, s := range d.stores {
		errs = append(errs, s.root.Close())
	}
	return errors.Join(append(errs, d.root.Close())...)
}

// Store returns the store of module's state file, creating the module's
// directory if it is missing. schemaVersion is the version this binary
// writes and reads. Every call for one module returns the same Store, so
// loads and saves of that module are serialised.
func (d *Dir) Store(module string, schemaVersion int) (*Store, error) {
	if !moduleNameRe.MatchString(module) {
		return nil, fmt.Errorf("state: invalid module name %q", module)
	}
	if schemaVersion < 1 {
		return nil, fmt.Errorf("state: module %q: schema version must be at least 1", module)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if s, ok := d.stores[module]; ok {
		if s.version != schemaVersion {
			return nil, fmt.Errorf("state: module %q is already open with schema version %d", module, s.version)
		}
		return s, nil
	}
	display := d.path + "/" + module
	if err := d.root.Mkdir(module, moduleDirMode); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, fmt.Errorf("state: create %s: %w", display, err)
	}
	// Lstat: a symbolic link here could lead the daemon to write elsewhere.
	info, err := d.root.Lstat(module)
	if err != nil {
		return nil, fmt.Errorf("state: %s: %w", display, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("state: %s is not a directory", display)
	}
	root, err := d.root.OpenRoot(module)
	if err != nil {
		return nil, fmt.Errorf("state: open %s: %w", display, err)
	}
	if err := checkOpened(root, display, d.euid); err != nil {
		_ = root.Close() // the check error is the one to report
		return nil, err
	}
	s := &Store{root: root, display: display + "/" + fileName, module: module,
		version: schemaVersion, euid: d.euid, clock: d.clock}
	d.stores[module] = s
	return s, nil
}

// create makes the missing directories of path inside its deepest
// existing ancestor, after checking that ancestor and its own path: root
// must not create directories through a path another user controls.
func create(path string, euid int) error {
	existing := path
	for {
		_, err := os.Lstat(existing)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("state: %s: %w", existing, err)
		}
		existing = filepath.Dir(existing) // "/" always exists
	}
	if existing == path {
		return nil
	}
	resolved, err := fstrust.Resolve(existing, euid)
	if err != nil {
		return fmt.Errorf("state: %s: %w", existing, err)
	}
	root, err := os.OpenRoot(resolved)
	if err != nil {
		return fmt.Errorf("state: open %s: %w", existing, err)
	}
	defer root.Close()
	if err := checkOpened(root, existing, euid); err != nil {
		return err
	}
	rel, err := filepath.Rel(existing, path)
	if err != nil {
		return fmt.Errorf("state: %s: %w", path, err)
	}
	if err := root.MkdirAll(rel, dirMode); err != nil {
		return fmt.Errorf("state: create %s: %w", path, err)
	}
	return nil
}

// checkOpened applies the ownership rule to the opened directory of r.
func checkOpened(r *os.Root, display string, euid int) error {
	info, err := r.Stat(".")
	if err != nil {
		return fmt.Errorf("state: %s: %w", display, err)
	}
	if err := fstrust.CheckOwnership(info, euid); err != nil {
		return fmt.Errorf("state: directory %s %w", display, err)
	}
	return nil
}
