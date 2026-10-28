package executor

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/clock"
)

// helperEnv selects what the re-executed test binary does (runHelper).
const helperEnv = "SENTINEL_EXEC_HELPER"

// TestMain runs the helper when the test binary is started by a test, and
// the tests otherwise, with umask 022: the output-file tests create files
// that the checks refuse when group-writable.
func TestMain(m *testing.M) {
	if mode := os.Getenv(helperEnv); mode != "" {
		runHelper(mode)
		os.Exit(0)
	}
	syscall.Umask(0o022)
	os.Exit(m.Run())
}

// runHelper is the child side of the tests. Every mode that waits does so
// until a signal ends it.
func runHelper(mode string) {
	self, _ := os.Executable()
	spawn := func(leafMode string, attr *syscall.SysProcAttr) int {
		p, err := os.StartProcess(self, []string{self}, &os.ProcAttr{
			Env: []string{helperEnv + "=" + leafMode}, Files: []*os.File{nil, os.Stdout, os.Stderr}, Sys: attr})
		if err != nil {
			os.Exit(90)
		}
		return p.Pid
	}
	switch {
	case mode == "env":
		fmt.Println(strings.Join(os.Environ(), "\n"))
	case mode == "ids":
		wd, _ := os.Getwd()
		fmt.Printf("pid=%d pgid=%d wd=%s\n", os.Getpid(), syscall.Getpgrp(), wd)
	case strings.HasPrefix(mode, "exit:"):
		code, _ := strconv.Atoi(strings.TrimPrefix(mode, "exit:"))
		os.Exit(code)
	case mode == "selfkill":
		// SIGQUIT and SIGUSR1 make Go programs exit with a stack dump
		// (status 2); SIGKILL cannot be handled.
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		waitForever()
	case mode == "lines":
		fmt.Print("one\ntwo\nthree")
	case mode == "sleep":
		waitForever()
	case strings.HasPrefix(mode, "hog:"):
		// Holds n MiB resident, every page written, and prints "ready".
		n, _ := strconv.Atoi(strings.TrimPrefix(mode, "hog:"))
		hog = make([]byte, n<<20)
		for i := range hog {
			hog[i] = 1
		}
		fmt.Println("ready")
		waitForever()
	case mode == "stubborn-leaf":
		signal.Ignore(syscall.SIGTERM)
		fmt.Printf("leaf=%d\n", os.Getpid())
		waitForever()
	case mode == "stubborn":
		// Ignores SIGTERM, with a grandchild in its group that ignores it too.
		signal.Ignore(syscall.SIGTERM)
		spawn("stubborn-leaf", nil)
		waitForever()
	case mode == "orphan-maker":
		// Leaves a grandchild in its group and exits.
		spawn("stubborn-leaf", nil)
		time.Sleep(200 * time.Millisecond) // let the leaf print its pid
	case mode == "escape":
		// Leaves a grandchild in a new session (outside the group) holding
		// stdout open, and exits.
		spawn("stubborn-leaf", &syscall.SysProcAttr{Setsid: true})
		time.Sleep(200 * time.Millisecond)
	case mode == "parent":
		runParentHelper()
	default:
		os.Exit(91)
	}
}

// hog keeps the memory of the "hog:" mode reachable.
var hog []byte

// waitForever blocks until a signal ends the helper. A bare select{} would
// be reported by the runtime as a deadlock (exit status 2).
func waitForever() {
	for {
		time.Sleep(time.Hour)
	}
}

func helperPath(t *testing.T) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return self
}

// helper returns a Spec running the helper in mode.
func helper(t *testing.T, mode string, out io.Writer) Spec {
	return Spec{Path: helperPath(t), Env: []string{helperEnv + "=" + mode}, Stdout: out}
}

func newExecutor(t *testing.T) *Executor {
	t.Helper()
	e := New(clock.Real())
	t.Cleanup(e.Close)
	return e
}

// syncBuffer collects output written by the copy goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitDone waits for p to end; the timeout is a safety net, not a timing
// assumption.
func waitDone(t *testing.T, p *Process) Exit {
	t.Helper()
	select {
	case <-p.Done():
		return p.Exit()
	case <-time.After(15 * time.Second):
		t.Fatal("the process did not end")
		return Exit{}
	}
}

// alive reports whether pid is a live process (not gone, not a zombie).
func alive(pid int) bool {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return false
	}
	if data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
		if i := bytes.LastIndexByte(data, ')'); i > 0 && i+2 < len(data) && data[i+2] == 'Z' {
			return false
		}
	}
	return true
}

// awaitGone waits until pid is no longer alive; the timeout is a safety net.
func awaitGone(t *testing.T, pid int) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); alive(pid); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("process %d is still alive", pid)
		}
	}
}

