package executor

import (
	"errors"
	"io"
	"io/fs"
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
	// A link to a file that passes every other check.
	if err := os.WriteFile(filepath.Join(safe, "lone"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("lone", filepath.Join(safe, "lonelink")); err != nil {
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
		{"symbolic link inside the directory", filepath.Join(safe, "lonelink"), 0o640, ErrUnsafeOutputFile},
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

func TestOpenOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.log")
	if err := os.WriteFile(path, []byte("hello\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	f, err := OpenOutput(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil || string(data) != "hello\n" {
		t.Errorf("read %q, %v", data, err)
	}
	if _, err := f.WriteString("x"); err == nil {
		t.Error("the file is writable")
	}
}

// Reading follows the rules of writing: an operator's request must not reach
// another file through a link, nor hang on a FIFO (criterion 3).
func TestOpenOutputRefuses(t *testing.T) {
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
	// A link to a file that passes every other check (os.Root ignores O_NOFOLLOW
	// for a link that stays inside it).
	if err := os.WriteFile(filepath.Join(safe, "lone"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("lone", filepath.Join(safe, "lonelink")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(safe, "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(safe, "groupw"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(safe, "groupw"), 0o620); err != nil {
		t.Fatal(err)
	}
	open := filepath.Join(base, "open")
	if err := os.Mkdir(open, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(open, "log"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o777); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"safe/symlink", "safe/lonelink", "safe/hardlink", "safe/fifo", "safe/groupw", "open/log"} {
		t.Run(name, func(t *testing.T) {
			done := make(chan error, 1)
			go func() {
				f, err := OpenOutput(filepath.Join(base, name))
				if err == nil {
					_ = f.Close()
				}
				done <- err
			}()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("opened")
				}
				if strings.Contains(err.Error(), base) {
					t.Errorf("the error names the path: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("blocked")
			}
		})
	}
	t.Run("relative", func(t *testing.T) {
		if _, err := OpenOutput("worker.log"); err == nil {
			t.Fatal("opened")
		}
	})
	for _, name := range []string{"safe/absent", "absent/log"} {
		t.Run(name, func(t *testing.T) {
			_, err := OpenOutput(filepath.Join(base, name))
			if !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("err = %v, want fs.ErrNotExist", err)
			}
		})
	}
}

// A file of another user cannot be made without root, so the check on the
// opened file is exercised on its own: a file of this user is another user's
// for a daemon with a different identity.
func TestCheckOutputFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root owns the file, which is always allowed")
	}
	path := filepath.Join(t.TempDir(), "worker.log")
	if err := os.WriteFile(path, nil, 0o640); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkOutputFile(info, os.Geteuid()); err != nil {
		t.Errorf("own file: %v", err)
	}
	if err := checkOutputFile(info, os.Geteuid()+1); !errors.Is(err, ErrUnsafeOutputFile) {
		t.Errorf("another user's file: %v", err)
	}
}
