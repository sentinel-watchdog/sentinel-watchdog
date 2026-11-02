package systemd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	// privateSocket is systemd's own D-Bus socket, the one systemctl uses
	// as root. systemd accepts only root on it, so its only peer is systemd
	// itself: on the system bus any local user may send sentineld signals,
	// which godbus decodes whole before they can be dropped (review 3c-1
	// F1). It needs no bus daemon either. The path is fixed: no variable
	// of the environment can point sentineld elsewhere.
	privateSocket = "/run/systemd/private"
	// bootedDir exists when systemd is the init system (sd_booted(3)).
	bootedDir = "/run/systemd/system"

	// dialTimeout bounds the dial and the authentication together.
	dialTimeout = 5 * time.Second
	// writeTimeout bounds each write: godbus writes a call synchronously
	// and holds its output lock, so a context alone cannot end a write to
	// a peer that stops reading (review 3c-1 C1).
	writeTimeout = 5 * time.Second

	systemdDest  = "org.freedesktop.systemd1"
	systemdPath  = dbus.ObjectPath("/org/freedesktop/systemd1")
	unitPrefix   = "/org/freedesktop/systemd1/unit/"
	unitIface    = "org.freedesktop.systemd1.Unit"
	serviceIface = "org.freedesktop.systemd1.Service"
)

// Bus reads units from systemd over its private socket. One connection
// serves every caller; it is dialled on the first read, and again after it
// is lost (systemd re-executing itself closes it) or after a call that its
// context ended: the call may never be answered, and godbus keeps each
// unanswered call's serial for the life of the connection (review 3c-1
// F3). Redialling has no backoff of its own: the callers' intervals pace
// it. A later user manager would get its own constructor returning a Bus;
// nothing here assumes the system manager beyond Open's address.
//
// Its only goroutine is godbus's reader, which Close ends.
type Bus struct {
	// Replaced by the tests.
	dial         func(ctx context.Context) (net.Conn, error)
	booted       func() bool
	dialTimeout  time.Duration
	writeTimeout time.Duration
	uid          string

	mu sync.Mutex // one dial at a time; guards conn, sock and closed
	// conn is the open connection, sock its socket: closing sock first
	// ends a write blocked in conn, whose Close would wait for it.
	conn   *dbus.Conn
	sock   *deadlineConn
	closed bool
}

// Open returns a Bus for the system manager. It does not dial: Open is
// cheap and has no effect on the system.
func Open() *Bus {
	return &Bus{
		dial: func(ctx context.Context) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", privateSocket)
		},
		booted:       systemdBooted,
		dialTimeout:  dialTimeout,
		writeTimeout: writeTimeout,
		uid:          strconv.Itoa(os.Geteuid()),
	}
}

func systemdBooted() bool {
	fi, err := os.Stat(bootedDir)
	return err == nil && fi.IsDir()
}

