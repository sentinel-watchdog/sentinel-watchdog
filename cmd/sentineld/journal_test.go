package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// streamOf returns the JOURNAL_STREAM value systemd would set for f.
func streamOf(t *testing.T, f *os.File) string {
	t.Helper()
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("no device and inode here")
	}
	return fmt.Sprint(st.Dev) + ":" + fmt.Sprint(st.Ino)
}

// Program output goes to journald's socket only when stderr is the stream
// systemd connected to journald (D-100). journalSender takes no log format, so
// format json forced under systemd changes nothing (brief Q3).
func TestJournalSender(t *testing.T) {
	stderr, err := os.Create(filepath.Join(t.TempDir(), "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	defer stderr.Close()
	other, err := os.Create(filepath.Join(t.TempDir(), "other"))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	tests := []struct {
		name   string
		stderr io.Writer
		env    string
		want   bool
	}{
		{"stderr is the journal stream", stderr, streamOf(t, stderr), true},
		{"another file's stream", stderr, streamOf(t, other), false},
		{"no variable", stderr, "", false},
		{"a malformed variable", stderr, "12", false},
		{"letters", stderr, "a:b", false},
		{"stderr is no file", &bytes.Buffer{}, streamOf(t, stderr), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			getenv := func(k string) string {
				if k == "JOURNAL_STREAM" {
					return tc.env
				}
				return ""
			}
			if got := journalSender(tc.stderr, getenv) != nil; got != tc.want {
				t.Errorf("sender %v, want %v", got, tc.want)
			}
		})
	}
}
