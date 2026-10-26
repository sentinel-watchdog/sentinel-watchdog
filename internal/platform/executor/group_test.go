package executor

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/clock"
)

// statLine is a /proc/<pid>/stat line with the fields cleanup reads.
func statLine(pid int, comm string, state byte, pgrp, sid int, start uint64) string {
	// pid (comm) state ppid pgrp session tty tpgid flags minflt cminflt
	// majflt cmajflt utime stime cutime cstime priority nice threads
	// itrealvalue starttime vsize
	return fmt.Sprintf("%d (%s) %c 1 %d %d 0 -1 4194560 1 0 0 0 0 0 0 0 20 0 1 0 %d 1000\n",
		pid, comm, state, pgrp, sid, start)
}

func TestParseStat(t *testing.T) {
	tests := []struct {
		name string
		line string
		want stat
		ok   bool
	}{
		{"plain", statLine(42, "worker", 'S', 40, 7, 1234), stat{pid: 42, pgrp: 40, sid: 7, state: 'S', start: 1234}, true},
		// A program chooses its own name: ") 1 2 3" must not shift fields.
		{"name with parens", statLine(42, "x) R 1 99 99 (y", 'S', 40, 7, 5), stat{pid: 42, pgrp: 40, sid: 7, state: 'S', start: 5}, true},
		{"name with spaces", statLine(42, "a b  c", 'Z', 40, 7, 5), stat{pid: 42, pgrp: 40, sid: 7, state: 'Z', start: 5}, true},
		{"no name", "42 S 1 40 7", stat{}, false},
		{"truncated", "42 (w) S 1 40 7 0", stat{}, false},
		{"pid zero", statLine(0, "w", 'S', 40, 7, 5), stat{}, false},
		{"pid overflow", strings.Replace(statLine(1, "w", 'S', 40, 7, 5), "1 (", "99999999999 (", 1), stat{}, false},
		{"negative group", statLine(42, "w", 'S', -1, 7, 5), stat{}, false},
		{"start overflow", strings.Replace(statLine(42, "w", 'S', 40, 7, 5), " 5 1000", " 99999999999999999999999 1000", 1), stat{}, false},
		{"long state", strings.Replace(statLine(42, "w", 'S', 40, 7, 5), ") S ", ") SS ", 1), stat{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseStat([]byte(tt.line))
			if (err == nil) != tt.ok || got != tt.want {
				t.Errorf("parseStat = %+v, %v; want %+v, ok %v", got, err, tt.want, tt.ok)
			}
		})
	}
}

// FuzzParseStat: any comm and any bytes give a stat or an error, never a
// panic, and an accepted line has a valid pid (AGENTS.md: every parser of
// untrusted input has a Fuzz target; comm is chosen by any program).
func FuzzParseStat(f *testing.F) {
	f.Add([]byte(statLine(42, "worker", 'S', 40, 7, 1234)))
	f.Add([]byte(statLine(42, ") ) (", 'Z', 40, 7, 1)))
	f.Add([]byte("1 ("))
	f.Fuzz(func(t *testing.T, b []byte) {
		s, err := parseStat(b)
		if err == nil && (s.pid <= 0 || s.pgrp < 0 || s.sid < 0) {
			t.Fatalf("accepted %+v", s)
		}
	})
}

// instant is a clock whose timers fire at once: cleanup's pauses are not
// what these tests are about.
type instant struct{ clock.Clock }

func (instant) NewTimer(time.Duration) clock.Timer { return firedTimer{} }

type firedTimer struct{}

func (firedTimer) C() <-chan time.Time {
	c := make(chan time.Time, 1)
	c <- time.Time{}
	return c
}

func (firedTimer) Stop() bool { return false }

// fakeProc is a /proc whose processes die when killed, unless stubborn.
type fakeProc struct {
	fsys     fstest.MapFS
	killed   []int
	stubborn map[int]bool
	// onKill runs after each kill, to make the table change (a fork).
	onKill func(pid int)
}

func newFakeProc(lines ...string) *fakeProc {
	p := &fakeProc{fsys: fstest.MapFS{}, stubborn: map[int]bool{}}
	for _, l := range lines {
		p.add(l)
	}
	return p
}

func (p *fakeProc) add(line string) {
	var pid int
	_, _ = fmt.Sscanf(line, "%d", &pid)
	p.fsys[fmt.Sprintf("%d/stat", pid)] = &fstest.MapFile{Data: []byte(line)}
}

func (p *fakeProc) view() procView {
	return procView{fsys: p.fsys, clock: instant{}, kill: func(pid int, still func() bool) error {
		if !still() {
			return nil
		}
		p.killed = append(p.killed, pid)
		if !p.stubborn[pid] {
			delete(p.fsys, fmt.Sprintf("%d/stat", pid))
		}
		if p.onKill != nil {
			p.onKill(pid)
		}
		return nil
	}}
}

const boot = "6a3c8f1e-2b4d-4e5f-8a9b-0c1d2e3f4a5b"

// The recorded group: leader 100 in session 50, started at tick 1000.
var rec = Group{Pgid: 100, Sid: 50, Start: 1000, Boot: boot}

