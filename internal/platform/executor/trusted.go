package executor

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/fstrust"
)

// ErrUntrustedExecutable is matched by the errors of TrustedExecutable
// that are about who could change the program, not about its absence.
var ErrUntrustedExecutable = errors.New("untrusted executable")

// TrustedExecutable resolves the program a service runs and checks that
// only root or sentineld's user could have changed it (D-086): sentineld
// runs it as root or as another user, so whoever can replace it, or a
// directory or link on its way, chooses what that identity runs.
//
// Every directory on the path and on the way of every link is checked
// with the ancestor rule (fstrust.Resolve); the directory holding each
// link and the file itself are checked strictly (owned by root or
// sentineld's user, not writable by group or others); the file is opened
// without following a link or blocking, and must be a regular file with
// an execute bit. It returns the real path, which the caller executes
// (Spec.Path) so that no link is followed again. Not covered: what the
// program itself reads or runs (a script's interpreter inputs, the
// dynamic loader's).
//
// Errors match fs.ErrNotExist for a missing file or directory, and
// ErrUntrustedExecutable for the rest; their text names the path.
func TrustedExecutable(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("executable path must be absolute")
	}
	euid := os.Geteuid()
	for range maxLinks {
		dir, err := fstrust.Resolve(filepath.Dir(path), euid)
		if err != nil {
			return "", untrusted(path, err)
		}
		name := filepath.Base(path)
		next, err := checkInDir(dir, name, euid)
		if err != nil {
			return "", untrusted(path, err)
		}
		if next == "" {
			return filepath.Join(dir, name), nil
		}
		if !filepath.IsAbs(next) {
			next = filepath.Join(dir, next)
		}
		path = next
	}
	return "", fmt.Errorf("%s: %w: more than %d symbolic links", path, ErrUntrustedExecutable, maxLinks)
}

// maxLinks bounds the links a program's path may go through.
const maxLinks = 8

// checkInDir checks dir strictly and the entry name in it. It returns the
// target of a link, or "" for a checked executable.
func checkInDir(dir, name string, euid int) (string, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", err
	}
	defer root.Close()
	info, err := root.Stat(".")
	if err != nil {
		return "", err
	}
	if err := fstrust.CheckOwnership(info, euid); err != nil {
		return "", fmt.Errorf("directory %s %w", dir, err)
	}
	// The directory cannot be changed by another user, so neither can a
	// link in it: follow it to its target, which is checked in turn.
	if info, err = root.Lstat(name); err != nil {
		return "", err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return root.Readlink(name)
	}
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if info, err = f.Stat(); err != nil {
		return "", err
	}
	switch {
	case !info.Mode().IsRegular():
		return "", errors.New("is not a regular file")
	case info.Mode().Perm()&0o111 == 0:
		return "", errors.New("is not executable")
	}
	if err := fstrust.CheckOwnership(info, euid); err != nil {
		return "", err
	}
	return "", nil
}

// untrusted wraps err for path: a missing file stays fs.ErrNotExist.
func untrusted(path string, err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s: %w", path, err)
	}
	return fmt.Errorf("%s: %w: %w", path, ErrUntrustedExecutable, err)
}
