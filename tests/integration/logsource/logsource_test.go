//go:build integration && linux

// Package logsource_test reads a real journal through the real runner (task
// test-vm, D-056, D-099). It installs throwaway units under
// /run/systemd/system as test fixtures, which the product never does
// (PLAN.md §2), and removes them at the end. On a host without systemd
// (Alpine) it checks that the journal is unavailable and a file still reads.
// The suite fails, not skips, without root: it is run on purpose.
package logsource_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/clock"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/platform/executor"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/platform/logsource"
)

const runtimeUnits = "/run/systemd/system"

func plain(s string) string { return s }

func newReader(t *testing.T) *logsource.Reader {
	t.Helper()
	e := executor.New(clock.Real())
	t.Cleanup(e.Close)
	return logsource.NewReader(e, slog.New(slog.DiscardHandler))
}

func hasSystemd() bool {
	fi, err := os.Stat(runtimeUnits)
	return err == nil && fi.IsDir()
}

func TestMain(m *testing.M) {
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "tests/integration/logsource needs root (run it with task test-vm)")
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func systemctl(t *testing.T, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "systemctl", args...).CombinedOutput(); err != nil {
		t.Fatalf("systemctl %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// uniqueUnit names a unit this run alone has used: the journal keeps the lines
// of earlier runs on the same host.
func uniqueUnit(what string) string {
	return fmt.Sprintf("sentinel-itest-%s-%d.service", what, time.Now().UnixNano())
}

// installLines installs and runs a oneshot unit that prints n numbered lines.
func installLines(t *testing.T, name string, n int) {
	t.Helper()
	body := fmt.Sprintf("[Service]\nType=oneshot\nExecStart=/bin/sh -c 'i=1; while [ $i -le %d ]; do echo n$i; i=$((i+1)); done'\n", n)
	path := filepath.Join(runtimeUnits, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Remove(path)
		_ = exec.Command("systemctl", "daemon-reload").Run()
	})
	systemctl(t, "daemon-reload")
	systemctl(t, "start", name)
}

// numbered returns the numbers of the n<i> lines of r, in order.
func numbered(r logsource.Result) []int {
	var out []int
	for _, l := range r.Lines {
		if i, err := strconv.Atoi(strings.TrimPrefix(l.Text, "n")); err == nil && strings.HasPrefix(l.Text, "n") {
			out = append(out, i)
		}
	}
	return out
}

// eventually retries until ok: journald takes what a unit writes through a
// stream, a little after the unit has ended.
func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: never happened", what)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestNoJournalWithoutSystemd(t *testing.T) {
	if hasSystemd() {
		t.Skip("this host has systemd")
	}
	_, err := newReader(t).Unit(t.Context(), logsource.Named("sshd.service"), 5, plain)
	if !errors.Is(err, logsource.ErrUnavailable) {
		t.Errorf("err = %v, want ErrUnavailable", err)
	}
	path := filepath.Join(t.TempDir(), "worker.log")
	if err := os.WriteFile(path, []byte("one\ntwo\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	got, err := newReader(t).File(t.Context(), path, 1, plain)
	if err != nil || len(got.Lines) != 1 || got.Lines[0].Text != "two" {
		t.Errorf("%+v, %v", got, err)
	}
}

// Criterion 1, and the fault measured on Rocky 8 that the argv avoids:
// --reverse there returned older lines and lost the newest.
func TestNewestLinesInOrder(t *testing.T) {
	if !hasSystemd() {
		t.Skip("no systemd")
	}
	unit := uniqueUnit("logs")
	installLines(t, unit, 30)
	r := newReader(t)
	var got logsource.Result
	eventually(t, "the unit's last line in the journal", func() bool {
		var err error
		got, err = r.Unit(t.Context(), logsource.Named(unit), 1000, plain)
		if err != nil {
			t.Fatal(err)
		}
		return slices.Contains(numbered(got), 30)
	})
	want := make([]int, 30)
	for i := range want {
		want[i] = i + 1
	}
	if !slices.Equal(numbered(got), want) {
		t.Errorf("all lines: %v", numbered(got))
	}
	// systemd's own messages about the unit come last: asking for fewer lines
	// than the unit wrote must still reach n30.
	few, err := r.Unit(t.Context(), logsource.Named(unit), 10, plain)
	if err != nil {
		t.Fatal(err)
	}
	if nums := numbered(few); len(few.Lines) != 10 || len(nums) == 0 || nums[0] <= 20 || nums[len(nums)-1] != 30 || !slices.IsSorted(nums) {
		t.Errorf("the last 10: %d lines, numbers %v", len(few.Lines), nums)
	}
	if few.Lines[0].Time.IsZero() || few.Truncated {
		t.Errorf("first line %+v, truncated %v", few.Lines[0], few.Truncated)
	}
}

func TestNoLinesIsNotAnError(t *testing.T) {
	if !hasSystemd() {
		t.Skip("no systemd")
	}
	got, err := newReader(t).Unit(t.Context(), logsource.Named("sentinel-itest-no-such-unit.service"), 5, plain)
	if err != nil || len(got.Lines) != 0 || got.Lines == nil {
		t.Errorf("%+v, %v", got, err)
	}
}

// What the journal holds and journalctl writes: a line that is not UTF-8, one
// over 4 KiB, and a terminal sequence, on the real path.
func TestRealEncodings(t *testing.T) {
	if !hasSystemd() {
		t.Skip("no systemd")
	}
	unit := uniqueUnit("enc")
	body := "[Service]\nType=oneshot\nExecStart=/bin/bash -c 'printf \"bad\\\\xff\\\\xfebytes\\\\n\"; printf \"esc\\\\x1b[2Jape\\\\n\"; head -c 5000 /dev/zero | tr \"\\\\0\" x; echo; echo the-end'\n"
	path := filepath.Join(runtimeUnits, unit)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Remove(path)
		_ = exec.Command("systemctl", "daemon-reload").Run()
	})
	systemctl(t, "daemon-reload")
	systemctl(t, "start", unit)
	r := newReader(t)
	var texts []string
	eventually(t, "the unit's last line in the journal", func() bool {
		got, err := r.Unit(t.Context(), logsource.Named(unit), 50, plain)
		if err != nil {
			t.Fatal(err)
		}
		texts = texts[:0]
		for _, l := range got.Lines {
			texts = append(texts, l.Text)
		}
		return slices.Contains(texts, "the-end")
	})
	for _, want := range []string{"bad�bytes", "esc\x1b[2Jape"} {
		if !slices.Contains(texts, want) {
			t.Errorf("%q not in %q", want, texts)
		}
	}
	for _, line := range texts {
		if len(line) > 4096 {
			t.Errorf("a line of %d bytes", len(line))
		}
	}
}

