package state

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/clock"
)

// Full review 2026-10-28 C1: a state directory has one owner. A second
// OpenDir of the same directory fails while the first is open, whatever
// socket each daemon uses, and succeeds once it is closed.
func TestOpenDirIsExclusive(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	first, err := OpenDir(dir, clock.NewFake(now))
	if err != nil {
		t.Fatal(err)
	}
	if second, err := OpenDir(dir, clock.NewFake(now)); err == nil || !strings.Contains(err.Error(), "in use") {
		if err == nil {
			_ = second.Close()
		}
		t.Fatalf("second OpenDir: %v, want in use", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := OpenDir(dir, clock.NewFake(now))
	if err != nil {
		t.Fatalf("OpenDir after Close: %v", err)
	}
	_ = again.Close()
}

// The lock file is opened inside the checked directory without following
// a link and without blocking: a link or a FIFO in its place is refused.
func TestOpenDirRefusesAnUntrustedLockFile(t *testing.T) {
	for name, plant := range map[string]func(path string) error{
		"symlink": func(path string) error { return os.Symlink("/etc/passwd", path) },
		"fifo":    func(path string) error { return syscall.Mkfifo(path, 0o600) },
		"readable by others": func(path string) error { // others could hold the flock
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				return err
			}
			return os.Chmod(path, 0o644)
		},
		"group-writable": func(path string) error {
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				return err
			}
			return os.Chmod(path, 0o660)
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "state")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := plant(filepath.Join(dir, lockFile)); err != nil {
				t.Fatal(err)
			}
			if d, err := OpenDir(dir, clock.NewFake(now)); err == nil {
				_ = d.Close()
				t.Fatal("OpenDir accepted an untrusted lock file")
			}
		})
	}
}
