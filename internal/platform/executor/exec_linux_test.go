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

// The record of a started process matches what /proc says of it.
func TestGroupRecorded(t *testing.T) {
	e := newExecutor(t)
	p, err := e.Start(context.Background(), helper(t, "sleep", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = p.Stop(context.Background(), syscall.SIGKILL, time.Second) }()
	self, err := e.systemProc().read(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	g := p.Group()
	if g.Pgid != p.Pid() || g.Sid != self.sid || g.Start < self.start || g.Boot != e.boot || len(g.Boot) != 36 {
		t.Errorf("group = %+v, pid %d, own sid %d", g, p.Pid(), self.sid)
	}
}

// D-084 (2): what a dead leader left in its group is killed by Cleanup.
// The group is made with os/exec, not the executor, whose reaper would
// kill the leftovers itself and hide a Cleanup that does nothing; the
// leftover is checked alive before Cleanup runs.
func TestCleanupKillsLeftovers(t *testing.T) {
	e := newExecutor(t)
	leader := exec.Command(helperPath(t))
	leader.Env = []string{helperEnv + "=orphan-maker"}
	leader.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// A file, not a writer: Wait would otherwise wait for the copy of an
	// output the leftover keeps open.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	leader.Stdout = w
	err = leader.Start()
	_ = w.Close() // the children hold their copies
	if err != nil {
		t.Fatal(err)
	}
	s, err := e.systemProc().read(leader.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	g := Group{Pgid: leader.Process.Pid, Sid: s.sid, Start: s.start, Boot: e.boot}
	line, err := bufio.NewReader(r).ReadString('\n')
	leaf, _ := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(line), "leaf="))
	if err != nil || leaf <= 0 {
		t.Fatalf("leaf pid: %q, %v", line, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(leaf, syscall.SIGKILL) })
	_ = leader.Wait() // the leader exits by itself and is reaped
	if !alive(leaf) {
		t.Fatal("the leftover is not running: the test would prove nothing")
	}
	if err := e.Cleanup(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	awaitGone(t, leaf)
	// Nothing is left: a second cleanup succeeds at once.
	if err := e.Cleanup(context.Background(), g); err != nil {
		t.Errorf("second cleanup: %v", err)
	}
}

// A record from another boot, or naming another session, kills nothing,
// even when a live process matches the group id.
func TestCleanupSparesOthers(t *testing.T) {
	e := newExecutor(t)
	leader := exec.Command(helperPath(t))
	leader.Env = []string{helperEnv + "=stubborn"}
	leader.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := leader.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Kill(-leader.Process.Pid, syscall.SIGKILL); _ = leader.Wait() }()
	s, err := e.systemProc().read(leader.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range []Group{
		{Pgid: leader.Process.Pid, Sid: s.sid, Start: s.start, Boot: "00000000-0000-0000-0000-000000000000"},
		{Pgid: leader.Process.Pid, Sid: s.sid + 1, Start: s.start, Boot: e.boot},
	} {
		if err := e.Cleanup(context.Background(), g); err != nil {
			t.Errorf("%+v: %v", g, err)
		}
		if !alive(leader.Process.Pid) {
			t.Fatalf("%+v: an unrelated group was killed", g)
		}
	}
}
