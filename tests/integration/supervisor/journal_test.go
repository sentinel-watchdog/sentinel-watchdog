//go:build integration && linux

package supervisor_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/platform/journald"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/api"
)

// forgeEnv names the source a forged entry claims.
const forgeEnv = "SENTINEL_ITEST_FORGE"

// forge sends, as whoever runs it, an entry that claims to be a program's
// output, with a _UID of root that journald must ignore.
func forge() int {
	entry := journald.Encode([]journald.Field{{Name: "MESSAGE", Value: "forged-line"},
		{Name: "SENTINEL_SOURCE", Value: os.Getenv(forgeEnv)}, {Name: "SENTINEL_STREAM", Value: "stdout"}})
	// The protocol's own form, so that the forged _UID reaches journald.
	entry = append(entry, "_UID=0\n"...)
	if err := journald.Send(journald.Socket, entry); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

var runIDRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

// journalFields returns the entries of source sent by uid, as journalctl
// prints them: a direct read, apart from sentineld's reader.
func journalFields(t *testing.T, source string, uid int) []map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "journalctl", "--no-pager", "--output=json",
		"SENTINEL_SOURCE="+source, fmt.Sprintf("_UID=%d", uid)).Output()
	if err != nil {
		t.Fatalf("journalctl: %v", err)
	}
	var entries []map[string]any
	for line := range strings.Lines(string(out)) {
		var e map[string]any
		if json.Unmarshal([]byte(line), &e) == nil {
			entries = append(entries, e)
		}
	}
	return entries
}

// startUnitDaemon runs the daemon as a transient systemd unit, so that its
// stderr is journald's stream, as under sentineld.service. The binary is
// copied where a unit may execute it (SELinux refuses one in /tmp).
func (s setup) startUnitDaemon(t *testing.T) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	unit := fmt.Sprintf("sentinel-itest-daemon-%d", os.Getpid())
	bin := filepath.Join("/usr/local/bin", unit)
	if err := os.WriteFile(bin, data, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			out, _ := exec.Command("journalctl", "--no-pager", "-o", "cat", "-u", unit).CombinedOutput()
			t.Logf("the daemon's journal:\n%s", out)
		}
		_ = exec.Command("systemctl", "stop", unit).Run()
		_ = exec.Command("systemctl", "reset-failed", unit).Run()
		_ = os.Remove(bin)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemd-run", "--quiet", "--unit="+unit,
		"--setenv="+modeEnv+"=daemon", "--setenv="+configEnv+"="+s.config, "--setenv=PATH=/usr/bin:/bin", bin).CombinedOutput()
	if err != nil {
		t.Fatalf("systemd-run: %v\n%s", err, out)
	}
	eventually(t, "the control socket", func() bool {
		_, err := os.Stat(filepath.Join(s.dir, "run", "s.sock"))
		return err == nil
	})
}

