package executor

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/clock"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/platform/privilege"
)

// runParentHelper plays sentineld: it starts a child through an Executor,
// prints the child's pid and waits to be killed.
func runParentHelper() {
	self, _ := os.Executable()
	e := New(clock.Real())
	p, err := e.Start(context.Background(), Spec{Path: self, Env: []string{helperEnv + "=sleep"}})
	if err != nil {
		os.Exit(92)
	}
	fmt.Printf("child=%d\n", p.Pid())
	waitForever()
}

// R-005, D-084 (2): when sentineld dies (here, SIGKILLed), its direct
// children die with it: Pdeathsig, set from the executor's locked thread.
func TestChildDiesWithSentineld(t *testing.T) {
	parent := exec.Command(helperPath(t))
	parent.Env = []string{helperEnv + "=parent"}
	stdout, err := parent.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		_ = parent.Process.Kill()
		t.Fatalf("parent helper: %v", err)
	}
	child, _ := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(line), "child="))
	if child <= 0 || !alive(child) {
		t.Fatalf("child %q not running", line)
	}
	_ = parent.Process.Kill()
	_ = parent.Wait()
	awaitGone(t, child)
}

// Without the capability to change identity, the start fails with a
// permission error (D-084: classified, never run as sentineld instead).
func TestCredentialsNeedPrivilege(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root may change identity; covered by tests/integration/executor")
	}
	spec := helper(t, "exit:0", nil)
	spec.Cred = &privilege.Cred{UID: 0, GID: 0}
	_, err := newExecutor(t).Start(context.Background(), spec)
	if !errors.Is(err, fs.ErrPermission) {
		t.Errorf("err = %v, want a permission error", err)
	}
}

// The forking thread outlives a burst of starts from many goroutines:
// every child still has a live parent-death relationship (none died).
func TestConcurrentStarts(t *testing.T) {
	e := newExecutor(t)
	procs := make(chan *Process, 16)
	for range 16 {
		go func() {
			p, err := e.Start(context.Background(), helper(t, "sleep", nil))
			if err != nil {
				t.Error(err)
			}
			procs <- p
		}()
	}
	for range 16 {
		p := <-procs
		if p == nil {
			continue
		}
		time.Sleep(10 * time.Millisecond)
		select {
		case <-p.Done():
			t.Errorf("child ended early: %v", p.Exit())
		default:
		}
		if _, err := p.Stop(context.Background(), syscall.SIGTERM, time.Minute); err != nil {
			t.Error(err)
		}
	}
}
