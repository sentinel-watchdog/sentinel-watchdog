// Package executor runs the programs the supervisor owns (D-005, D-084):
// one program per Spec, exec'd directly with an explicit environment, in
// its own process group, stopped with a signal to the whole group and then
// SIGKILL. It is a platform adapter; the decisions (when to start, restart
// or stop) belong to the supervisor.
//
// On Linux every child also gets a parent-death signal (SIGKILL), so it
// does not outlive sentineld. The kernel ties that signal to the thread
// that forked the child, so every child is started from one goroutine
// locked to its thread for the executor's lifetime.
package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/clock"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/platform/privilege"
)

// drainTimeout bounds how long a process's output is read after its group
// is gone: a descendant that escaped the group may keep the pipe open.
const drainTimeout = 2 * time.Second

// Spec describes one program to run. Every field is the caller's
// validated configuration: nothing is looked up in $PATH or inherited
// from sentineld.
type Spec struct {
	// Path is the absolute path of the executable (for a script, the
	// shell, with Args ["-c", script]).
	Path string
	Args []string // argv[1:]
	// Dir is the absolute working directory; "" means "/".
	Dir string
	// Env is the complete environment, "KEY=value"; nil means empty, never
	// sentineld's own (which holds notification secrets).
	Env []string
	// Cred is the identity to run as; nil keeps sentineld's. When
	// sentineld is root, the child's supplementary groups are always set
	// to Cred.Groups (or none), never inherited (threat model T-15).
	Cred *privilege.Cred
	// Stdout and Stderr receive the output; nil sends it to /dev/null.
	// Each is written by one goroutine, until the process group is gone.
	// Write should return promptly: after a write error the rest of the
	// stream is discarded (OutputErr), so the program never blocks on a
	// full pipe, and a Write still blocked twice the drain timeout after
	// the group is gone is abandoned (it ends when Write returns).
	Stdout, Stderr io.Writer
}

// Exit is how a process ended.
type Exit struct {
	// Code is the exit status, or -1 when a signal ended the process.
	Code int
	// Signal names the signal that ended the process ("SIGKILL"), if any.
	Signal string
}

// String describes the exit for logs: "exit status 3", "signal SIGTERM".
func (e Exit) String() string {
	if e.Signal != "" {
		return "signal " + e.Signal
	}
	return fmt.Sprintf("exit status %d", e.Code)
}

// ErrClosed is returned by Start once the executor is closed.
var ErrClosed = errors.New("executor: closed")

// Executor starts processes. Its zero value is not usable; New starts the
// goroutine that forks every child, and Close ends it.
type Executor struct {
	clock clock.Clock
	reqs  chan startRequest
	done  chan struct{}
	close sync.Once
}

type startRequest struct {
	cmd   *exec.Cmd
	reply chan error
}

// New returns an executor. Its forking goroutine runs until Close; the
// processes it started are not affected by Close.
func New(c clock.Clock) *Executor {
	e := &Executor{clock: c, reqs: make(chan startRequest), done: make(chan struct{})}
	go e.forker()
	return e
}

// forker forks every child from one locked thread (see the package
// documentation). It unlocks the thread before returning, so the thread
// stays alive in the runtime's pool instead of exiting with the
// goroutine, which would kill every child started from it.
func (e *Executor) forker() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	for {
		select {
		case req := <-e.reqs:
			req.reply <- req.cmd.Start()
		case <-e.done:
			return
		}
	}
}

// Close ends the forking goroutine. Start then returns ErrClosed.
func (e *Executor) Close() { e.close.Do(func() { close(e.done) }) }