func TestCleanup(t *testing.T) {
	tests := []struct {
		name       string
		procs      []string
		group      Group
		stubborn   []int
		wantKilled []int
		wantErr    bool
	}{
		{"leftovers of a dead leader are killed", []string{
			statLine(1, "init", 'S', 1, 1, 1),
			statLine(101, "leaf", 'S', 100, 50, 1001),
			statLine(102, "leaf", 'S', 100, 50, 1500),
		}, rec, nil, []int{101, 102}, false},
		{"a leader still alive is killed too", []string{
			statLine(100, "worker", 'S', 100, 50, 1000),
			statLine(101, "leaf", 'S', 100, 50, 1001),
		}, rec, nil, []int{100, 101}, false},
		{"another boot: nothing", []string{
			statLine(101, "leaf", 'S', 100, 50, 1001),
		}, Group{Pgid: 100, Sid: 50, Start: 1000, Boot: "other"}, nil, nil, false},
		{"pid reused by a live process: nothing", []string{
			statLine(100, "other", 'S', 100, 100, 9000),
			statLine(101, "leaf", 'S', 100, 50, 1001),
		}, rec, nil, nil, false},
		// C1: the group id reused, during sentineld's downtime, by a group
		// whose own leader also exited: its members are in another session.
		{"reused leaderless group in another session: nothing", []string{
			statLine(101, "stranger", 'S', 100, 77, 2000),
		}, rec, nil, nil, false},
		{"older than the leader: not a member", []string{
			statLine(101, "old", 'S', 100, 50, 999),
		}, rec, nil, nil, false},
		{"zombie: already gone", []string{
			statLine(101, "leaf", 'Z', 100, 50, 1001),
		}, rec, nil, nil, false},
		{"empty record: nothing", []string{
			statLine(101, "leaf", 'S', 0, 0, 1001),
		}, Group{}, nil, nil, false},
		{"member that survives every round", []string{
			statLine(101, "leaf", 'D', 100, 50, 1001),
		}, rec, []int{101}, slices.Repeat([]int{101}, cleanupRounds), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newFakeProc(tt.procs...)
			for _, pid := range tt.stubborn {
				p.stubborn[pid] = true
			}
			err := p.view().cleanup(context.Background(), tt.group, boot)
			if (err != nil) != tt.wantErr || (err != nil && !errors.Is(err, ErrCleanupIncomplete)) {
				t.Errorf("err = %v, want error %v", err, tt.wantErr)
			}
			slices.Sort(p.killed)
			if !slices.Equal(p.killed, tt.wantKilled) {
				t.Errorf("killed %v, want %v", p.killed, tt.wantKilled)
			}
		})
	}
}

// A member that forks while being killed: the next round finds the new
// process and kills it too.
func TestCleanupCatchesForks(t *testing.T) {
	p := newFakeProc(statLine(101, "leaf", 'S', 100, 50, 1001))
	forked := false
	p.onKill = func(int) {
		if !forked {
			forked = true
			p.add(statLine(105, "fork", 'S', 100, 50, 1100))
		}
	}
	if err := p.view().cleanup(context.Background(), rec, boot); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.killed, []int{101, 105}) {
		t.Errorf("killed %v", p.killed)
	}
}

// A process that changed between the scan and the kill (it exited and its
// pid now names another program) is not signalled.
func TestCleanupRechecksBeforeKill(t *testing.T) {
	p := newFakeProc(statLine(101, "leaf", 'S', 100, 50, 1001))
	v := p.view()
	inner := v.kill
	v.kill = func(pid int, still func() bool) error {
		p.add(statLine(101, "newcomer", 'S', 101, 101, 5000))
		return inner(pid, still)
	}
	if err := v.cleanup(context.Background(), rec, boot); err != nil {
		t.Fatal(err)
	}
	if len(p.killed) != 0 {
		t.Errorf("killed %v", p.killed)
	}
}

// A pid whose stat names another pid is refused: the entry is not what
// its directory says.
func TestReadChecksPid(t *testing.T) {
	p := newFakeProc()
	p.fsys["101/stat"] = &fstest.MapFile{Data: []byte(statLine(7, "w", 'S', 100, 50, 1001))}
	if _, err := p.view().read(101); err == nil {
		t.Error("accepted a stat of another pid")
	}
	p.fsys["102/stat"] = &fstest.MapFile{Data: []byte(strings.Repeat("x", maxStatBytes+1))}
	if _, err := p.view().read(102); err == nil {
		t.Error("accepted an oversized stat")
	}
	if _, err := p.view().read(103); !errors.Is(err, ErrGone) {
		t.Errorf("missing entry: %v, want ErrGone", err)
	}
}

// An entry that cannot be parsed makes the scan incomplete: cleanup never
// reports success for what it could not read.
func TestCleanupUnreadable(t *testing.T) {
	p := newFakeProc(statLine(101, "leaf", 'S', 100, 50, 1001))
	p.fsys["103/stat"] = &fstest.MapFile{Data: []byte("garbage")}
	if err := p.view().cleanup(context.Background(), rec, boot); !errors.Is(err, ErrCleanupIncomplete) {
		t.Errorf("err = %v", err)
	}
	if len(p.killed) != 0 {
		t.Errorf("killed %v before the scan was complete", p.killed)
	}
}

// Cancellation ends a cleanup with ErrCleanupIncomplete.
func TestCleanupCancelled(t *testing.T) {
	p := newFakeProc(statLine(101, "leaf", 'S', 100, 50, 1001))
	p.stubborn[101] = true
	v := p.view()
	v.clock = clock.NewFake(time.Unix(0, 0)) // timers never fire
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := v.cleanup(ctx, rec, boot); !errors.Is(err, ErrCleanupIncomplete) || !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
}