// Unit reads the state of unit n. Errors wrap ErrUnavailable when systemd
// cannot be reached, ErrChanging when no consistent state could be read,
// and ErrInvalid when systemd's reply could not be trusted; a context that
// ended is returned as its own error.
func (b *Bus) Unit(ctx context.Context, n UnitName) (UnitState, error) {
	if !b.booted() {
		return UnitState{}, ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return UnitState{}, err // godbus would still take a serial for the call
	}
	conn, err := b.connection(ctx)
	if err != nil {
		return UnitState{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	st, err := read(ctx, conn, n)
	switch {
	case err == nil:
		return st, nil
	case b.lost(conn):
		b.drop(conn)
		return UnitState{}, fmt.Errorf("%w: connection lost: %w", ErrUnavailable, err)
	case ctx.Err() != nil:
		b.drop(conn) // a call left unanswered: start afresh
	}
	return UnitState{}, err
}

// lost reports whether conn is closed, is no longer the open connection
// (another read dropped it, or Close ran), or its socket failed a write:
// godbus's reader may not have noticed the last two yet.
func (b *Bus) lost(conn *dbus.Conn) bool {
	if !conn.Connected() {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.conn != conn || b.sock.failed.Load()
}

// Close closes the connection; later reads fail with ErrUnavailable.
func (b *Bus) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	if b.conn == nil {
		return nil
	}
	_ = b.sock.Close() // only unblocks a write: conn.Close reports the outcome
	err := b.conn.Close()
	b.conn, b.sock = nil, nil
	return err
}

// connection returns the open connection, dialling one if there is none.
func (b *Bus) connection(ctx context.Context) (*dbus.Conn, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch {
	case b.closed:
		return nil, errors.New("closed")
	case b.conn != nil && b.conn.Connected():
		return b.conn, nil
	}
	conn, sock, err := b.connect(ctx)
	if err != nil {
		return nil, err
	}
	b.conn, b.sock = conn, sock
	return conn, nil
}

// drop forgets conn, if it is still the open one, and closes it.
func (b *Bus) drop(conn *dbus.Conn) {
	b.mu.Lock()
	if b.conn == conn {
		_ = b.sock.Close() // the connection is being discarded: nothing to report
		b.conn, b.sock = nil, nil
	}
	b.mu.Unlock()
	_ = conn.Close() // as above
}

// connect dials systemd and authenticates as sentineld's uid (EXTERNAL:
// systemd checks it with the socket's credentials), within dialTimeout or
// ctx's deadline, whichever comes first. A direct connection to systemd
// has no bus daemon, so there is no Hello.
func (b *Bus) connect(ctx context.Context) (*dbus.Conn, *deadlineConn, error) {
	deadline := time.Now().Add(b.dialTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	dctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	raw, err := b.dial(dctx)
	if err != nil {
		return nil, nil, fmt.Errorf("dial systemd: %w", err)
	}
	// Auth takes no context: the socket's deadlines bound it, the writes'
	// capped by the setup's deadline too (review 3c-1 fixes R1), and the
	// context's end closes the socket: a deadline set then could be
	// replaced by the next write's own (fixes2 R1).
	if err := raw.SetReadDeadline(deadline); err != nil {
		_ = raw.Close() // the setup's own error is the one to report
		return nil, nil, fmt.Errorf("dial systemd: %w", err)
	}
	sock := &deadlineConn{Conn: raw, timeout: b.writeTimeout}
	sock.until.Store(deadline.UnixNano())
	stop := context.AfterFunc(dctx, func() { _ = sock.Close() }) // only during the setup: stop() ends it
	defer stop()
	conn, err := dbus.NewConn(sock, dbus.WithSignalHandler(dropSignals{}), dbus.WithHandler(noObjects{}))
	if err != nil {
		_ = raw.Close() // the setup's own error is the one to report
		return nil, nil, fmt.Errorf("open the connection to systemd: %w", err)
	}
	if err := conn.Auth([]dbus.Auth{dbus.AuthExternal(b.uid)}); err != nil {
		_ = conn.Close() // the setup's own error is the one to report
		return nil, nil, fmt.Errorf("authenticate to systemd: %w", err)
	}
	if !stop() { // the context ended during the setup: its deadline is set
		_ = conn.Close() // the setup's own error is the one to report
		return nil, nil, fmt.Errorf("authenticate to systemd: %w", dctx.Err())
	}
	sock.until.Store(0)
	if err := raw.SetReadDeadline(time.Time{}); err != nil { // the reader now waits for replies
		_ = conn.Close() // the setup's own error is the one to report
		return nil, nil, fmt.Errorf("open the connection to systemd: %w", err)
	}
	return conn, sock, nil
}

// read reads one consistent state of n: InvocationID is read first and
// last, and a sample in which it changed (a restart between two reads) is
// read once more (review 3c-1 C4). Only single properties are read, never
// GetAll: the Service interface also holds ExecStart and Environment,
// which may carry secrets.
func read(ctx context.Context, conn *dbus.Conn, n UnitName) (UnitState, error) {
	var path dbus.ObjectPath
	call := conn.Object(systemdDest, systemdPath).CallWithContext(ctx,
		"org.freedesktop.systemd1.Manager.LoadUnit", 0, n.String())
	if err := call.Store(&path); err != nil {
		return UnitState{}, fmt.Errorf("load unit: %w", classify(err))
	}
	if !path.IsValid() || !strings.HasPrefix(string(path), unitPrefix) || len(path) == len(unitPrefix) {
		return UnitState{}, fmt.Errorf("%w: load unit: a path outside systemd's units", ErrInvalid)
	}
	unit := conn.Object(systemdDest, path)
	for range 2 {
		st, same, err := sample(ctx, unit, n)
		if err != nil || same {
			return st, err
		}
	}
	return UnitState{}, ErrChanging
}

// sample reads the properties of unit n once; same reports whether its
// InvocationID was the same at the start and at the end.
func sample(ctx context.Context, unit dbus.BusObject, n UnitName) (UnitState, bool, error) {
	get := func(iface, name string) (any, error) {
		var v dbus.Variant
		err := unit.CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0, iface, name).Store(&v)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, classify(err))
		}
		return v.Value(), nil
	}
	props := map[string]any{}
	first, err := get(unitIface, "InvocationID")
	if err != nil {
		return UnitState{}, false, err
	}
	props["InvocationID"] = first
	for _, name := range []string{"Id", "LoadState", "ActiveState", "SubState", "Job"} {
		if props[name], err = get(unitIface, name); err != nil {
			return UnitState{}, false, err
		}
	}
	if n.Type == TypeService && props["LoadState"] == string(LoadLoaded) {
		for _, name := range []string{"NRestarts", "Result"} {
			if props[name], err = get(serviceIface, name); err != nil {
				return UnitState{}, false, err
			}
		}
	}
	last, err := get(unitIface, "InvocationID")
	if err != nil {
		return UnitState{}, false, err
	}
	st, err := decodeState(props)
	if err != nil {
		return UnitState{}, false, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	lastID, err := invocation(last)
	if err != nil {
		return UnitState{}, false, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	return st, lastID == st.InvocationID, nil
}

// classify marks the error of a unit that went away while it was read
// (unloaded between two calls) as ErrChanging.
func classify(err error) error {
	var de dbus.Error
	if errors.As(err, &de) && de.Name == "org.freedesktop.DBus.Error.UnknownObject" {
		return fmt.Errorf("%w: %w", ErrChanging, err)
	}
	return err
}

// deadlineConn bounds every write by timeout, and while until is set (Unix
// nanoseconds) by that instant too. A write that fails leaves a message
// half sent, so it closes the socket and records the failure: godbus's
// reader then ends the connection whole. It closes the socket once: Bus
// closes it first to end a blocked write, then godbus closes it again
// (review 3c-1 fixes R2).
type deadlineConn struct {
	net.Conn
	timeout   time.Duration
	until     atomic.Int64
	failed    atomic.Bool
	closeOnce sync.Once
	closeErr  error
}

func (c *deadlineConn) Write(p []byte) (int, error) {
	d := time.Now().Add(c.timeout)
	if u := c.until.Load(); u != 0 && time.Unix(0, u).Before(d) {
		d = time.Unix(0, u)
	}
	if err := c.SetWriteDeadline(d); err != nil {
		c.fail()
		return 0, err
	}
	n, err := c.Conn.Write(p)
	if err != nil {
		c.fail()
	}
	return n, err
}

func (c *deadlineConn) fail() {
	c.failed.Store(true)
	_ = c.Close() // the failed write's error is the one to report
}

// Close closes the socket the first time and returns that outcome every
// time.
func (c *deadlineConn) Close() error {
	c.closeOnce.Do(func() { c.closeErr = c.Conn.Close() })
	return c.closeErr
}

// dropSignals drops every signal: no match rule and no Subscribe are
// sent, so systemd sends none (review 3c-1 C11).
type dropSignals struct{}

func (dropSignals) DeliverSignal(string, string, *dbus.Signal) {}

// noObjects exports nothing: godbus answers a method call to sentineld
// with an error, from a goroutine of its own per call; on the private
// socket only systemd could send one.
type noObjects struct{}

func (noObjects) LookupObject(dbus.ObjectPath) (dbus.ServerObject, bool) { return nil, false }
