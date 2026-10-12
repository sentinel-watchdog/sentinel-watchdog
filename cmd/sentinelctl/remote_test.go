package main

import (
	"context"
	"encoding/json/v2"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/audit"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/authz"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/config"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/daemon"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/api"
)

// startDaemon runs an in-process daemon on a temporary socket. Its peers
// are faked as root: real credentials are Linux-only and covered by
// internal/platform/peercred.
func startDaemon(t *testing.T) string {
	t.Helper()
	return startDaemonWith(t, nil)
}

// startDaemonWith runs prepare on the state directory before the daemon
// starts.
func startDaemonWith(t *testing.T, prepare func(stateDir string)) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "swl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if prepare != nil {
		state := filepath.Join(dir, "state")
		if err := os.Mkdir(state, 0o750); err != nil {
			t.Fatal(err)
		}
		prepare(state)
	}
	cfg := &config.Config{Daemon: config.Daemon{
		Socket: filepath.Join(dir, "run", "s.sock"), SocketMode: 0o660,
		StateDir: filepath.Join(dir, "state"), ShutdownTimeout: config.Duration(5 * time.Second),
	}}
	d := daemon.New(cfg, nil, daemon.Options{
		Logger: slog.New(slog.DiscardHandler), Version: "9.9.9",
		Modules:         map[string]config.ModuleAvailability{"supervisor": config.ModulePlanned},
		PeerCredentials: func(*net.UnixConn) (authz.Peer, error) { return authz.Peer{UID: 0}, nil },
	})
	if err := d.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Stop(context.Background()) })
	return cfg.Daemon.Socket
}

func TestRemoteCommands(t *testing.T) {
	socket := startDaemon(t)
	tests := []struct {
		args   []string
		code   int
		stdout []string
		stderr string
	}{
		{[]string{"status", "-socket", socket}, exitOK, []string{"sentineld 9.9.9, up ", "MODULE", "supervisor", "planned", "CHANNEL", "audit chain: ok, last record 0"}, ""},
		{[]string{"audit", "-socket", socket}, exitOK, []string{"audit chain: ok", "SEQ"}, ""},
		{[]string{"audit", "-n", "0", "-socket", socket}, exitFailure, nil, "bad_request"},
		{[]string{"audit", "-n", "5000", "-socket", socket}, exitFailure, nil, "bad_request"},
		{[]string{"modules", "-socket", socket}, exitOK, []string{"MODULE", "supervisor  planned"}, ""},
		{[]string{"config", "show", "-socket", socket}, exitOK, []string{`"daemon": {`, `"socket": "` + socket}, ""},
		{[]string{"events", "-socket", socket}, exitFailure, nil, "Phase 3a"},
		{[]string{"status", "-socket", socket, "extra"}, exitUsage, nil, "unexpected argument"},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args[:1], " "), func(t *testing.T) {
			code, out, errOut := runCmd(tt.args...)
			if code != tt.code || !strings.Contains(errOut, tt.stderr) {
				t.Fatalf("exit %d (want %d)\nstdout: %s\nstderr: %s", code, tt.code, out, errOut)
			}
			for _, want := range tt.stdout {
				if !strings.Contains(out, want) {
					t.Errorf("stdout lacks %q:\n%s", want, out)
				}
			}
		})
	}
}

func TestRemoteJSON(t *testing.T) {
	socket := startDaemon(t)
	code, out, errOut := runCmd("status", "-json", "-socket", socket)
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	var st api.Status
	if err := json.Unmarshal([]byte(out), &st); err != nil || st.Version != "9.9.9" {
		t.Errorf("status %+v, %v\n%s", st, err, out)
	}
}

// The errors an operator meets get an explanation, not a raw system error.
func TestRemoteExplainsFailures(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "none.sock")
	if code, _, errOut := runCmd("status", "-socket", missing); code != exitFailure || !strings.Contains(errOut, "sentineld is not running") {
		t.Errorf("no daemon: exit %d, %s", code, errOut)
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores the socket's mode")
	}
	socket := startDaemon(t)
	if err := os.Chmod(socket, 0); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := runCmd("status", "-socket", socket); code != exitFailure || !strings.Contains(errOut, "no access to") {
		t.Errorf("no access: exit %d, %s", code, errOut)
	}
}

func TestPrintable(t *testing.T) {
	if got := printable("nginx\x1b[31m"); got != `"nginx\x1b[31m"` {
		t.Errorf("printable = %s", got)
	}
	if got := printable("nginx.service"); got != "nginx.service" {
		t.Errorf("printable = %s", got)
	}
}

// sentinelctl audit shows the last records and what a broken chain means.
func TestAuditCommand(t *testing.T) {
	socket := startDaemonWith(t, func(state string) {
		root, err := os.OpenRoot(state)
		if err != nil {
			t.Fatal(err)
		}
		l, err := audit.Open(root, audit.Options{Logger: slog.New(slog.DiscardHandler)})
		if err != nil {
			t.Fatal(err)
		}
		uid := uint32(1000)
		for _, r := range []audit.Result{audit.ResultDenied, audit.ResultIntent} {
			_, err := l.Record(context.Background(), audit.Entry{Actor: audit.Actor{Kind: audit.ActorSocket, UID: &uid, PID: 7},
				Action: "core.reload", Result: r, Detail: map[string]string{"tier": "read"}})
			if err != nil {
				t.Fatal(err)
			}
		}
		if err := l.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
	code, out, errOut := runCmd("audit", "-n", "1", "-socket", socket)
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	for _, want := range []string{"audit chain: ok, last record 2", "socket uid 1000 pid 7", "core.reload", "intent", "tier=read"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "denied") {
		t.Errorf("-n 1 printed more than one record:\n%s", out)
	}

	broken := startDaemonWith(t, func(state string) {
		if err := os.WriteFile(filepath.Join(state, "audit.jsonl"), []byte("tampered\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	if code, out, _ := runCmd("status", "-socket", broken); code != exitOK ||
		!strings.Contains(out, "audit chain: BROKEN (bad_record at audit.jsonl line 1): operate and admin requests are refused") {
		t.Errorf("status of a broken chain: exit %d\n%s", code, out)
	}
}
