//go:build integration && linux

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

// The contract 3c-2 relies on (D-095), on the systemd of each VM of the
// matrix: RestartUnit in mode fail queues a job read back from the Job
// property until it ends; systemd answers a refusal with an error, before
// it queues anything. Each run logs the systemd version it ran on.

func TestSystemdVersion(t *testing.T) {
	out, err := exec.Command("systemctl", "--version").Output()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s", strings.SplitN(string(out), "\n", 2)[0])
}

// restart asks for a restart of n with a 10 s bound, after a read of the
// unit, as the supervisor always does: a call sent as the first one on a
// connection opened right after a daemon-reload was sometimes never
// answered on the matrix (B-040), which the read absorbs.
func restart(b *systemd.Bus, n systemd.UnitName) (uint32, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := b.Unit(ctx, n); err != nil {
		return 0, fmt.Errorf("the read before the restart: %w", err)
	}
	return b.Restart(ctx, n)
}

// A slow start keeps its job visible: the job id returned is the unit's
// Job until the start completes, then the unit runs with a new
// invocation.
func TestRestartJobPolled(t *testing.T) {
	install(t, "sentinel-itest-slow.service", "[Service]\nExecStartPre=/bin/sleep 3\nExecStart=/bin/sleep infinity\n")
	b := systemd.Open()
	defer b.Close()
	n := mustName("sentinel-itest-slow.service")
	before := eventually(t, b, n, func(systemd.UnitState) bool { return true })
	job, err := restart(b, n)
	if err != nil || job == 0 {
		t.Fatalf("Restart = %d, %v", job, err)
	}
	seen := eventually(t, b, n, func(s systemd.UnitState) bool { return s.Job == job })
	if seen.Active != systemd.ActiveActivating {
		t.Errorf("with the job queued: %+v", seen)
	}
	after := eventually(t, b, n, func(s systemd.UnitState) bool { return s.Job != job })
	if after.Active != systemd.ActiveActive || after.InvocationID == "" || after.InvocationID == before.InvocationID {
		t.Errorf("after the job: %+v (before %+v)", after, before)
	}
}

// A start that fails ends the job; the unit is failed, not running.
func TestRestartJobFails(t *testing.T) {
	install(t, "sentinel-itest-fails.service", "[Service]\nType=oneshot\nExecStart=/bin/false\n")
	b := systemd.Open()
	defer b.Close()
	n := mustName("sentinel-itest-fails.service")
	job, err := restart(b, n)
	if err != nil || job == 0 {
		t.Fatalf("Restart = %d, %v", job, err)
	}
	st := eventually(t, b, n, func(s systemd.UnitState) bool { return s.Job != job })
	if st.Active != systemd.ActiveFailed {
		t.Errorf("after a failed start: %+v", st)
	}
}

// Mode fail: a stop job queued by an administrator is not replaced; the
// refusal says so and the stop goes on.
func TestRestartConflictsWithQueuedStop(t *testing.T) {
	install(t, "sentinel-itest-slowstop.service", "[Service]\nExecStart=/bin/sleep infinity\nExecStop=/bin/sleep 5\n")
	systemctl(t, "start", "sentinel-itest-slowstop.service")
	b := systemd.Open()
	defer b.Close()
	n := mustName("sentinel-itest-slowstop.service")
	eventually(t, b, n, func(s systemd.UnitState) bool { return s.Active == systemd.ActiveActive })
	systemctl(t, "stop", "--no-block", "sentinel-itest-slowstop.service")
	stop := eventually(t, b, n, func(s systemd.UnitState) bool { return s.Job != 0 })
	_, err := restart(b, n)
	var re *systemd.RefusedError
	if !errors.As(err, &re) || re.Code != systemd.RefusedConflict {
		t.Fatalf("Restart = %v, want refused %s", err, systemd.RefusedConflict)
	}
	if st := eventually(t, b, n, func(systemd.UnitState) bool { return true }); st.Job != stop.Job {
		t.Errorf("the stop job was replaced: %+v", st)
	}
	eventually(t, b, n, func(s systemd.UnitState) bool { return s.Active == systemd.ActiveInactive })
}

