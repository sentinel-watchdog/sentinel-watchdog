//go:build integration && linux

// Package systemd_test reads real units over systemd's real private socket (task
// test-vm, D-056, D-093). It installs throwaway units under
// /run/systemd/system as test fixtures, which the product never does
// (PLAN.md §2), and removes them at the end. On a host without systemd
// (Alpine) it checks only that the adapter answers ErrUnavailable. The
// suite fails, not skips, without root: it is run on purpose.
package systemd_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/platform/systemd"
)

const runtimeUnits = "/run/systemd/system"

func TestMain(m *testing.M) {
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "tests/integration/systemd needs root (run it with task test-vm)")
		os.Exit(1)
	}
	if fi, err := os.Stat(runtimeUnits); err != nil || !fi.IsDir() {
		// No systemd (Alpine): the adapter must say so, and nothing else runs.
		b := systemd.Open()
		defer b.Close()
		_, err := b.Unit(context.Background(), mustName("sshd.service"))
		if !errors.Is(err, systemd.ErrUnavailable) {
			fmt.Fprintf(os.Stderr, "without systemd: error = %v, want ErrUnavailable\n", err)
			os.Exit(1)
		}
		fmt.Println("no systemd on this host: ErrUnavailable as expected")
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func mustName(s string) systemd.UnitName {
	n, err := systemd.ParseUnitName(s)
	if err != nil {
		panic(err)
	}
	return n
}

func systemctl(t *testing.T, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "systemctl", args...).CombinedOutput(); err != nil {
		t.Fatalf("systemctl %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// install writes a throwaway unit and removes it when the test ends.
func install(t *testing.T, name, body string) {
	t.Helper()
	path := filepath.Join(runtimeUnits, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	systemctl(t, "daemon-reload")
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = exec.CommandContext(ctx, "systemctl", "stop", name).Run()         // best effort: the unit goes away below
		_ = exec.CommandContext(ctx, "systemctl", "reset-failed", name).Run() // a failed unit stays listed otherwise
		_ = os.Remove(path)                                                   // the fixture
		_ = exec.CommandContext(ctx, "systemctl", "daemon-reload").Run()      // forget it
	})
}

// eventually reads n until ok accepts the state or the deadline passes.
func eventually(t *testing.T, b *systemd.Bus, n systemd.UnitName, ok func(systemd.UnitState) bool) systemd.UnitState {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		st, err := b.Unit(ctx, n)
		cancel()
		if err == nil && ok(st) {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: last state %+v, error %v", n, st, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

const sleeper = "[Service]\nExecStart=/bin/sleep infinity\nRestart=always\nRestartSec=1\n"

func TestActiveAndRestartedBySystemd(t *testing.T) {
	install(t, "sentinel-itest-sleep.service", sleeper)
	systemctl(t, "start", "sentinel-itest-sleep.service")
	b := systemd.Open()
	defer b.Close()
	n := mustName("sentinel-itest-sleep.service")
	first := eventually(t, b, n, func(s systemd.UnitState) bool { return s.Active == systemd.ActiveActive })
	if first.Load != systemd.LoadLoaded || first.Sub != "running" || first.InvocationID == "" || first.ID != n.String() {
		t.Fatalf("state %+v", first)
	}
	systemctl(t, "kill", "--signal=KILL", "sentinel-itest-sleep.service") // systemd restarts it (Restart=always)
	again := eventually(t, b, n, func(s systemd.UnitState) bool {
		return s.Active == systemd.ActiveActive && s.InvocationID != first.InvocationID
	})
	if again.NRestarts <= first.NRestarts {
		t.Errorf("NRestarts %d → %d", first.NRestarts, again.NRestarts)
	}
}

func TestFailed(t *testing.T) {
	install(t, "sentinel-itest-false.service", "[Service]\nType=oneshot\nExecStart=/bin/false\n")
	// The start fails, as it should: the unit ends failed.
	_ = exec.Command("systemctl", "start", "sentinel-itest-false.service").Run()
	b := systemd.Open()
	defer b.Close()
	st := eventually(t, b, mustName("sentinel-itest-false.service"), func(s systemd.UnitState) bool {
		return s.Active == systemd.ActiveFailed
	})
	if st.Result == "" || st.Result == "success" {
		t.Errorf("state %+v", st)
	}
}

// An inactive unit and one that does not exist are read through LoadUnit.
func TestNotLoaded(t *testing.T) {
	install(t, "sentinel-itest-idle.service", sleeper)
	b := systemd.Open()
	defer b.Close()
	idle := eventually(t, b, mustName("sentinel-itest-idle.service"), func(systemd.UnitState) bool { return true })
	if idle.Active != systemd.ActiveInactive || idle.Load != systemd.LoadLoaded {
		t.Errorf("idle %+v", idle)
	}
	missing := eventually(t, b, mustName("sentinel-itest-missing-0f3c.service"), func(systemd.UnitState) bool { return true })
	if missing.Load != systemd.LoadNotFound {
		t.Errorf("missing %+v", missing)
	}
}

func TestTemplateInstance(t *testing.T) {
	install(t, "sentinel-itest-tmpl@.service", sleeper)
	systemctl(t, "start", "sentinel-itest-tmpl@a.service")
	t.Cleanup(func() { _ = exec.Command("systemctl", "stop", "sentinel-itest-tmpl@a.service").Run() }) // best effort
	b := systemd.Open()
	defer b.Close()
	eventually(t, b, mustName("sentinel-itest-tmpl@a.service"), func(s systemd.UnitState) bool {
		return s.Active == systemd.ActiveActive
	})
}

// systemd re-executing itself does not leave the adapter stuck: reads
// succeed again, over the same connection or a new one.
func TestDaemonReexec(t *testing.T) {
	install(t, "sentinel-itest-reexec.service", sleeper)
	systemctl(t, "start", "sentinel-itest-reexec.service")
	b := systemd.Open()
	defer b.Close()
	n := mustName("sentinel-itest-reexec.service")
	eventually(t, b, n, func(s systemd.UnitState) bool { return s.Active == systemd.ActiveActive })
	systemctl(t, "daemon-reexec")
	eventually(t, b, n, func(s systemd.UnitState) bool { return s.Active == systemd.ActiveActive })
}
