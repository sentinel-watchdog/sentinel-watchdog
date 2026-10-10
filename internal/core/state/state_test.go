package state

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/clock"
)

// Review 2b R3: OpenDir checks the existing part of the path before it
// creates the missing directories, so root never creates a directory
// through a path it then refuses.
func TestOpenDirChecksBeforeCreating(t *testing.T) {
	base := t.TempDir()
	open, target := filepath.Join(base, "open"), filepath.Join(base, "target")
	for _, d := range []string{open, target} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(open, 0o777); err != nil { // writable by others, not sticky
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(open, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenDir(filepath.Join(open, "link", "state"), clock.NewFake(now)); err == nil {
		t.Fatal("unsafe path accepted")
	}
	if _, err := os.Stat(filepath.Join(target, "state")); err == nil {
		t.Error("OpenDir created a directory through the path it then refused")
	}
}

func TestOpenDirCreatesMissingDirectories(t *testing.T) {
	path := filepath.Join(t.TempDir(), "var", "lib", "sentinel")
	d, err := OpenDir(path, clock.NewFake(now))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() || info.Mode().Perm()&0o027 != 0 {
		t.Errorf("created %s with mode %s", path, info.Mode())
	}
}

// A relative path would be checked from / but created from the working
// directory.
func TestOpenDirRejectsRelativePaths(t *testing.T) {
	t.Chdir(t.TempDir())
	if _, err := OpenDir("state", clock.NewFake(now)); err == nil {
		t.Error("relative state directory accepted")
	}
	if _, err := os.Stat("state"); err == nil {
		t.Error("OpenDir created a relative directory")
	}
}
