package sdnotify

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// listen opens the datagram socket systemd would hold, at a path or an
// abstract name, and returns it with its address.
func listen(t *testing.T, abstract bool) (*net.UnixConn, string) {
	t.Helper()
	addr := filepath.Join(t.TempDir(), "n.sock")
	if abstract {
		addr = fmt.Sprintf("@sentinel-sdnotify-test-%d-%d", os.Getpid(), rand.Uint64())
	}
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: addr, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn, addr
}

// receive reads one datagram, or returns "" when none comes within wait.
func receive(t *testing.T, conn *net.UnixConn, wait time.Duration) string {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(wait)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(buf[:n])
}

// notifier parses addr with a short retry policy.
func notifier(t *testing.T, addr string, every, total time.Duration) *Notifier {
	t.Helper()
	n, err := Parse(addr)
	if err != nil {
		t.Fatal(err)
	}
	n.retryEvery, n.retryFor = every, total
	return n
}

// One notification is one datagram, at a path or at an abstract name
// (systemd uses both).
func TestNotifySendsOneDatagram(t *testing.T) {
	for _, abstract := range []bool{false, true} {
		t.Run(fmt.Sprintf("abstract=%v", abstract), func(t *testing.T) {
			conn, addr := listen(t, abstract)
			n := notifier(t, addr, time.Millisecond, time.Second)
			if err := n.Notify(context.Background(), Ready, Status("ready")); err != nil {
				t.Fatal(err)
			}
			if got, want := receive(t, conn, 10*time.Second), "READY=1\nSTATUS=ready"; got != want {
				t.Errorf("datagram %q, want %q", got, want)
			}
		})
	}
}

// A ctx already done sends nothing at all.
func TestNotifyWithDoneContextSendsNothing(t *testing.T) {
	conn, addr := listen(t, false)
	n := notifier(t, addr, time.Millisecond, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := n.Notify(ctx, Ready); !errors.Is(err, context.Canceled) {
		t.Fatalf("error %v, want context.Canceled", err)
	}
	if got := receive(t, conn, 50*time.Millisecond); got != "" {
		t.Errorf("sent %q after cancellation", got)
	}
}

func TestNotifyWithoutListenerFails(t *testing.T) {
	_, addr := listen(t, false)
	missing := filepath.Join(filepath.Dir(addr), "missing.sock")
	n := notifier(t, missing, time.Millisecond, time.Second)
	err := n.Notify(context.Background(), Ready)
	if !errors.Is(err, syscall.ENOENT) {
		t.Fatalf("error %v, want ENOENT", err)
	}
	if strings.Contains(err.Error(), filepath.Dir(addr)) {
		t.Errorf("error %q echoes the socket address", err)
	}
}

// fill sends until the receiver's queue is full and returns how many
// datagrams it took. The cap bounds the test whatever the kernel's limit
// (net.unix.max_dgram_qlen, 512 by default).
func fill(t *testing.T, addr string) int {
	t.Helper()
	once := notifier(t, addr, time.Hour, 0) // no retry
	for i := range 5000 {
		err := once.Notify(context.Background(), Ready)
		if errors.Is(err, syscall.EAGAIN) {
			return i
		}
		if err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	t.Fatal("the receiver's queue never filled")
	return 0
}

// A systemd that stops reading never holds the caller: with its queue full
// Notify retries, then reports EAGAIN within its bound.
func TestNotifyGivesUpWhenTheQueueStaysFull(t *testing.T) {
	_, addr := listen(t, false) // never read
	fill(t, addr)
	n := notifier(t, addr, 10*time.Millisecond, 100*time.Millisecond)
	start := time.Now()
	err := n.Notify(context.Background(), Ready)
	if !errors.Is(err, syscall.EAGAIN) {
		t.Fatalf("error %v, want EAGAIN", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("gave up after %v", elapsed)
	}
}

// A queue that drains while Notify retries gets the notification: READY is
// not lost to a moment of backpressure.
func TestNotifyRetriesUntilTheQueueDrains(t *testing.T) {
	conn, addr := listen(t, false)
	queued := fill(t, addr)
	go func() { // frees one slot; no t methods off the test goroutine
		time.Sleep(30 * time.Millisecond)
		_, _ = conn.Read(make([]byte, 4096))
	}()
	n := notifier(t, addr, 5*time.Millisecond, 10*time.Second)
	if err := n.Notify(context.Background(), Stopping); err != nil {
		t.Fatalf("after a slot was freed: %v", err)
	}
	// The queue holds the earlier datagrams, then this one, intact.
	var last string
	for range queued {
		last = receive(t, conn, time.Second)
	}
	if last != Stopping {
		t.Errorf("last datagram %q, want %q", last, Stopping)
	}
}

// Review 2c-4 (fixes) R1: no send after the retry budget, even when the
// retry timer fires together with the budget's and the queue has drained
// meanwhile. The clock jumps past the budget, and drains the queue, before
// the second attempt.
func TestNotifyNeverSendsAfterTheBudget(t *testing.T) {
	conn, addr := listen(t, false)
	queued := fill(t, addr)
	n := notifier(t, addr, time.Millisecond, time.Hour)
	start, calls := time.Now(), 0
	n.now = func() time.Time {
		calls++
		if calls == 1 {
			return start
		}
		for range queued { // systemd catches up...
			_ = receive(t, conn, time.Second)
		}
		return start.Add(2 * time.Hour) // ...after the budget
	}
	if err := n.Notify(context.Background(), Stopping); !errors.Is(err, syscall.EAGAIN) {
		t.Fatalf("error %v, want EAGAIN", err)
	}
	if got := receive(t, conn, 50*time.Millisecond); got != "" {
		t.Errorf("sent %q after the budget", got)
	}
}

// Cancelling ctx stops the retries at once.
func TestNotifyCancelledWhileRetrying(t *testing.T) {
	_, addr := listen(t, false) // never read
	fill(t, addr)
	n := notifier(t, addr, 5*time.Millisecond, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(30*time.Millisecond, cancel)
	start := time.Now()
	if err := n.Notify(ctx, Ready); !errors.Is(err, context.Canceled) {
		t.Fatalf("error %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("cancellation took %v", elapsed)
	}
}

// Every attempt closes its socket, failed ones included.
func TestNotifyLeaksNoDescriptor(t *testing.T) {
	_, addr := listen(t, false)
	// No real retries: past the receiver's queue, sends fail with EAGAIN.
	ok := notifier(t, addr, time.Millisecond, time.Millisecond)
	missing := notifier(t, filepath.Join(filepath.Dir(addr), "missing.sock"), time.Millisecond, time.Millisecond)
	before := openFDs(t)
	for range 100 {
		_ = ok.Notify(context.Background(), Ready)
		_ = missing.Notify(context.Background(), Ready)
	}
	if after := openFDs(t); after != before {
		t.Errorf("open descriptors %d, before %d", after, before)
	}
}

func openFDs(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}
