package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/config"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/module"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/daemon"
)

// helperEnv makes the test binary act as sentineld (TestMain), so tests
// can send real signals to a real process.
const helperEnv = "SENTINELD_TEST_HELPER"

func TestMain(m *testing.M) {
	switch os.Getenv(helperEnv) {
	case "run":
		os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
	case "slowstart", "hungstart":
		modules = slowModules(os.Getenv(helperEnv) == "hungstart")
		os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
	case "umask":
		run([]string{"-version"}, io.Discard, io.Discard)
		fmt.Printf("%04o\n", syscall.Umask(0))
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// writeConfig writes a central file whose state directory is inside the
// test's temporary directory, and returns its path and the state path.
func writeConfig(t *testing.T, extra string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	body := fmt.Sprintf("version: 1\ndaemon:\n  state_dir: %s\n  shutdown_timeout: 5s\n%s", stateDir, extra)
	path := filepath.Join(dir, "sentinel.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, stateDir
}

func runCmd(args ...string) (int, string, string) {
	var stdout, stderr strings.Builder
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestVersion(t *testing.T) {
	code, out, _ := runCmd("-version")
	if code != exitOK || !strings.HasPrefix(out, "sentineld ") {
		t.Errorf("exit %d, output %q", code, out)
	}
}

func TestUsage(t *testing.T) {
	for _, args := range [][]string{{"-nope"}, {"extra"}} {
		if code, _, _ := runCmd(args...); code != exitUsage {
			t.Errorf("%v: exit %d, want %d", args, code, exitUsage)
		}
	}
}

// -validate reports every problem, exits 1 on an invalid configuration
// and changes nothing on disk.
func TestValidate(t *testing.T) {
	tests := []struct {
		name  string
		extra string
		code  int
		want  []string
	}{
		{"valid", "", exitOK, []string{"configuration is valid"}},
		{"two problems", "notifications:\n  core: [missing]\nmodules:\n  firewall: {enabled: true}\n", exitFailure,
			[]string{"error: ", "missing", "firewall"}},
		{"planned module", "modules:\n  supervisor: {enabled: true}\n", exitFailure, []string{"supervisor"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path, stateDir := writeConfig(t, tt.extra)
			code, out, _ := runCmd("-validate", "-config", path)
			if code != tt.code {
				t.Errorf("exit %d, want %d; output:\n%s", code, tt.code, out)
			}
			for _, w := range tt.want {
				if !strings.Contains(out, w) {
					t.Errorf("output lacks %q:\n%s", w, out)
				}
			}
			if _, err := os.Stat(stateDir); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("-validate created the state directory: %v", err)
			}
		})
	}
}

func TestInvalidConfigurationRefusesToStart(t *testing.T) {
	path, stateDir := writeConfig(t, "modules:\n  firewall: {enabled: true}\n")
	code, _, errOut := runCmd("-config", path)
	if code != exitFailure || !strings.Contains(errOut, "firewall") {
		t.Errorf("exit %d, stderr:\n%s", code, errOut)
	}
	if _, err := os.Stat(stateDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("an invalid configuration created the state directory: %v", err)
	}
}

// startHelper runs the test binary as sentineld and waits until it logs
// that it is ready.
func startHelper(t *testing.T, args ...string) (*exec.Cmd, *bufio.Scanner) {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), helperEnv+"=run")
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	lines := bufio.NewScanner(stderr)
	ready := make(chan bool, 1)
	go func() {
		for lines.Scan() {
			if strings.Contains(lines.Text(), "sentineld ready") {
				ready <- true
				return
			}
		}
		ready <- false
	}()
	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("sentineld exited before it was ready")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("sentineld not ready after 30s")
	}
	return cmd, lines
}

// SIGTERM and SIGINT stop the daemon cleanly: exit 0 after "stopped".
func TestSignalsStopTheDaemon(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT} {
		t.Run(sig.String(), func(t *testing.T) {
			path, stateDir := writeConfig(t, "")
			cmd, lines := startHelper(t, "-config", path)
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			stopped := false
			for lines.Scan() {
				stopped = stopped || strings.Contains(lines.Text(), "sentineld stopped")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("exit: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("sentineld did not exit after the signal")
			}
			if !stopped {
				t.Error("shutdown not logged")
			}
			if info, err := os.Stat(stateDir); err != nil || info.Mode().Perm() != 0o750 {
				t.Errorf("state directory: %v, mode %v", err, info.Mode().Perm())
			}
		})
	}
}

