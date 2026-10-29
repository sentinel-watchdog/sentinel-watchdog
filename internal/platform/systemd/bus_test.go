package systemd

import (
	"context"
	"encoding/hex"
	"errors"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

var nginx = UnitName{Prefix: "nginx", Type: TypeService}

func TestBusUnit(t *testing.T) {
	p := &peer{t: t}
	b := newTestBus(p)
	defer b.Close()
	st, err := b.Unit(context.Background(), nginx)
	if err != nil {
		t.Fatal(err)
	}
	if st.ID != "nginx.service" || st.Active != ActiveActive || st.NRestarts != 2 || st.InvocationID == "" {
		t.Errorf("state %+v", st)
	}
	// The connection is kept for the next read.
	if _, err := b.Unit(context.Background(), nginx); err != nil {
		t.Fatal(err)
	}
	if n := p.dialCount(); n != 1 {
		t.Errorf("dials = %d, want 1", n)
	}
}

func TestBusNotBooted(t *testing.T) {
	p := &peer{t: t}
	b := newTestBus(p)
	b.booted = func() bool { return false }
	if _, err := b.Unit(context.Background(), nginx); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("error = %v, want ErrUnavailable", err)
	}
	if p.dialCount() != 0 {
		t.Error("dialled although systemd is not booted")
	}
}

// bounded runs f and fails when it takes longer than limit.
func bounded(t *testing.T, limit time.Duration, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { f(); close(done) }()
	select {
	case <-done:
	case <-time.After(limit):
		t.Fatalf("still running after %s", limit)
	}
}

func TestBusAuthNeverAnswered(t *testing.T) {
	b := newTestBus(&peer{t: t, silentAuth: true})
	defer b.Close()
	bounded(t, 2*time.Second, func() {
		if _, err := b.Unit(context.Background(), nginx); !errors.Is(err, ErrUnavailable) {
			t.Errorf("error = %v, want ErrUnavailable", err)
		}
	})
}

// A peer that stops reading blocks the client's writes: the write
// deadline ends the call, and Close still returns (C1).
func TestBusBlockedWrite(t *testing.T) {
	b := newTestBus(&peer{t: t, stopAfterAuth: true})
	bounded(t, 2*time.Second, func() {
		if _, err := b.Unit(context.Background(), nginx); !errors.Is(err, ErrUnavailable) {
			t.Errorf("error = %v, want ErrUnavailable: the write failed", err)
		}
	})
	bounded(t, 2*time.Second, func() { b.Close() })
}

// writeSignal is a client connection that reports its first D-Bus message
// (one starts with its byte order, 'l'; the authentication is lines of
// text): a peer that stops reading after the authentication never reads
// it, so that write blocks.
type writeSignal struct {
	net.Conn
	blocked chan struct{}
	once    sync.Once
}

func (w *writeSignal) Write(b []byte) (int, error) {
	if len(b) > 0 && b[0] == 'l' {
		w.once.Do(func() { close(w.blocked) })
	}
	return w.Conn.Write(b)
}

// Close ends a write blocked on a peer that reads nothing, at once: it
// closes the socket before godbus's Close, which would wait for the write
// (review 3c-1 C1, F11).
func TestBusCloseDuringBlockedWrite(t *testing.T) {
	p := &peer{t: t, stopAfterAuth: true}
	b := newTestBus(p)
	b.writeTimeout = time.Hour // only Close can end the write
	blocked := make(chan struct{})
	dial := b.dial
	b.dial = func(ctx context.Context) (net.Conn, error) {
		c, err := dial(ctx)
		return &writeSignal{Conn: c, blocked: blocked}, err
	}
	done := make(chan error, 1)
	go func() {
		_, err := b.Unit(context.Background(), nginx)
		done <- err
	}()
	select {
	case <-blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("the read never wrote its call")
	}
	bounded(t, 2*time.Second, func() { b.Close() })
	bounded(t, 2*time.Second, func() {
		if err := <-done; err == nil {
			t.Error("the blocked read succeeded")
		}
	})
}

func TestBusCanceled(t *testing.T) {
	never := func(string, []any) ([]any, string) { return nil, "" }
	b := newTestBus(&peer{t: t, answer: never})
	defer b.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	bounded(t, 2*time.Second, func() {
		if _, err := b.Unit(ctx, nginx); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("error = %v, want the context's", err)
		}
	})
}