// Refusals before anything is queued, with their codes.
func TestRestartRefused(t *testing.T) {
	install(t, "sentinel-itest-manual.service", "[Unit]\nRefuseManualStart=yes\n[Service]\nExecStart=/bin/sleep infinity\n")
	// mask --runtime links the name to /dev/null in /run/systemd/system;
	// no unit file may be there (review 3c-2 R9).
	systemctl(t, "mask", "--runtime", "sentinel-itest-masked.service")
	t.Cleanup(func() {
		_ = exec.Command("systemctl", "unmask", "--runtime", "sentinel-itest-masked.service").Run() // best effort
		_ = exec.Command("systemctl", "daemon-reload").Run()                                        // forget it
	})
	b := systemd.Open()
	defer b.Close()
	for name, code := range map[string]string{
		"sentinel-itest-manual.service": systemd.RefusedManual,
		"sentinel-itest-masked.service": systemd.RefusedMasked,
	} {
		_, err := restart(b, mustName(name))
		var re *systemd.RefusedError
		if !errors.As(err, &re) || re.Code != code {
			t.Errorf("%s: Restart = %v, want refused %s", name, err, code)
		}
	}
}

// A unit at its start limit is not restarted, and its limit is not reset:
// systemd refuses, or queues a job that fails ("Start request repeated
// too quickly"; the unit keeps its last Result). Either way the program
// never runs a second time (review 3c-2 R10, P6).
func TestRestartAtStartLimit(t *testing.T) {
	runs := filepath.Join(t.TempDir(), "runs")
	install(t, "sentinel-itest-limit.service", "[Unit]\nStartLimitIntervalSec=1h\nStartLimitBurst=1\n"+
		"[Service]\nType=oneshot\nExecStart=/bin/sh -c 'echo run >> "+runs+"; exit 1'\n")
	_ = exec.Command("systemctl", "start", "sentinel-itest-limit.service").Run() // fails, as it should: the burst is spent
	b := systemd.Open()
	defer b.Close()
	n := mustName("sentinel-itest-limit.service")
	job, err := restart(b, n)
	var re *systemd.RefusedError
	switch {
	case errors.As(err, &re):
		t.Logf("at the start limit: refused (%s)", re.Code)
	case err == nil:
		st := eventually(t, b, n, func(s systemd.UnitState) bool { return s.Job != job })
		if st.Active == systemd.ActiveActive {
			t.Fatalf("restarted past its start limit: %+v", st)
		}
		t.Logf("at the start limit: job %d (error %v) ended, unit %s, result %s", job, err, st.Active, st.Result)
	default:
		t.Fatalf("Restart = %v", err)
	}
	if b, _ := os.ReadFile(runs); strings.Count(string(b), "run") != 1 {
		t.Errorf("the program ran %d times: the start limit was bypassed", strings.Count(string(b), "run"))
	}
}

// The premise of counting systemd's restarts apart (D-095 (4)):
// NRestarts rises with each automatic restart, and a manual restart
// (sentineld's) sets it back to 0 (review 3c-2 panel S1).
func TestNRestartsCountsAutomaticRestarts(t *testing.T) {
	install(t, "sentinel-itest-flap.service",
		"[Service]\nExecStart=/bin/sh -c 'sleep 1; exit 1'\nRestart=always\nRestartSec=100ms\n"+
			"[Unit]\nStartLimitIntervalSec=0\n")
	systemctl(t, "start", "sentinel-itest-flap.service")
	b := systemd.Open()
	defer b.Close()
	n := mustName("sentinel-itest-flap.service")
	before := eventually(t, b, n, func(s systemd.UnitState) bool { return s.NRestarts >= 3 })
	job, err := restart(b, n)
	if err != nil || job == 0 {
		t.Fatalf("Restart = %d, %v", job, err)
	}
	after := eventually(t, b, n, func(s systemd.UnitState) bool {
		return s.Job != job && s.InvocationID != before.InvocationID
	})
	if after.NRestarts >= before.NRestarts {
		t.Errorf("NRestarts %d before the manual restart, %d after: not reset", before.NRestarts, after.NRestarts)
	}
	t.Logf("NRestarts %d before the manual restart, %d after", before.NRestarts, after.NRestarts)
}
