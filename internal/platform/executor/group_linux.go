package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
)

// bootIDPath names the current boot (a UUID, random at each boot).
const bootIDPath = "/proc/sys/kernel/random/boot_id"

// bootID returns the kernel's boot id.
func bootID() (string, error) {
	b, err := os.ReadFile(bootIDPath)
	if err != nil {
		return "", fmt.Errorf("executor: read boot id: %w", err)
	}
	id := strings.TrimSpace(string(b))
	if len(id) != 36 {
		return "", errors.New("executor: invalid boot id")
	}
	return id, nil
}

// systemProc is the real /proc, with signals sent through a process
// handle: os.FindProcess holds a pidfd when the kernel gives one (Linux
// 5.3 and later), so the signal cannot reach a process that reused the
// pid after the re-check. Without a pidfd (older kernels, no descriptor
// left) a short window between the re-check and the signal remains
// (D-086).
func (e *Executor) systemProc() procView {
	return procView{fsys: os.DirFS("/proc"), clock: e.clock, kill: func(pid int, still func() bool) error {
		p, err := os.FindProcess(pid)
		if err != nil {
			return err
		}
		defer func() { _ = p.Release() }() // only closes the handle
		if !still() {
			return nil
		}
		if err := p.Signal(syscall.SIGKILL); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return err
		}
		return nil
	}}
}

// captureGroup records the group of the process pid has just started.
// The process is a child that has not been reaped yet, so its /proc entry
// exists even if it has already exited.
func (e *Executor) captureGroup(pid int) (Group, error) {
	s, err := e.systemProc().read(pid)
	if err != nil {
		return Group{}, fmt.Errorf("executor: read the new process: %w", err)
	}
	if s.pgrp != pid {
		return Group{}, errors.New("executor: the new process is not its group's leader")
	}
	return Group{Pgid: pid, Sid: s.sid, Start: s.start, Boot: e.boot}, nil
}

// Cleanup kills what is left of a group recorded by an earlier Start,
// possibly by another run of sentineld on the same boot (D-084 (2)). It
// returns nil when nothing of the group is alive, and an error matching
// ErrCleanupIncomplete when it cannot tell, or members survived; ctx bounds
// it.
func (e *Executor) Cleanup(ctx context.Context, g Group) error {
	if err := e.bootErr; err != nil {
		return err
	}
	return e.systemProc().cleanup(ctx, g, e.boot)
}