// A call its context ended is left unanswered, and godbus keeps its serial
// for the life of the connection: the connection is dropped, and the next
// read dials afresh. A context already done sends nothing (review 3c-1 F3).
func TestBusTimeoutDropsConnection(t *testing.T) {
	quiet := true // the first call is never answered
	var mu sync.Mutex
	answer := func(member string, args []any) ([]any, string) {
		mu.Lock()
		defer mu.Unlock()
		if quiet {
			return nil, ""
		}
		return healthy(member, args)
	}
	p := &peer{t: t, answer: answer}
	b := newTestBus(p)
	defer b.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	if _, err := b.Unit(ctx, nginx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v", err)
	}
	cancel()
	mu.Lock()
	quiet = false
	mu.Unlock()
	if _, err := b.Unit(context.Background(), nginx); err != nil {
		t.Fatal(err)
	}
	if n := p.dialCount(); n != 2 {
		t.Errorf("dials = %d, want a new connection after the timeout", n)
	}
	calls := p.calls.Load()
	done, stop := context.WithCancel(context.Background())
	stop()
	if _, err := b.Unit(done, nginx); !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want the context's", err)
	}
	if p.calls.Load() != calls {
		t.Error("a read with a context already done sent a call")
	}
}

// invocations are distinct InvocationIDs, A, B, C.
var invocations = [][]byte{[]byte("AAAAAAAAAAAAAAAA"), []byte("BBBBBBBBBBBBBBBB"), []byte("CCCCCCCCCCCCCCCC")}

// changing answers InvocationID A, then moves to the next invocation at
// the last read of each of the first flips samples: A B | B B for one
// flip, so the sample accepted is distinguishable from the one refused.
// reads counts the InvocationID reads.
func changing(flips int, reads *atomic.Int64) func(string, []any) ([]any, string) {
	cur := 0
	return func(member string, args []any) ([]any, string) {
		if member == "Get" && args[1] == "InvocationID" {
			n := reads.Add(1)
			if n <= int64(2*flips) && n%2 == 0 { // the last read of a changing sample
				cur++
			}
			return []any{dbus.MakeVariant(invocations[cur])}, ""
		}
		return healthy(member, args)
	}
}

func TestBusConsistentSample(t *testing.T) {
	t.Run("one change, read again", func(t *testing.T) {
		var reads atomic.Int64
		b := newTestBus(&peer{t: t, answer: changing(1, &reads)})
		defer b.Close()
		st, err := b.Unit(context.Background(), nginx)
		if err != nil {
			t.Fatal(err)
		}
		if want := hex.EncodeToString(invocations[1]); st.InvocationID != want || reads.Load() != 4 {
			t.Errorf("invocation %s after %d reads, want the second sample's %s after 4", st.InvocationID, reads.Load(), want)
		}
	})
	t.Run("two changes", func(t *testing.T) {
		var reads atomic.Int64
		b := newTestBus(&peer{t: t, answer: changing(2, &reads)})
		defer b.Close()
		if _, err := b.Unit(context.Background(), nginx); !errors.Is(err, ErrChanging) {
			t.Fatalf("error = %v, want ErrChanging", err)
		}
	})
	t.Run("unit gone", func(t *testing.T) {
		gone := func(member string, args []any) ([]any, string) {
			if member == "Get" && args[1] == "ActiveState" {
				return nil, "org.freedesktop.DBus.Error.UnknownObject"
			}
			return healthy(member, args)
		}
		b := newTestBus(&peer{t: t, answer: gone})
		defer b.Close()
		if _, err := b.Unit(context.Background(), nginx); !errors.Is(err, ErrChanging) {
			t.Fatalf("error = %v, want ErrChanging", err)
		}
	})
}

