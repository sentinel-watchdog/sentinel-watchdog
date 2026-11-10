//go:build integration && linux

package supervisor_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/api"
)

const (
	runtimeUnits = "/run/systemd/system"
	logsUnit     = "sentinel-itest-supervised.service"
	logsAlias    = "sentinel-itest-other-name.service"
	logsAlias2   = "sentinel-itest-third-name.service"
)

func hasSystemd() bool {
	fi, err := os.Stat(runtimeUnits)
	return err == nil && fi.IsDir()
}

func systemctl(t *testing.T, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "systemctl", args...).CombinedOutput(); err != nil {
		t.Fatalf("systemctl %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// logsOf asks the daemon for name's logs; code says how it refused, if it did.
func (s setup) logsOf(t *testing.T, name string) (res api.SupervisorLogsResult, code api.Code) {
	t.Helper()
	resp := s.call(t, api.SupervisorLogs.Name, `{"name":"`+name+`"}`)
	if resp.Error != nil {
		return res, resp.Error.Code
	}
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatal(err)
	}
	return res, ""
}

func logTexts(r api.SupervisorLogsResult) []string {
	out := make([]string, len(r.Lines))
	for i, l := range r.Lines {
		out[i] = l.Text
	}
	return out
}

// The whole path on a real system: the socket, the tier and its audit, the
// supervisor's source, the runner as nobody, a real journal; an alias reaches
// its unit's lines, through the Id an observation read (named) or one read for
// the request (aliased, disabled) (criteria 1, 2).
func TestLogsOfARealUnit(t *testing.T) {
	if !hasSystemd() {
		t.Skip("no systemd")
	}
	body := "[Service]\nType=oneshot\nRemainAfterExit=yes\nExecStart=/bin/sh -c 'echo first-line; echo token " + secret + " here; echo last-line'\n"
	path := filepath.Join(runtimeUnits, logsUnit)
	aliasPath, alias2Path := filepath.Join(runtimeUnits, logsAlias), filepath.Join(runtimeUnits, logsAlias2)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, link := range []string{aliasPath, alias2Path} {
		if err := os.Symlink(logsUnit, link); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_ = exec.Command("systemctl", "stop", logsUnit).Run() // best effort: the unit goes away below
		_ = os.Remove(aliasPath)
		_ = os.Remove(alias2Path)
		_ = os.Remove(path)
		_ = exec.Command("systemctl", "daemon-reload").Run()
	})
	systemctl(t, "daemon-reload")
	systemctl(t, "start", logsUnit)

	s := newSetup(t)
	writeFile(t, filepath.Join(s.dir, "etc", "supervisor", "units.yaml"), "version: 1\nservices:\n"+
		"  - {name: named, unit: "+logsAlias+", check_interval: 1s}\n"+
		"  - {name: aliased, unit: "+logsAlias2+", check_interval: 1s, enabled: false}\n")
	s.startDaemon(t)
	eventually(t, "the control socket", func() bool {
		_, err := os.Stat(filepath.Join(s.dir, "run", "s.sock"))
		return err == nil
	})
	eventually(t, "the unit's lines through the daemon", func() bool {
		r, _ := s.logsOf(t, "named")
		return slices.Contains(logTexts(r), "last-line")
	})
	r, code := s.logsOf(t, "named")
	if code != "" || r.Source != api.LogSourceJournal || r.Lines[0].Time.IsZero() {
		t.Errorf("code %q, %+v", code, r)
	}
	// The worker's environment holds the secret, so its value is masked here.
	if texts := strings.Join(logTexts(r), "\n"); strings.Contains(texts, secret) || !strings.Contains(texts, "token [REDACTED] here") {
		t.Errorf("lines:\n%s", texts)
	}
	got, code := s.logsOf(t, "aliased")
	if code != "" || !slices.Contains(logTexts(got), "last-line") {
		t.Errorf("a disabled alias: code %q, lines %q", code, logTexts(got))
	}

	resp := s.call(t, api.CoreAudit.Name, "")
	var audits api.AuditResult
	if resp.Error != nil || json.Unmarshal(resp.Result, &audits) != nil {
		t.Fatalf("core.audit: %+v", resp)
	}
	seen := false
	for _, rec := range audits.Records {
		seen = seen || (rec.Action == api.SupervisorLogs.Name && rec.Target == "aliased")
	}
	if !seen {
		t.Error("no audit record names the service")
	}
}

// Criterion 3 on a real system, and criterion 5: a file needs no journal, so
// it reads on Alpine too, where a unit's logs are unavailable.
func TestLogsOfARealFile(t *testing.T) {
	s := newSetup(t)
	writeFile(t, filepath.Join(s.dir, "etc", "supervisor", "units.yaml"), "version: 1\nservices:\n"+
		"  - {name: unit, unit: sentinel-itest-no-such-unit.service, check_interval: 1s}\n")
	s.startDaemon(t)
	eventually(t, "the worker's output file", func() bool {
		_, err := os.Stat(s.output)
		return err == nil
	})
	f, err := os.OpenFile(s.output, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("key " + secret + " end\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	r, code := s.logsOf(t, "worker")
	if code != "" || r.Source != api.LogSourceFile || !slices.Contains(logTexts(r), "key [REDACTED] end") {
		t.Errorf("code %q, %q", code, logTexts(r))
	}

	_, code = s.logsOf(t, "unit")
	switch {
	case !hasSystemd() && code != api.CodeUnavailable:
		t.Errorf("a unit without systemd: code %q", code)
	case hasSystemd() && code != "":
		t.Errorf("a unit with no lines: code %q", code)
	}

	// An output file that became a link is not followed by root's read.
	if err := os.Remove(s.output); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/shadow", s.output); err != nil {
		t.Fatal(err)
	}
	if r, code := s.logsOf(t, "worker"); code != api.CodeUnavailable {
		t.Errorf("a link to /etc/shadow: code %q, %q", code, logTexts(r))
	}
}
