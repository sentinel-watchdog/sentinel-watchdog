package fstrust

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// OpenDir opens a directory the daemon trusts and writes into, creating it
// (and its missing parents) with exactly mode if needed: the process umask
// does not narrow it, so a socket directory created 0755 stays reachable. The existing part of the
// path is checked before anything is created, so root never creates a
// directory through a path another user controls; the missing part is
// created inside that checked directory. The directory itself is checked
// on the opened descriptor. path must be absolute.
func OpenDir(path string, mode fs.FileMode, euid int) (*os.Root, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("%s is not an absolute path", path)
	}
	path = filepath.Clean(path)
	if err := create(path, mode, euid); err != nil {
		return nil, err
	}
	resolved, err := Resolve(path, euid)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	root, err := os.OpenRoot(resolved)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if err := CheckOpened(root, path, euid); err != nil {
		_ = root.Close() // the check error is the one to report
		return nil, err
	}
	return root, nil
}

// CheckOpened applies CheckOwnership to the opened directory of r; display
// names it in the error.
func CheckOpened(r *os.Root, display string, euid int) error {
	info, err := r.Stat(".")
	if err != nil {
		return fmt.Errorf("%s: %w", display, err)
	}
	if err := CheckOwnership(info, euid); err != nil {
		return fmt.Errorf("directory %s %w", display, err)
	}
	return nil
}

// create makes the missing directories of path inside its deepest existing
// ancestor, after checking that ancestor and its own path.
func create(path string, mode fs.FileMode, euid int) error {
	existing := path
	for {
		_, err := os.Lstat(existing)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%s: %w", existing, err)
		}
		existing = filepath.Dir(existing) // "/" always exists
	}
	if existing == path {
		return nil
	}
	resolved, err := Resolve(existing, euid)
	if err != nil {
		return fmt.Errorf("%s: %w", existing, err)
	}
	root, err := os.OpenRoot(resolved)
	if err != nil {
		return fmt.Errorf("open %s: %w", existing, err)
	}
	defer root.Close()
	if err := CheckOpened(root, existing, euid); err != nil {
		return err
	}
	rel, err := filepath.Rel(existing, path)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if err := root.MkdirAll(rel, mode); err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	// Every component of rel is new: give each the requested mode, which
	// the umask may have narrowed.
	created := ""
	for part := range strings.SplitSeq(rel, string(filepath.Separator)) {
		created = filepath.Join(created, part)
		if err := root.Chmod(created, mode); err != nil {
			return fmt.Errorf("chmod %s: %w", filepath.Join(existing, created), err)
		}
	}
	return nil
}
