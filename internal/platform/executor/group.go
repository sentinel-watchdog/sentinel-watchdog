package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strconv"
	"syscall"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/clock"
)

// Group identifies the process group of a started process, so that a
// later run of sentineld can kill what is left of it (D-084 (2)): after a
// SIGKILL of sentineld outside systemd, Pdeathsig ends the leader only,
// and its descendants stay in the group.
//
// A group id alone could name another group by then. Members must also
// be in the leader's session and not older than the leader, on the same
// boot. Linux never hands out an id still used as a group or session id,
// so while one member lives neither can be reused; a group and a session
// both reused while sentineld was down, by unrelated programs, would still
// match (a residual documented in D-086).
type Group struct {
	Pgid int // the leader's pid
	Sid  int // the leader's session
	// Start is the leader's start time in clock ticks after boot.
	Start uint64
	// Boot is the kernel's boot id: start times count from it.
	Boot string
}

// IsZero reports whether g records nothing (outside Linux, or no process).
func (g Group) IsZero() bool { return g == Group{} }

// Cleanup bounds (D-086): a member that forks while being killed is caught
// by the next round.
const (
	cleanupRounds = 5
	cleanupPause  = 100 * time.Millisecond
	maxStatBytes  = 4 << 10
)

// stat is what is read of /proc/<pid>/stat.
type stat struct {
	pid, pgrp, sid int
	state          byte
	start          uint64
}

// gone reports whether the process has ended: a zombie is only waiting to
// be reaped by its parent and cannot be killed further.
func (s stat) gone() bool { return s.state == 'Z' || s.state == 'X' }

// member reports whether s belongs to the recorded group g.
func (s stat) member(g Group) bool {
	return s.pgrp == g.Pgid && s.sid == g.Sid && s.start >= g.Start && !s.gone()
}

// parseStat reads the fields of a /proc/<pid>/stat line. The command name
// is between the first '(' and the last ')': it may itself hold spaces
// and parentheses, and any program can choose it.
func parseStat(b []byte) (stat, error) {
	open := bytes.IndexByte(b, '(')
	closing := bytes.LastIndexByte(b, ')')
	if open < 1 || closing < open {
		return stat{}, errors.New("stat: no command name")
	}
	var s stat
	pid, err := strconv.ParseUint(string(bytes.TrimSpace(b[:open])), 10, 31)
	if err != nil || pid == 0 {
		return stat{}, errors.New("stat: invalid pid")
	}
	s.pid = int(pid)
	// After the name: state ppid pgrp session tty_nr tpgid flags minflt
	// cminflt majflt cmajflt utime stime cutime cstime priority nice
	// num_threads itrealvalue starttime ... (proc_pid_stat(5)).
	fields := bytes.Fields(b[closing+1:])
	if len(fields) < 20 {
		return stat{}, errors.New("stat: too few fields")
	}
	if len(fields[0]) != 1 {
		return stat{}, errors.New("stat: invalid state")
	}
	s.state = fields[0][0]
	ids := [2]*int{&s.pgrp, &s.sid}
	for i, dst := range ids {
		v, err := strconv.ParseUint(string(fields[2+i]), 10, 31)
		if err != nil {
			return stat{}, errors.New("stat: invalid group or session id")
		}
		*dst = int(v)
	}
	if s.start, err = strconv.ParseUint(string(fields[19]), 10, 64); err != nil {
		return stat{}, errors.New("stat: invalid start time")
	}
	return s, nil
}

// procView is the part of /proc that a cleanup reads and the signal it
// sends. The variation it isolates is the real system versus the tables
// of the tests, so the decisions are tested on every platform.
type procView struct {
	fsys fs.FS // rooted at /proc
	// kill sends SIGKILL to pid if still holds just before the signal
	// (the caller re-reads the process), and reports a process already
	// gone as nil.
	kill  func(pid int, still func() bool) error
	clock clock.Clock
}

// errGone reports a process that ended while it was being read.
var errGone = errors.New("process gone")

// read returns the stat of pid. A process that ended meanwhile is errGone.
func (v procView) read(pid int) (stat, error) {
	f, err := v.fsys.Open(strconv.Itoa(pid) + "/stat")
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			return stat{}, errGone
		}
		return stat{}, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxStatBytes+1))
	switch {
	case errors.Is(err, syscall.ESRCH):
		return stat{}, errGone
	case err != nil:
		return stat{}, err
	case len(b) > maxStatBytes:
		return stat{}, errors.New("stat: too long")
	case len(b) == 0: // the process ended between open and read
		return stat{}, errGone
	}
	s, err := parseStat(b)
	if err != nil {
		return stat{}, err
	}
	if s.pid != pid {
		return stat{}, errors.New("stat: pid does not match its directory")
	}
	return s, nil
}

// members lists the live members of g. reused reports that a live process
// that is not the recorded leader holds the leader's pid: the ids now
// belong to someone else.
func (v procView) members(g Group) (pids []int, reused bool, err error) {
	entries, err := fs.ReadDir(v.fsys, ".")
	if err != nil {
		return nil, false, err
	}
	for _, e := range entries {
		pid, convErr := strconv.Atoi(e.Name())
		if convErr != nil || pid <= 0 {
			continue // not a process directory
		}
		s, err := v.read(pid)
		if errors.Is(err, errGone) {
			continue
		}
		if err != nil {
			return nil, false, fmt.Errorf("process %d: %w", pid, err)
		}
		if pid == g.Pgid && !s.gone() && (s.start != g.Start || s.sid != g.Sid) {
			return nil, true, nil
		}
		if s.member(g) {
			pids = append(pids, pid)
		}
	}
	return pids, false, nil
}

// cleanup kills every live member of g, recorded on boot, in rounds until
// a full scan finds none. Another boot, or ids now used by someone else,
// leave nothing to do.
func (v procView) cleanup(ctx context.Context, g Group, boot string) error {
	if g.IsZero() || g.Boot != boot {
		return nil
	}
	for round := 0; ; round++ {
		pids, reused, err := v.members(g)
		switch {
		case err != nil:
			return fmt.Errorf("%w: %w", ErrCleanupIncomplete, err)
		case reused || len(pids) == 0:
			return nil
		case round == cleanupRounds:
			return fmt.Errorf("%w: %d processes still alive", ErrCleanupIncomplete, len(pids))
		}
		for _, pid := range pids {
			still := func() bool {
				s, err := v.read(pid)
				return err == nil && s.member(g)
			}
			if err := v.kill(pid, still); err != nil {
				return fmt.Errorf("%w: kill %d: %w", ErrCleanupIncomplete, pid, err)
			}
		}
		timer := v.clock.NewTimer(cleanupPause)
		select {
		case <-timer.C():
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("%w: %w", ErrCleanupIncomplete, ctx.Err())
		}
	}
}
