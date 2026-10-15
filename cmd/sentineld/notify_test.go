package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/config"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/module"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/transport"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/daemon"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/platform/sdnotify"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/api"
)

// notifySocket stands in for systemd: a datagram socket at a short path
// (macOS limits socket paths to 104 bytes).
func notifySocket(t *testing.T) (*net.UnixConn, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "snt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "notify")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn, path
}

// datagram returns the next notification, or "" when none arrives within
// wait.
func datagram(t *testing.T, conn *net.UnixConn, wait time.Duration) string {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(wait)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(buf[:n])
}

// Under systemd: READY once the control socket answers, with a degraded
// status when a module failed (D-075 keeps the daemon running); nothing on
// reload (D-079); STOPPING on SIGTERM. The modules never see
// NOTIFY_SOCKET, so they cannot report state for sentineld.
func TestSystemdNotifications(t *testing.T) {
	conn, socket := notifySocket(t)
	path, _ := writeConfig(t, "modules:\n  probe: {enabled: true}\n")
	cmd, scanner, before := startHelperWith(t,
		[]string{helperEnv + "=probe", sdnotify.EnvSocket + "=" + socket}, "-config", path)
	lines := make(chan string, 64)
	go func() {
		defer close(lines)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
	}()

	ready := datagram(t, conn, 30*time.Second)
	if want := "READY=1\nSTATUS=ready, degraded: 1 of 1 modules not running (sentinelctl modules)"; ready != want {
		t.Fatalf("first notification %q, want %q", ready, want)
	}
	if !strings.Contains(strings.Join(before, "\n"), "probe "+sdnotify.EnvSocket+"=\"\"") {
		t.Errorf("the module saw %s:\n%s", sdnotify.EnvSocket, strings.Join(before, "\n"))
	}
	// Ready means served: a request gets an answer (on Linux the status; on
	// other systems "unavailable", which still proves the socket serves).
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := transport.Call(ctx, socketPath(t, path), api.Request{Version: api.Version, Command: api.CoreStatus.Name}); err != nil {
		t.Errorf("control socket after READY: %v", err)
	}

	if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	waitFor(t, lines, "configuration reloaded")
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if got, want := datagram(t, conn, 30*time.Second), "STOPPING=1\nSTATUS=stopping"; got != want {
		t.Errorf("after the reload and SIGTERM: %q, want %q (a reload sends nothing)", got, want)
	}
	for range lines { // drain, so that the process can exit
	}
	// The probe failed at start, which makes the stop unclean (D-075).
	if err := cmd.Wait(); cmd.ProcessState.ExitCode() != exitFailure {
		t.Errorf("exit: %v, want %d", err, exitFailure)
	}
}

// An invalid NOTIFY_SOCKET disables the notifications without stopping the
// daemon, and the log never shows the value (untrusted environment).
func TestInvalidNotifySocketIsIgnored(t *testing.T) {
	path, _ := writeConfig(t, "")
	const value = "relative/secret-notify-value"
	cmd, scanner, before := startHelperWith(t,
		[]string{helperEnv + "=run", sdnotify.EnvSocket + "=" + value}, "-config", path)
	log := strings.Join(before, "\n")
	if !strings.Contains(log, "systemd notifications disabled") || strings.Contains(log, "secret-notify-value") {
		t.Errorf("warning missing or echoing the value:\n%s", log)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	for scanner.Scan() {
		if strings.Contains(scanner.Text(), "secret-notify-value") {
			t.Errorf("logged the value: %s", scanner.Text())
		}
	}
	if err := cmd.Wait(); err != nil {
		t.Errorf("exit: %v", err)
	}
}

// A start that fails sends nothing: systemd sees the exit, never READY.
func TestNoReadyWhenStartFails(t *testing.T) {
	conn, socket := notifySocket(t)
	t.Setenv(sdnotify.EnvSocket, socket)
	path, stateDir := writeConfig(t, "")
	if err := os.WriteFile(stateDir, nil, 0o600); err != nil { // a file where the directory goes
		t.Fatal(err)
	}
	if code, _, errOut := runCmd("-config", path); code != exitFailure {
		t.Fatalf("exit %d, stderr:\n%s", code, errOut)
	}
	// run notifies synchronously: anything sent is already queued.
	if got := datagram(t, conn, 10*time.Millisecond); got != "" {
		t.Errorf("notification after a failed start: %q", got)
	}
}

// socketPath is the control socket that writeConfig put in the file.
func socketPath(t *testing.T, configFile string) string {
	t.Helper()
	return filepath.Join(filepath.Dir(configFile), "run", "s.sock")
}

// waitFor reads lines until one contains want.
func waitFor(t *testing.T, lines <-chan string, want string) {
	t.Helper()
	timeout := time.After(30 * time.Second)
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatalf("sentineld exited before logging %q", want)
			}
			if strings.Contains(line, want) {
				return
			}
		case <-timeout:
			t.Fatalf("no %q after 30s", want)
		}
	}
}

// probe is a test module that prints the NOTIFY_SOCKET it inherits and
// fails to start.
type probe struct{}

func (probe) Name() string { return "probe" }

func (p probe) Configure(config.ModuleConfig) (module.Configured, error) { return p, nil }

func (probe) Start(context.Context, module.Runtime) error {
	fmt.Fprintf(os.Stderr, "probe %s=%q\n", sdnotify.EnvSocket, os.Getenv(sdnotify.EnvSocket))
	return errors.New("probe fails on purpose")
}

func (probe) Stop(context.Context) error { return nil }

// probeModules registers the probe beside the production modules.
func probeModules() (*module.Registry, error) {
	reg, err := daemon.Modules()
	if err != nil {
		return nil, err
	}
	return reg, reg.Register(func() module.Module { return probe{} })
}