func TestBusBadReplies(t *testing.T) {
	tests := []struct {
		name    string
		answer  func(string, []any) ([]any, string)
		invalid bool // the error is ErrInvalid
	}{
		{"path outside systemd's units", func(m string, a []any) ([]any, string) {
			if m == "LoadUnit" {
				return []any{dbus.ObjectPath("/org/freedesktop/systemd1")}, ""
			}
			return healthy(m, a)
		}, true},
		{"active state of another type", func(m string, a []any) ([]any, string) {
			if m == "Get" && a[1] == "ActiveState" {
				return []any{dbus.MakeVariant(uint32(1))}, ""
			}
			return healthy(m, a)
		}, true},
		{"a state systemd may add", func(m string, a []any) ([]any, string) {
			if m == "Get" && a[1] == "ActiveState" {
				return []any{dbus.MakeVariant("hibernating")}, ""
			}
			return healthy(m, a)
		}, true},
		{"last invocation of another type", func() func(string, []any) ([]any, string) {
			reads := 0
			return func(m string, a []any) ([]any, string) {
				if m == "Get" && a[1] == "InvocationID" {
					if reads++; reads == 2 {
						return []any{dbus.MakeVariant("not bytes")}, ""
					}
				}
				return healthy(m, a)
			}
		}(), true},
		{"LoadUnit refused", func(m string, a []any) ([]any, string) {
			if m == "LoadUnit" {
				return nil, "org.freedesktop.DBus.Error.AccessDenied"
			}
			return healthy(m, a)
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newTestBus(&peer{t: t, answer: tt.answer})
			defer b.Close()
			_, err := b.Unit(context.Background(), nginx)
			if err == nil {
				t.Fatal("accepted")
			}
			if errors.Is(err, ErrInvalid) != tt.invalid {
				t.Errorf("error = %v, want ErrInvalid %v", err, tt.invalid)
			}
		})
	}
}

func TestBusRedials(t *testing.T) {
	p := &peer{t: t}
	b := newTestBus(p)
	defer b.Close()
	if _, err := b.Unit(context.Background(), nginx); err != nil {
		t.Fatal(err)
	}
	p.hangUp() // systemd re-executed itself, which closes its private connections
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, err := b.Unit(context.Background(), nginx)
		if err == nil {
			break
		}
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("error = %v, want ErrUnavailable while the connection is lost", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("never redialled")
		}
	}
	if n := p.dialCount(); n < 2 {
		t.Errorf("dials = %d, want a redial", n)
	}
}

func TestBusClosed(t *testing.T) {
	b := newTestBus(&peer{t: t})
	b.Close()
	if _, err := b.Unit(context.Background(), nginx); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("error = %v, want ErrUnavailable", err)
	}
}

// Close racing reads and redials ends them all, with no data race (C6):
// the hang-up and Close wait until reads have reached the peer.
func TestBusCloseRacesReads(t *testing.T) {
	p := &peer{t: t}
	b := newTestBus(p)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 50 {
				if _, err := b.Unit(context.Background(), nginx); err != nil && !errors.Is(err, ErrUnavailable) {
					t.Errorf("error = %v", err)
					return
				}
			}
		})
	}
	waitCalls := func(n int64) {
		deadline := time.Now().Add(5 * time.Second)
		for p.calls.Load() < n {
			if time.Now().After(deadline) {
				t.Fatalf("only %d calls reached the peer", p.calls.Load())
			}
			runtime.Gosched()
		}
	}
	waitCalls(20)
	p.hangUp() // reads in flight fail and redial
	waitCalls(p.calls.Load() + 20)
	b.Close()
	bounded(t, 5*time.Second, wg.Wait)
	if p.dialCount() < 2 {
		t.Errorf("dials = %d: no redial was exercised", p.dialCount())
	}
}

// Review 3c-1 F23: a connection another read replaced is lost, although
// godbus still thinks it connected.
func TestBusReplacedConnectionIsLost(t *testing.T) {
	b := newTestBus(&peer{t: t})
	defer b.Close()
	if _, err := b.Unit(context.Background(), nginx); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	old := b.conn
	b.conn, b.sock = nil, nil // another read dropped it
	b.mu.Unlock()
	defer old.Close()
	if !old.Connected() {
		t.Fatal("the old connection is closed already")
	}
	if !b.lost(old) {
		t.Error("a replaced connection is not lost")
	}
}

// Review 3c-1 fixes R1: an authentication that stalls ends at the read's
// deadline, its writes too, not at writeTimeout.
func TestBusAuthHonoursCallDeadline(t *testing.T) {
	b := newTestBus(&peer{t: t, silentAuth: true})
	defer b.Close()
	b.dialTimeout, b.writeTimeout = time.Hour, time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	bounded(t, time.Second, func() {
		if _, err := b.Unit(ctx, nginx); err == nil {
			t.Error("a stalled authentication succeeded")
		}
	})
}

