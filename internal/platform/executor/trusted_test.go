package executor

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// D-086, threat model T-02: a program sentineld runs must not be
// replaceable by another user, through the file, a directory or a link on
// its way. The tree is built under t.TempDir(), whose ancestors are
// root-owned and whose directories belong to the test's user (sentineld's
// user here).
func TestTrustedExecutable(t *testing.T) {
	type tree func(t *testing.T, base string) string // returns the path to check
	file := func(t *testing.T, path string, mode fs.FileMode) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil { // past the umask
			t.Fatal(err)
		}
	}
	chmod := func(t *testing.T, path string, mode fs.FileMode) {
		t.Helper()
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	link := func(t *testing.T, target, path string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		name     string
		build    tree
		real     string // the expected real path, relative to base; "" when refused
		notExist bool
	}{
		{"trusted file", func(t *testing.T, b string) string {
			file(t, b+"/bin/worker", 0o755)
			return b + "/bin/worker"
		}, "bin/worker", false},
		{"link in its own directory", func(t *testing.T, b string) string {
			file(t, b+"/bin/dash", 0o755)
			link(t, "dash", b+"/bin/sh")
			return b + "/bin/sh"
		}, "bin/dash", false},
		{"link to another trusted directory", func(t *testing.T, b string) string {
			file(t, b+"/usr/bin/bash", 0o755)
			link(t, b+"/usr/bin/bash", b+"/etc/alternatives/sh")
			link(t, b+"/etc/alternatives/sh", b+"/bin/sh")
			return b + "/bin/sh"
		}, "usr/bin/bash", false},
		{"group-writable directory", func(t *testing.T, b string) string {
			file(t, b+"/bin/worker", 0o755)
			chmod(t, b+"/bin", 0o775)
			return b + "/bin/worker"
		}, "", false},
		{"writable file", func(t *testing.T, b string) string {
			file(t, b+"/bin/worker", 0o757)
			return b + "/bin/worker"
		}, "", false},
		{"not executable", func(t *testing.T, b string) string {
			file(t, b+"/bin/worker", 0o644)
			return b + "/bin/worker"
		}, "", false},
		// C7: a link another user could swap, in a sticky world-writable
		// directory, even though it points to a trusted file.
		{"link in a sticky directory", func(t *testing.T, b string) string {
			file(t, b+"/bin/worker", 0o755)
			link(t, b+"/bin/worker", b+"/tmp/worker")
			chmod(t, b+"/tmp", 0o1777)
			return b + "/tmp/worker"
		}, "", false},
		{"link through an untrusted directory", func(t *testing.T, b string) string {
			file(t, b+"/open/bin/worker", 0o755)
			chmod(t, b+"/open", 0o777)
			link(t, b+"/open/bin/worker", b+"/bin/worker")
			return b + "/bin/worker"
		}, "", false},
		{"fifo", func(t *testing.T, b string) string {
			if err := os.MkdirAll(b+"/bin", 0o755); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(b+"/bin/worker", 0o755); err != nil {
				t.Fatal(err)
			}
			return b + "/bin/worker"
		}, "", false},
		{"directory", func(t *testing.T, b string) string {
			if err := os.MkdirAll(b+"/bin/worker", 0o755); err != nil {
				t.Fatal(err)
			}
			return b + "/bin/worker"
		}, "", false},
		{"missing", func(t *testing.T, b string) string {
			if err := os.MkdirAll(b+"/bin", 0o755); err != nil {
				t.Fatal(err)
			}
			return b + "/bin/worker"
		}, "", true},
		{"link loop", func(t *testing.T, b string) string {
			link(t, "b", b+"/bin/a")
			link(t, "a", b+"/bin/b")
			return b + "/bin/a"
		}, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base, err := filepath.EvalSymlinks(t.TempDir()) // macOS: /var is a link
			if err != nil {
				t.Fatal(err)
			}
			got, err := TrustedExecutable(tt.build(t, base))
			switch {
			case tt.real != "":
				if err != nil || got != filepath.Join(base, tt.real) {
					t.Errorf("got %q, %v; want %q", got, err, tt.real)
				}
			case tt.notExist:
				if !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("err = %v, want not exist", err)
				}
			default:
				if !errors.Is(err, ErrUntrustedExecutable) {
					t.Errorf("got %q, %v; want ErrUntrustedExecutable", got, err)
				}
			}
		})
	}
	// The system's shell passes, whatever links lead to it.
	if _, err := TrustedExecutable("/bin/sh"); err != nil {
		t.Errorf("/bin/sh: %v", err)
	}
	if _, err := TrustedExecutable("bin/sh"); err == nil {
		t.Error("accepted a relative path")
	}
}