// Criteria 13 and 14 on a real journald, with the real daemon under systemd:
// a service's lines on both streams, redacted, with their fields; two runs of a
// job, each line with its run id; an entry forged by another user is stored
// under that user's uid and never shown.
func TestLogOutputThroughTheJournal(t *testing.T) {
	if !hasSystemd() {
		t.Skip("no systemd")
	}
	s := newSetup(t)
	if err := os.Remove(filepath.Join(s.dir, "etc", "supervisor", "worker.yaml")); err != nil {
		t.Fatal(err)
	}
	// Names of this invocation only: the journal keeps the lines of earlier
	// runs, and a reused name shows them (a documented residual, D-100).
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	talker, ticker := "talker-"+suffix, "ticker-"+suffix
	writeFile(t, filepath.Join(s.dir, "etc", "supervisor", "out.yaml"), "version: 1\nservices:\n"+
		"  - name: "+talker+"\n    script: \"echo out-1; echo err-1 >&2; echo key $API_TOKEN; "+
		"head -c 5000 /dev/zero | tr '\\\\0' x; echo; exec sleep 3600\"\n"+
		"    environment: {API_TOKEN: "+secret+"}\n    stop_timeout: 2s\n"+
		"jobs:\n  - name: "+ticker+"\n    schedule: \"* * * * *\"\n    script: \"echo tick; echo tick-err >&2\"\n")
	s.startUnitDaemon(t)

	// The forged entry exists, under its own uid, before it is shown absent.
	// s.bin is a copy nobody may execute; the test binary's directory is
	// private.
	for _, name := range []string{talker, ticker} {
		cmd := exec.Command(s.bin)
		cmd.Env = []string{modeEnv + "=forge", forgeEnv + "=" + name}
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 65534, Gid: 65534}}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("forge: %v\n%s", err, out)
		}
		eventually(t, "the forged entry under uid 65534", func() bool { return len(journalFields(t, name, 65534)) == 1 })
	}
	// A root program printing the field's text on its stdout reaches the journal
	// through the stream transport, which cannot set fields.
	streamUnit := "sentinel-itest-stream-" + suffix
	if out, err := exec.Command("systemd-run", "--quiet", "--wait", "--unit="+streamUnit,
		"/bin/sh", "-c", "echo SENTINEL_SOURCE="+talker).CombinedOutput(); err != nil {
		t.Fatalf("systemd-run: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("systemctl", "reset-failed", streamUnit).Run() })
	eventually(t, "the stream entry", func() bool {
		out, _ := exec.Command("journalctl", "--no-pager", "-o", "cat", "-u", streamUnit).Output()
		return strings.Contains(string(out), "SENTINEL_SOURCE="+talker)
	})

	eventually(t, "the service's lines through the daemon", func() bool {
		r, _ := s.logsOf(t, talker)
		return len(r.Lines) >= 3
	})
	r, code := s.logsOf(t, talker)
	if code != "" || r.Source != api.LogSourceJournal {
		t.Fatalf("code %q, %+v", code, r)
	}
	texts := logTexts(r)
	for _, want := range []string{"out-1", "err-1", "key [REDACTED]"} {
		if !slices.Contains(texts, want) {
			t.Errorf("lines %q lack %q", texts, want)
		}
	}
	joined := strings.Join(texts, "\n")
	if strings.Contains(joined, secret) || strings.Contains(joined, "forged-line") || strings.Contains(joined, "SENTINEL_SOURCE=") {
		t.Errorf("lines:\n%s", joined)
	}
	// Review 3c-5b R2: a line cut at 4 KiB reads back, not as journalctl's null.
	long := slices.IndexFunc(texts, func(l string) bool { return strings.HasPrefix(l, "xxxx") })
	if long < 0 || !strings.HasSuffix(texts[long], "[line cut]") {
		t.Errorf("no cut line of x in %d lines", len(texts))
	}
	streams := map[string]string{}
	for _, e := range journalFields(t, talker, 0) {
		msg, _ := e["MESSAGE"].(string)
		streams[msg], _ = e["SENTINEL_STREAM"].(string)
		if id, _ := e["SYSLOG_IDENTIFIER"].(string); id != "sentineld/"+talker {
			t.Errorf("identifier %q", id)
		}
	}
	if streams["out-1"] != "stdout" || streams["err-1"] != "stderr" {
		t.Errorf("streams %v", streams)
	}

	// Two runs of the job, each a line on each stream: up to two minutes.
	var lines map[string][]string // run id → its lines
	for deadline := time.Now().Add(150 * time.Second); len(lines) < 2; time.Sleep(time.Second) {
		if time.Now().After(deadline) {
			t.Fatalf("job lines %v", lines)
		}
		j, code := s.logsOf(t, ticker)
		if code != "" {
			continue
		}
		lines = map[string][]string{}
		for _, l := range j.Lines {
			if !runIDRe.MatchString(l.RunID) || l.Text == "forged-line" {
				t.Fatalf("job line %+v", l)
			}
			lines[l.RunID] = append(lines[l.RunID], l.Text)
		}
	}
	for id, texts := range lines {
		slices.Sort(texts)
		if !slices.Equal(texts, []string{"tick", "tick-err"}) {
			t.Errorf("run %s: lines %q", id, texts)
		}
	}
}

// Criterion 16 on a real system: a daemon whose stderr is not journald's
// leaves log output in its own log, and cannot read it back.
func TestLogOutputWithoutTheJournal(t *testing.T) {
	s := newSetup(t)
	writeFile(t, filepath.Join(s.dir, "etc", "supervisor", "out.yaml"), "version: 1\nservices:\n"+
		"  - name: talker\n    script: \"echo out-1; exec sleep 3600\"\n    stop_timeout: 2s\n")
	s.startDaemon(t)
	eventually(t, "the control socket", func() bool {
		_, err := os.Stat(filepath.Join(s.dir, "run", "s.sock"))
		return err == nil
	})
	if _, code := s.logsOf(t, "talker"); code != api.CodeUnavailable {
		t.Errorf("code %q, want unavailable", code)
	}
}
