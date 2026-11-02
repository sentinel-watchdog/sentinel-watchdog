package systemd

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

// peer is a fake systemd on the far end of a net.Pipe: the EXTERNAL
// authentication, then just enough of the D-Bus API for Bus, with no root,
// systemd or bus.
type peer struct {
	t *testing.T
	// answer returns the reply to a method call: its body, or the name
	// of an error; neither means no reply. A nil answer replies with the
	// properties of props().
	answer func(member string, args []any) (body []any, errName string)
	// silentAuth: never answer the authentication.
	silentAuth bool
	// stopAfterAuth: once authenticated, read nothing more, so that the
	// client's next write blocks; stopped is closed then.
	stopAfterAuth bool
	stopped       chan struct{}
	// sendAfterAuth is written to the client right after the
	// authentication.
	sendAfterAuth []*dbus.Message

	quit  chan struct{}  // closed when the test ends: stalled handlers return
	serve sync.WaitGroup // the served pipes
	calls atomic.Int64   // method calls received

	mu    sync.Mutex
	dials int
	conns []net.Conn
}

// dial is Bus.dial: each call opens a new pipe served by the peer.
func (p *peer) dial(ctx context.Context) (net.Conn, error) {
	client, server := net.Pipe()
	p.mu.Lock()
	p.dials++
	p.conns = append(p.conns, server)
	p.mu.Unlock()
	p.serve.Add(1)
	go func() {
		defer p.serve.Done()
		p.handle(server)
	}()
	return client, nil
}

func (p *peer) dialCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.dials
}

// hangUp closes every connection the peer serves.
func (p *peer) hangUp() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		c.Close()
	}
	p.conns = nil
}

func (p *peer) handle(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	if b, err := r.ReadByte(); err != nil || b != 0 {
		return
	}
	if p.silentAuth {
		<-p.quit
		return
	}
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		switch {
		case line == "AUTH\r\n":
			_, _ = c.Write([]byte("REJECTED EXTERNAL\r\n"))
		case strings.HasPrefix(line, "AUTH EXTERNAL"):
			_, _ = c.Write([]byte("OK 0123456789abcdef0123456789abcdef\r\n"))
		case line == "BEGIN\r\n":
			for _, m := range p.sendAfterAuth {
				_ = m.EncodeTo(c, binary.LittleEndian)
			}
			if p.stopAfterAuth {
				close(p.stopped)
				<-p.quit
				return
			}
			p.messages(c, r)
			return
		default:
			_, _ = c.Write([]byte("ERROR\r\n"))
		}
	}
}

func (p *peer) messages(c net.Conn, r *bufio.Reader) {
	for {
		msg, err := dbus.DecodeMessage(r)
		if err != nil {
			return
		}
		if msg.Type != dbus.TypeMethodCall {
			continue
		}
		p.calls.Add(1)
		member, _ := msg.Headers[dbus.FieldMember].Value().(string)
		answer := p.answer
		if answer == nil {
			answer = healthy
		}
		body, errName := answer(member, msg.Body)
		if body == nil && errName == "" {
			continue // no reply at all
		}
		p.reply(c, msg, body, errName)
	}
}

func (p *peer) reply(c net.Conn, call *dbus.Message, body []any, errName string) {
	m := &dbus.Message{Type: dbus.TypeMethodReply, Headers: map[dbus.HeaderField]dbus.Variant{
		dbus.FieldReplySerial: dbus.MakeVariant(call.Serial())}, Body: body}
	if errName != "" {
		m.Type = dbus.TypeError
		m.Headers[dbus.FieldErrorName] = dbus.MakeVariant(errName)
		m.Body = []any{"fake error"}
	}
	if len(m.Body) > 0 {
		m.Headers[dbus.FieldSignature] = dbus.MakeVariant(dbus.SignatureOf(m.Body...))
	}
	if err := m.EncodeTo(c, binary.LittleEndian); err != nil && !errors.Is(err, net.ErrClosed) {
		p.t.Logf("peer: reply: %v", err)
	}
}

// healthy answers like systemd for a running service.
func healthy(member string, args []any) ([]any, string) {
	switch member {
	case "LoadUnit":
		return []any{dbus.ObjectPath("/org/freedesktop/systemd1/unit/nginx_2eservice")}, ""
	case "Get":
		name, _ := args[1].(string)
		v, ok := props()[name]
		if !ok {
			return nil, "org.freedesktop.DBus.Error.UnknownProperty"
		}
		if name == "Job" { // a (uo) struct on the wire, decoded as []any
			v = struct {
				ID   uint32
				Path dbus.ObjectPath
			}{0, "/"}
		}
		return []any{dbus.MakeVariant(v)}, ""
	}
	return nil, "org.freedesktop.DBus.Error.UnknownMethod"
}

// newTestBus returns a Bus served by p, with short bounds.
func newTestBus(p *peer) *Bus {
	p.quit, p.stopped = make(chan struct{}), make(chan struct{})
	p.t.Cleanup(func() { close(p.quit); p.hangUp(); p.serve.Wait() })
	b := Open()
	b.dial = p.dial
	b.booted = func() bool { return true }
	b.dialTimeout, b.writeTimeout = 200*time.Millisecond, 200*time.Millisecond
	return b
}