// leafPid reads the "leaf=<pid>" line a helper prints.
func leafPid(t *testing.T, out *syncBuffer) int {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		sc := bufio.NewScanner(strings.NewReader(out.String()))
		for sc.Scan() {
			if v, ok := strings.CutPrefix(sc.Text(), "leaf="); ok {
				pid, _ := strconv.Atoi(v)
				return pid
			}
		}
	}
	t.Fatalf("no leaf pid in %q", out.String())
	return 0
}

// D-084, threat model T-15: the child gets exactly the configured
// environment (never sentineld's, which holds notification secrets), the
// working directory, its own process group and /dev/null as stdin.
func TestStartIsolation(t *testing.T) {
	t.Setenv("SENTINEL_WEBHOOK_TOKEN", "parent-secret")
	e := newExecutor(t)
	var out syncBuffer
	spec := helper(t, "env", &out)
	spec.Env = append(spec.Env, "QUEUE=jobs")
	p, err := e.Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if ex := waitDone(t, p); ex.Code != 0 {
		t.Fatalf("exit %v", ex)
	}
	if got := strings.Fields(out.String()); len(got) != 2 || got[0] != helperEnv+"=env" || got[1] != "QUEUE=jobs" {
		t.Errorf("environment %q", got)
	}

	out = syncBuffer{}
	dir := t.TempDir()
	spec = helper(t, "ids", &out)
	spec.Dir = dir
	if p, err = e.Start(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	waitDone(t, p)
	resolved, _ := filepath.EvalSymlinks(dir)
	want := fmt.Sprintf("pid=%d pgid=%d wd=", p.Pid(), p.Pid())
	if got := out.String(); !strings.HasPrefix(got, want) || !strings.Contains(got, resolved) {
		t.Errorf("ids %q, want prefix %q and dir %s", got, want, resolved)
	}
}

// Without Dir the child runs in "/", not in sentineld's directory.
func TestDefaultDir(t *testing.T) {
	var out syncBuffer
	p, err := newExecutor(t).Start(context.Background(), helper(t, "ids", &out))
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, p)
	if !strings.HasSuffix(strings.TrimSpace(out.String()), "wd=/") {
		t.Errorf("ids %q", out.String())
	}
}

func TestExit(t *testing.T) {
	e := newExecutor(t)
	tests := []struct {
		mode string
		want Exit
	}{
		{"exit:0", Exit{Code: 0}},
		{"exit:3", Exit{Code: 3}},
		{"selfkill", Exit{Code: -1, Signal: "SIGKILL"}},
	}
	for _, tt := range tests {
		p, err := e.Start(context.Background(), helper(t, tt.mode, nil))
		if err != nil {
			t.Fatal(err)
		}
		if got := waitDone(t, p); got != tt.want {
			t.Errorf("%s: %v, want %v", tt.mode, got, tt.want)
		}
	}
	if s := (Exit{Code: -1, Signal: "SIGTERM"}).String(); s != "signal SIGTERM" {
		t.Error(s)
	}
}

// R-005: Stop sends the stop signal to the group and, past the timeout,
// SIGKILL to the whole group: a leader and a grandchild that both ignore
// SIGTERM are gone.
func TestStopEscalatesToTheGroup(t *testing.T) {
	var out syncBuffer
	p, err := newExecutor(t).Start(context.Background(), helper(t, "stubborn", &out))
	if err != nil {
		t.Fatal(err)
	}
	leaf := leafPid(t, &out)
	ex, err := p.Stop(context.Background(), syscall.SIGTERM, 300*time.Millisecond)
	if err != nil || ex.Signal != "SIGKILL" {
		t.Fatalf("Stop = %v, %v; want SIGKILL", ex, err)
	}
	awaitGone(t, leaf)
}

// A program that honours SIGTERM ends with it, before the timeout.
func TestStopGracefully(t *testing.T) {
	p, err := newExecutor(t).Start(context.Background(), helper(t, "sleep", nil))
	if err != nil {
		t.Fatal(err)
	}
	ex, err := p.Stop(context.Background(), syscall.SIGTERM, time.Minute)
	if err != nil || ex.Signal != "SIGTERM" {
		t.Fatalf("Stop = %v, %v", ex, err)
	}
	if again, err := p.Stop(context.Background(), syscall.SIGTERM, time.Minute); err != nil || again != ex {
		t.Errorf("second Stop = %v, %v", again, err)
	}
}

// Stop honours its context: a cancelled stop kills the group and returns.
func TestStopHonoursContext(t *testing.T) {
	var out syncBuffer
	p, err := newExecutor(t).Start(context.Background(), helper(t, "stubborn", &out))
	if err != nil {
		t.Fatal(err)
	}
	leaf := leafPid(t, &out)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Stop(ctx, syscall.SIGTERM, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("Stop err = %v", err)
	}
	waitDone(t, p)
	awaitGone(t, leaf)
}