// The daemon sets its own umask whatever it inherits.
func TestUmaskIsRestrictive(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", `umask 000; exec "$0"`, os.Args[0])
	cmd.Env = append(os.Environ(), helperEnv+"=umask")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out)); got != fmt.Sprintf("%04o", umask) {
		t.Errorf("umask %s, want %04o", got, umask)
	}
}

// slowStart is a test module whose Start announces itself on stderr and
// waits for ctx (or, with ignoreCtx, forever).
type slowStart struct {
	name      string
	ignoreCtx bool
}

func (m slowStart) Name() string { return m.name }

func (m slowStart) Configure(config.ModuleConfig) (module.Configured, error) { return m, nil }

func (m slowStart) Start(ctx context.Context, _ module.Runtime) error {
	fmt.Fprintln(os.Stderr, "entered start", m.name)
	if m.ignoreCtx {
		select {}
	}
	<-ctx.Done()
	return nil
}

func (slowStart) Stop(context.Context) error { return nil }

// slowModules registers two slow test modules, "a" before "b".
func slowModules(ignoreCtx bool) func() (*module.Registry, error) {
	return func() (*module.Registry, error) {
		reg, err := daemon.Modules()
		if err != nil {
			return nil, err
		}
		for _, name := range []string{"a", "b"} {
			if err := reg.Register(func() module.Module { return slowStart{name: name, ignoreCtx: ignoreCtx} }); err != nil {
				return nil, err
			}
		}
		return reg, nil
	}
}

// Review 2c-1 R4: a signal during start lets the starting module return and
// starts no other; a second signal ends a start that ignores the first, at
// once (the process dies of SIGTERM, well before the 5s start timeout).
func TestSignalDuringStart(t *testing.T) {
	tests := []struct {
		name    string
		mode    string
		signals int
	}{
		{"first signal cancels start", "slowstart", 1},
		{"second signal ends the process", "hungstart", 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path, _ := writeConfig(t, "modules:\n  a: {enabled: true}\n  b: {enabled: true}\n")
			cmd := exec.Command(os.Args[0], "-config", path)
			cmd.Env = append(os.Environ(), helperEnv+"="+tt.mode)
			stderr, err := cmd.StderrPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cmd.Process.Kill() })
			lines := make(chan string, 64)
			go func() {
				defer close(lines)
				for sc := bufio.NewScanner(stderr); sc.Scan(); {
					lines <- sc.Text()
				}
			}()
			var out []string
			timeout := time.After(30 * time.Second)
			for entered := false; !entered; {
				select {
				case line, ok := <-lines:
					if !ok {
						t.Fatalf("sentineld exited before starting a:\n%s", strings.Join(out, "\n"))
					}
					out = append(out, line)
					entered = strings.Contains(line, "entered start a")
				case <-timeout:
					t.Fatal("module a never started")
				}
			}
			for range tt.signals {
				if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
					t.Fatal(err)
				}
				time.Sleep(100 * time.Millisecond) // let the first signal be handled
			}
			exited := make(chan error, 1)
			go func() {
				for line := range lines {
					out = append(out, line)
				}
				exited <- cmd.Wait()
			}()
			select {
			case err = <-exited:
			case <-time.After(3 * time.Second): // shorter than the 5s start timeout
				t.Fatal("sentineld did not exit promptly after the signal")
			}
			log := strings.Join(out, "\n")
			if strings.Contains(log, "entered start b") {
				t.Errorf("b started after the signal:\n%s", log)
			}
			if tt.signals == 1 {
				if err != nil {
					t.Errorf("exit: %v\n%s", err, log)
				}
				return
			}
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Fatalf("exit %v, want death by SIGTERM", err)
			}
			if status, ok := exitErr.Sys().(syscall.WaitStatus); !ok || !status.Signaled() || status.Signal() != syscall.SIGTERM {
				t.Errorf("exit %v, want death by SIGTERM", exitErr)
			}
		})
	}
}
