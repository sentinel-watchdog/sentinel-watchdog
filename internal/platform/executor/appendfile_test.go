package executor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestOpenAppend(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "worker.log")
	for _, line := range []string{"one\n", "two\n"} {
		f, err := OpenAppend(path, 0o640)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.WriteString(line)
		_ = f.Close()
	}
	data, _ := os.ReadFile(path)
	info, _ := os.Stat(path)
	if string(data) != "one\ntwo\n" || info.Mode().Perm() != 0o640 {
		t.Errorf("content %q, mode %v", data, info.Mode().Perm())
	}
}

// D-084: sentineld writes output files as root, so every way to redirect
// or block that write is refused, and no error names the path.
func TestOpenAppendRefuses(t *testing.T) {
	base := t.TempDir()
	safe := filepath.Join(base, "safe")
	if err := os.Mkdir(safe, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(safe, "target")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(safe, "symlink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(target, filepath.Join(safe, "hardlink")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(safe, "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(safe, "shared"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(safe, "shared"), 0o666); err != nil {
		t.Fatal(err)
	}
	open := filepath.Join(base, "open")
	if err := os.Mkdir(open, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o777); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(open, 0o755) })
	tests := []struct {
		name string
		path string
		mode os.FileMode
		is   error
	}{
		{"symbolic link", filepath.Join(safe, "symlink"), 0o640, nil},
		{"hard link", filepath.Join(safe, "hardlink"), 0o640, ErrUnsafeOutputFile},
		{"FIFO", filepath.Join(safe, "fifo"), 0o640, nil},
		{"writable by others", filepath.Join(safe, "shared"), 0o640, ErrUnsafeOutputFile},
		{"writable directory", filepath.Join(open, "x.log"), 0o640, nil},
		{"relative path", "x.log", 0o640, nil},
		{"group-writable mode", filepath.Join(safe, "new.log"), 0o660, ErrUnsafeOutputMode},
		{"special mode bits", filepath.Join(safe, "new.log"), os.ModeSetuid | 0o640, ErrUnsafeOutputMode},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			done := make(chan error, 1)
			go func() {
				f, err := OpenAppend(tt.path, tt.mode)
				if err == nil {
					_ = f.Close()
				}
				done <- err
			}()
			var err error
			select {
			case err = <-done:
			case <-time.After(10 * time.Second): // a FIFO must not block
				t.Fatal("OpenAppend blocked")
			}
			if err == nil || (tt.is != nil && !errors.Is(err, tt.is)) {
				t.Fatalf("err = %v, want %v", err, tt.is)
			}
			if tt.is != nil && strings.Contains(err.Error(), safe) {
				t.Errorf("the error names the path: %v", err)
			}
		})
	}
	if data, _ := os.ReadFile(target); len(data) != 0 {
		t.Error("the link target was written")
	}
}