// D-084 (2): when a leader exits on its own, what is left of its group is
// killed, so nothing of a crashed program runs beside its replacement.
func TestLeaderExitKillsTheGroup(t *testing.T) {
	var out syncBuffer
	p, err := newExecutor(t).Start(context.Background(), helper(t, "orphan-maker", &out))
	if err != nil {
		t.Fatal(err)
	}
	leaf := leafPid(t, &out)
	waitDone(t, p)
	awaitGone(t, leaf)
}

// A descendant that left the group keeps the output pipe open: Done still
// comes, after the drain timeout (documented residual of D-084).
func TestEscapedDescendantDoesNotBlockDone(t *testing.T) {
	var out syncBuffer
	p, err := newExecutor(t).Start(context.Background(), helper(t, "escape", &out))
	if err != nil {
		t.Fatal(err)
	}
	leaf := leafPid(t, &out)
	t.Cleanup(func() { _ = syscall.Kill(leaf, syscall.SIGKILL) })
	waitDone(t, p)
	if !alive(leaf) {
		t.Error("the escaped process was killed: the test does not show what it should")
	}
}

func TestOutputLines(t *testing.T) {
	var lines []string
	var mu sync.Mutex
	lw := NewLineWriter(clock.Real(), DefaultLineOptions, func(l string) { mu.Lock(); lines = append(lines, l); mu.Unlock() })
	p, err := newExecutor(t).Start(context.Background(), helper(t, "lines", lw))
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, p)
	_ = lw.Close()
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(lines, "|") != "one|two|three" {
		t.Errorf("lines %q", lines)
	}
}

func TestStartErrors(t *testing.T) {
	e := newExecutor(t)
	dir := t.TempDir()
	notExec := filepath.Join(dir, "script.sh")
	if err := os.WriteFile(notExec, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		spec Spec
		is   error
	}{
		{"relative path", Spec{Path: "bin/worker"}, nil},
		{"relative dir", Spec{Path: helperPath(t), Dir: "tmp"}, nil},
		{"bad environment", Spec{Path: helperPath(t), Env: []string{"NOEQUALS"}}, nil},
		{"NUL in environment", Spec{Path: helperPath(t), Env: []string{"A=b\x00c"}}, nil},
		{"missing executable", Spec{Path: filepath.Join(dir, "missing")}, fs.ErrNotExist},
		{"missing directory", Spec{Path: helperPath(t), Dir: filepath.Join(dir, "missing")}, fs.ErrNotExist},
		{"not executable", Spec{Path: notExec}, fs.ErrPermission},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := e.Start(context.Background(), tt.spec)
			if err == nil {
				waitDone(t, p)
				t.Fatal("started")
			}
			if tt.is != nil && !errors.Is(err, tt.is) {
				t.Errorf("err = %v, want %v", err, tt.is)
			}
		})
	}
}

func TestClosedExecutor(t *testing.T) {
	e := New(clock.Real())
	e.Close()
	e.Close() // idempotent
	if _, err := e.Start(context.Background(), helper(t, "exit:0", nil)); !errors.Is(err, ErrClosed) {
		t.Errorf("err = %v", err)
	}
}

func TestParseSignal(t *testing.T) {
	for _, s := range []string{"SIGTERM", "TERM", "term", "SIGINT", "SIGQUIT", "SIGHUP", "SIGUSR1"} {
		if _, ok := ParseSignal(s); !ok {
			t.Errorf("%s refused", s)
		}
	}
	for _, s := range []string{"", "SIG", "KILLALL", "9"} {
		if _, ok := ParseSignal(s); ok {
			t.Errorf("%s accepted", s)
		}
	}
}

// Full review 2026-10-28 N1 (and Codex C3): once reap has waited for the
// leader and killed its group, the group's number may be reused by an
// unrelated group while the output drains (up to two timeouts per stream).
// Stop must not signal that number any more. The test models the reuse by
// pointing the process at an unrelated group once reap is draining.
func TestStopDoesNotSignalAfterReap(t *testing.T) {
	start := func(mode string) *exec.Cmd {
		cmd := exec.Command(helperPath(t))
		cmd.Env = []string{helperEnv + "=" + mode}
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		return cmd
	}
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer write.Close()
	c := clock.NewFake(time.Unix(0, 0))
	leader := start("exit:0")
	copied := make(chan struct{})
	p := &Process{pid: leader.Process.Pid, clock: c, done: make(chan struct{}),
		outs: []*output{{read: read, copied: copied}}}
	go p.reap(leader)
	c.BlockUntilTimers(1) // reaped, group killed, now draining

	victim := start("sleep")
	ended := make(chan struct{})
	go func() { _ = victim.Wait(); close(ended) }()
	defer func() { _ = victim.Process.Kill(); <-ended }()
	p.pid = victim.Process.Pid // the old number now names another group

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = p.Stop(ctx, syscall.SIGTERM, time.Second)
	select {
	case <-ended:
		t.Error("Stop signalled the group number after the leader was reaped")
	case <-time.After(500 * time.Millisecond):
	}
	close(copied)
	<-p.Done()
}