// cancelAtWrite cancels the setup's context just as a write sets its
// deadline, and lets the write go on only once the cancellation acted.
type cancelAtWrite struct {
	net.Conn
	cancel      context.CancelFunc
	interrupted chan struct{}
	once        sync.Once
}

func (c *cancelAtWrite) SetWriteDeadline(d time.Time) error {
	c.cancel()
	<-c.interrupted
	return c.Conn.SetWriteDeadline(d)
}

func (c *cancelAtWrite) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { close(c.interrupted) })
	return err
}

// Review 3c-1 fixes2 R1: a cancellation that lands just before an
// authentication write is not undone by that write's own deadline.
func TestBusAuthEarlyCancellation(t *testing.T) {
	raw, other := net.Pipe() // the other end reads nothing
	defer raw.Close()
	defer other.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &cancelAtWrite{Conn: raw, cancel: cancel, interrupted: make(chan struct{})}
	b := Open()
	b.dialTimeout, b.writeTimeout = time.Hour, time.Hour
	b.dial = func(context.Context) (net.Conn, error) { return c, nil }
	done := make(chan error, 1)
	go func() {
		conn, _, err := b.connect(ctx)
		if conn != nil {
			_ = conn.Close() // only a failed test gets here
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a canceled authentication succeeded")
		}
	case <-time.After(5 * time.Second):
		_ = raw.Close()
		if err := <-done; err == nil { // wait for the setup to end
			t.Error("the authentication succeeded after all")
		}
		t.Fatal("an authentication write undid the cancellation")
	}
}

// unixLikeClose fails a second Close as a Unix socket does.
type unixLikeClose struct {
	net.Conn
	mu     sync.Mutex
	closed bool
}

func (c *unixLikeClose) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return net.ErrClosed
	}
	c.closed = true
	return c.Conn.Close()
}

// Review 3c-1 fixes R2: a normal Close reports no error of its own double
// close.
func TestBusCloseReportsNoDoubleClose(t *testing.T) {
	b := newTestBus(&peer{t: t})
	dial := b.dial
	b.dial = func(ctx context.Context) (net.Conn, error) {
		c, err := dial(ctx)
		if err != nil {
			return nil, err
		}
		return &unixLikeClose{Conn: c}, nil
	}
	if _, err := b.Unit(context.Background(), nginx); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Errorf("Close = %v", err)
	}
}

// Review 3c-1 F1, fixes R4: production dials systemd's private socket,
// never the system bus.
func TestBusDialsThePrivateSocket(t *testing.T) {
	done, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Open().dial(done) // no connection is made with a context already done
	var oe *net.OpError
	if !errors.As(err, &oe) || oe.Net != "unix" || oe.Addr == nil || oe.Addr.String() != "/run/systemd/private" {
		t.Fatalf("dial error = %#v", err)
	}
}

// What systemd might send unasked (a method call to sentineld, a signal)
// is dropped, and leaves no goroutine behind (C11).
func TestBusUnsolicited(t *testing.T) {
	call := &dbus.Message{Type: dbus.TypeMethodCall, Headers: map[dbus.HeaderField]dbus.Variant{
		dbus.FieldPath: dbus.MakeVariant(dbus.ObjectPath("/x")), dbus.FieldMember: dbus.MakeVariant("Poke")}}
	signal := &dbus.Message{Type: dbus.TypeSignal, Headers: map[dbus.HeaderField]dbus.Variant{
		dbus.FieldPath:      dbus.MakeVariant(dbus.ObjectPath("/x")),
		dbus.FieldInterface: dbus.MakeVariant("org.example.X"), dbus.FieldMember: dbus.MakeVariant("Ping")}}
	before := runtime.NumGoroutine()
	p := &peer{t: t, sendAfterAuth: []*dbus.Message{call, signal}}
	b := newTestBus(p)
	if _, err := b.Unit(context.Background(), nginx); err != nil {
		t.Fatal(err)
	}
	b.Close()
	p.hangUp()
	p.serve.Wait()
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before {
		if time.Now().After(deadline) {
			t.Fatalf("goroutines: %d before, %d after", before, runtime.NumGoroutine())
		}
		time.Sleep(10 * time.Millisecond) // godbus's reader and call handler end asynchronously
	}
}