// Start runs spec. The process belongs to the caller, who must Stop it or
// see it end (Done). Errors from the system match fs.ErrNotExist (no such
// executable or directory) or fs.ErrPermission (not allowed) with
// errors.Is; their text may name the path.
func (e *Executor) Start(ctx context.Context, spec Spec) (*Process, error) {
	if err := spec.check(); err != nil {
		return nil, err
	}
	attr, err := sysProcAttr(spec.Cred)
	if err != nil {
		return nil, err
	}
	stdout, err := newOutput(spec.Stdout)
	if err != nil {
		return nil, err
	}
	stderr, err := newOutput(spec.Stderr)
	if err != nil {
		stdout.abort()
		return nil, err
	}
	cmd := &exec.Cmd{
		Path:        spec.Path,
		Args:        append([]string{spec.Path}, spec.Args...),
		Dir:         orDefault(spec.Dir, "/"),
		Env:         cmpEnv(spec.Env),
		SysProcAttr: attr,
	}
	// Only a real file: a nil *os.File in the io.Writer would be a non-nil
	// interface, and exec would close the descriptor instead of opening
	// /dev/null (review R1).
	if stdout.child != nil {
		cmd.Stdout = stdout.child
	}
	if stderr.child != nil {
		cmd.Stderr = stderr.child
	}
	reply := make(chan error, 1)
	select {
	case e.reqs <- startRequest{cmd: cmd, reply: reply}:
	case <-e.done:
		stdout.abort()
		stderr.abort()
		return nil, ErrClosed
	case <-ctx.Done():
		stdout.abort()
		stderr.abort()
		return nil, ctx.Err()
	}
	if err := <-reply; err != nil {
		stdout.abort()
		stderr.abort()
		return nil, fmt.Errorf("executor: start: %w", err)
	}
	stdout.started()
	stderr.started()
	p := &Process{pid: cmd.Process.Pid, clock: e.clock, done: make(chan struct{}), outs: []*output{stdout, stderr}}
	go p.reap(cmd)
	return p, nil
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// cmpEnv returns env, never nil: exec.Cmd treats a nil Env as "inherit".
func cmpEnv(env []string) []string {
	if env == nil {
		return []string{}
	}
	return slices.Clone(env)
}

// check refuses what the configuration layer should already have refused:
// a relative path would be resolved against Dir, and an environment entry
// without "=" or with NUL is undefined.
func (s Spec) check() error {
	switch {
	case !filepath.IsAbs(s.Path):
		return errors.New("executor: the executable path must be absolute")
	case s.Dir != "" && !filepath.IsAbs(s.Dir):
		return errors.New("executor: the working directory must be absolute")
	}
	for _, kv := range s.Env {
		if k, _, ok := strings.Cut(kv, "="); !ok || k == "" || strings.ContainsRune(kv, 0) {
			return errors.New("executor: invalid environment entry")
		}
	}
	return nil
}

// Process is a running program and its process group (pgid = pid).
type Process struct {
	pid   int
	clock clock.Clock
	done  chan struct{}
	exit  Exit // set before done is closed
	outs  []*output
}

// OutputErr returns the first error of an output sink, if any: from then
// on the stream was discarded.
func (p *Process) OutputErr() error {
	for _, o := range p.outs {
		if err := o.err(); err != nil {
			return err
		}
	}
	return nil
}

// Pid returns the process id, which is also its process group id.
func (p *Process) Pid() int { return p.pid }

// Done is closed when the process has ended, its group has been killed and
// its output read.
func (p *Process) Done() <-chan struct{} { return p.done }

// Exit returns how the process ended; valid once Done is closed.
func (p *Process) Exit() Exit {
	<-p.done
	return p.exit
}

// reap is the only goroutine that waits for the process. Once the leader
// has exited it kills what is left of the group (a crashed program's
// children must not run beside its replacement), then reads the rest of
// the output.
//
// The group is signalled just after the leader is reaped. Its id cannot be
// reused while any member lives; if none does, it could in principle be
// reused in that instant by a new group, a window documented in D-084.
func (p *Process) reap(cmd *exec.Cmd) {
	err := cmd.Wait()
	_ = killGroup(p.pid, syscall.SIGKILL) // ESRCH: the group is already empty
	p.exit = exitOf(cmd.ProcessState, err)
	for _, o := range p.outs {
		o.drain(p.clock, drainTimeout)
	}
	close(p.done)
}

// Stop sends sig to the process group, waits up to timeout (on the
// executor's clock), then sends SIGKILL to the group and waits for the
// process to end or ctx to end. It returns how the process ended; when ctx
// ends first the process may still be running and Stop returns ctx's
// error. Stopping a process that has ended returns its exit.
func (p *Process) Stop(ctx context.Context, sig syscall.Signal, timeout time.Duration) (Exit, error) {
	select {
	case <-p.done:
		return p.exit, nil
	default:
	}
	_ = killGroup(p.pid, sig) // ESRCH: ended meanwhile; done will say so
	timer := p.clock.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-p.done:
		return p.exit, nil
	case <-timer.C():
		_ = killGroup(p.pid, syscall.SIGKILL)
	case <-ctx.Done():
		_ = killGroup(p.pid, syscall.SIGKILL)
		return Exit{}, ctx.Err()
	}
	select {
	case <-p.done:
		return p.exit, nil
	case <-ctx.Done():
		return Exit{}, ctx.Err()
	}
}

// killGroup signals every process of group pgid.
func killGroup(pgid int, sig syscall.Signal) error {
	return syscall.Kill(-pgid, sig)
}

func exitOf(ps *os.ProcessState, waitErr error) Exit {
	if ps == nil {
		return Exit{Code: -1, Signal: ""}
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return Exit{Code: -1, Signal: signalName(ws.Signal())}
	}
	_ = waitErr // an *exec.ExitError for a non-zero status, described by ps
	return Exit{Code: ps.ExitCode()}
}
