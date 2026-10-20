//go:build integration && linux

// Package executor_test runs the executor as root on a real Linux system
// (task test-vm, D-056): identity switching, supplementary groups and the
// parent-death signal across an identity change. The suite fails, not
// skips, without root: it is run on purpose.
package executor_test

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/clock"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/platform/executor"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/platform/privilege"
)

const helperEnv = "SENTINEL_ITEST_HELPER"

// nobody is the identity the children switch to.
var nobody = privilege.Cred{UID: 65534, GID: 65534}

func TestMain(m *testing.M) {
	switch os.Getenv(helperEnv) {
	case "":
	case "ids":
		groups, _ := syscall.Getgroups()
		fmt.Printf("uid=%d gid=%d groups=%v\n", os.Getuid(), os.Getgid(), groups)
		os.Exit(0)
	case "sleep":
		for {
			time.Sleep(time.Hour)
		}
	case "parent":
		runParent()
	default:
		os.Exit(91)
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "tests/integration/executor needs root (run it with task test-vm)")
		os.Exit(1)
	}
	syscall.Umask(0o022)
	os.Exit(m.Run())
}

// helperBin is a copy of the test binary that nobody may execute: the
// staging directory of test-vm is private to the user who copied it.
var helperBin = sync.OnceValues(func() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp("", "sentinel-itest-bin")
	if err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		return "", err
	}
	data, err := os.ReadFile(self)
	if err != nil {
		return "", err
	}
	bin := filepath.Join(dir, "helper")
	return bin, os.WriteFile(bin, data, 0o755)
})

func spec(t *testing.T, mode string, out io.Writer) executor.Spec {
	t.Helper()
	bin, err := helperBin()
	if err != nil {
		t.Fatal(err)
	}
	return executor.Spec{Path: bin, Env: []string{helperEnv + "=" + mode}, Stdout: out}
}

func run(t *testing.T, s executor.Spec) {
	t.Helper()
	e := executor.New(clock.Real())
	defer e.Close()
	p, err := e.Start(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.Done():
	case <-time.After(15 * time.Second):
		t.Fatal("the process did not end")
	}
}

// T-15: a child switched to another user runs with that uid and gid and
// none of root's supplementary groups.
func TestRunsAsAnotherUser(t *testing.T) {
	var out bytes.Buffer
	s := spec(t, "ids", &out)
	s.Cred = &nobody
	run(t, s)
	if got := strings.TrimSpace(out.String()); got != "uid=65534 gid=65534 groups=[]" {
		t.Errorf("child identity %q", got)
	}
}

// T-15: a child that keeps root's identity still drops root's
// supplementary groups.
func TestRootChildDropsGroups(t *testing.T) {
	if err := syscall.Setgroups([]int{4, 27}); err != nil { // give the test process some groups
		t.Fatal(err)
	}
	var out bytes.Buffer
	run(t, spec(t, "ids", &out))
	if got := strings.TrimSpace(out.String()); got != "uid=0 gid=0 groups=[]" {
		t.Errorf("child identity %q", got)
	}
}

// Configured supplementary groups are set exactly.
func TestConfiguredGroups(t *testing.T) {
	var out bytes.Buffer
	s := spec(t, "ids", &out)
	s.Cred = &privilege.Cred{UID: 65534, GID: 65534, Groups: []uint32{100}}
	run(t, s)
	if got := strings.TrimSpace(out.String()); got != "uid=65534 gid=65534 groups=[100]" {
		t.Errorf("child identity %q", got)
	}
}

// runParent plays sentineld: it starts a child as nobody, prints its pid
// and waits to be killed.
func runParent() {
	self, _ := os.Executable()
	e := executor.New(clock.Real())
	p, err := e.Start(context.Background(), executor.Spec{Path: self, Env: []string{helperEnv + "=sleep"}, Cred: &nobody})
	if err != nil {
		fmt.Println("error:", err)
		os.Exit(92)
	}
	fmt.Printf("child=%d\n", p.Pid())
	for {
		time.Sleep(time.Hour)
	}
}

// R-005: the parent-death signal survives the identity change (Go sets it
// after setuid): the child of a SIGKILLed sentineld dies.
func TestChildOfAnotherUserDiesWithSentineld(t *testing.T) {
	bin, err := helperBin()
	if err != nil {
		t.Fatal(err)
	}
	parent := exec.Command(bin)
	parent.Env = []string{helperEnv + "=parent"}
	stdout, err := parent.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(stdout).ReadString('\n')
	child, _ := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(line), "child="))
	if child <= 0 {
		_ = parent.Process.Kill()
		t.Fatalf("parent helper said %q", line)
	}
	_ = parent.Process.Kill()
	_ = parent.Wait()
	for deadline := time.Now().Add(10 * time.Second); alive(child); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			_ = syscall.Kill(child, syscall.SIGKILL)
			t.Fatalf("child %d outlived its parent", child)
		}
	}
}

// Root stops a group owned by another user.
func TestStopsAnotherUsersGroup(t *testing.T) {
	e := executor.New(clock.Real())
	defer e.Close()
	s := spec(t, "sleep", nil)
	s.Cred = &nobody
	p, err := e.Start(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	ex, err := p.Stop(context.Background(), syscall.SIGTERM, 5*time.Second)
	if err != nil || ex.Signal != "SIGTERM" {
		t.Errorf("Stop = %v, %v", ex, err)
	}
}

func alive(pid int) bool {
	if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
		return false
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	i := bytes.LastIndexByte(data, ')')
	return i < 0 || i+2 >= len(data) || data[i+2] != 'Z'
}