// Criterion 6: journalctl does not run as root. Root would still read a
// journal closed to its group, so a refusal here proves the identity, and that
// a permission failure is reported, never an empty log.
func TestRunsWithoutRoot(t *testing.T) {
	if !hasSystemd() {
		t.Skip("no systemd")
	}
	unit := uniqueUnit("identity")
	installLines(t, unit, 3)
	r := newReader(t)
	eventually(t, "the unit's last line in the journal", func() bool {
		got, err := r.Unit(t.Context(), logsource.Named(unit), 50, plain)
		if err != nil {
			t.Fatal(err) // this read is as nobody:systemd-journal: V4 on every system
		}
		return slices.Contains(numbered(got), 3)
	})
	for _, dir := range []string{"/run/log/journal", "/var/log/journal"} {
		info, err := os.Stat(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		// Stays in place: the mask a directory's ACL needs comes back with
		// the group's bits.
		t.Cleanup(func() { _ = os.Chmod(dir, info.Mode()) })
		if err := os.Chmod(dir, info.Mode()&^0o077); err != nil {
			t.Fatal(err)
		}
	}
	got, err := r.Unit(t.Context(), logsource.Named(unit), 50, plain)
	if !errors.Is(err, logsource.ErrUnavailable) {
		t.Errorf("with the journal closed to its group: %+v, %v", got, err)
	}
}

func TestProbeSeesTheJournal(t *testing.T) {
	if !hasSystemd() {
		t.Skip("no systemd")
	}
	e := executor.New(clock.Real())
	defer e.Close()
	out, err := e.Journal(t.Context(), executor.JournalQuery{Lines: 1})
	if err != nil || out.Exit.Code != 0 || len(out.Stdout) == 0 {
		t.Errorf("%+v, %v", out, err)
	}
}
