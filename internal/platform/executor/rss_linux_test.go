package executor

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/clock"
)

// D-084 (7) on a real kernel: the leader's resident memory includes what
// it wrote, and an executor of another boot samples nothing.
func TestLeaderRSSOfAChild(t *testing.T) {
	e := New(clock.Real())
	defer e.Close()
	r, w := io.Pipe()
	p, err := e.Start(context.Background(), helper(t, "hog:32", w))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = p.Stop(context.Background(), syscall.SIGKILL, time.Second) }()
	if line, err := bufio.NewReader(r).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("helper: %q, %v", line, err)
	}
	rss, err := e.LeaderRSS(p.Group())
	if err != nil {
		t.Fatal(err)
	}
	if rss < 32<<20 {
		t.Errorf("rss = %d, want at least 32 MiB", rss)
	}
	other := p.Group()
	other.Boot = "00000000-0000-0000-0000-000000000000"
	if _, err := e.LeaderRSS(other); !errors.Is(err, ErrGone) {
		t.Errorf("another boot: %v, want ErrGone", err)
	}
}

// A /proc/<pid> directory opened before the process ended never reads
// another process: once the leader is reaped every read through it is
// ErrGone, whoever holds the pid by then (no pid reuse is needed to prove
// it: the descriptor is bound to the process, not to the number).
func TestLeaderRSSPinnedAfterReap(t *testing.T) {
	e := New(clock.Real())
	defer e.Close()
	p, err := e.Start(context.Background(), helper(t, "sleep", nil))
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot("/proc/" + strconv.Itoa(p.Pid()))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if _, err := leaderRSS(root.FS(), p.Group(), uint64(os.Getpagesize())); err != nil {
		t.Fatalf("while running: %v", err)
	}
	if _, err := p.Stop(context.Background(), syscall.SIGKILL, time.Second); err != nil {
		t.Fatal(err)
	}
	<-p.Done() // reaped
	if _, err := leaderRSS(root.FS(), p.Group(), uint64(os.Getpagesize())); !errors.Is(err, ErrGone) {
		t.Errorf("after the reap: %v, want ErrGone", err)
	}
	if _, err := e.LeaderRSS(p.Group()); !errors.Is(err, ErrGone) {
		t.Errorf("LeaderRSS after the reap: %v, want ErrGone", err)
	}
}
