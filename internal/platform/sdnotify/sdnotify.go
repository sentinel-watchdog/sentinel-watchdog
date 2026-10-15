// Package sdnotify tells systemd about the service's state, as sd_notify(3)
// does: newline-separated assignments sent as one datagram to the socket
// that NOTIFY_SOCKET names (D-079). Without the variable nothing is sent,
// so sentineld behaves the same outside systemd.
//
// It is a platform adapter: cmd/sentineld decides when to notify, this
// package only validates the address and sends.
package sdnotify

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"syscall"
	"time"
)

// EnvSocket names the environment variable that systemd sets for
// Type=notify services.
const EnvSocket = "NOTIFY_SOCKET"

// States understood by systemd.
const (
	Ready    = "READY=1"
	Stopping = "STOPPING=1"
)

// timeout bounds one notification, dial and write together. Generous for a
// local datagram, so that a busy systemd at boot still gets READY; short
// enough that a stalled one cannot hold the start or the shutdown.
const timeout = time.Second

// maxAddr is the longest address a sockaddr_un holds (107 bytes on Linux,
// 103 on macOS): one byte stays for the terminating NUL of a path.
const maxAddr = len(syscall.RawSockaddrUnix{}.Path) - 1

// maxStatus bounds a STATUS= text: systemd shows it on one line.
const maxStatus = 200

// ErrInvalidSocket reports a NOTIFY_SOCKET value that is not an absolute
// path or an abstract name. Its text never contains the value: the
// environment is untrusted and may carry anything into the logs.
var ErrInvalidSocket = errors.New(EnvSocket + " is not a valid socket address")

// Notifier sends state to systemd. A nil *Notifier sends nothing.
type Notifier struct {
	addr string
}

// Parse validates the value of NOTIFY_SOCKET. An empty value gives a nil
// Notifier, which sends nothing. Accepted: an absolute path, or an abstract
// name starting with '@' (Linux). Rejected with ErrInvalidSocket: relative
// paths, other address families (systemd's "vsock:"), a bare '@', control
// characters and values longer than a socket address holds.
func Parse(value string) (*Notifier, error) {
	if value == "" {
		return nil, nil
	}
	if len(value) > maxAddr {
		return nil, fmt.Errorf("%w: longer than %d bytes", ErrInvalidSocket, maxAddr)
	}
	if value == "@" || (value[0] != '/' && value[0] != '@') {
		return nil, fmt.Errorf("%w: neither an absolute path nor an abstract name", ErrInvalidSocket)
	}
	if strings.ContainsFunc(value, isControl) {
		return nil, fmt.Errorf("%w: control characters", ErrInvalidSocket)
	}
	return &Notifier{addr: value}, nil
}

// Notify sends the states as one datagram, waiting at most one second or
// until ctx ends. A nil Notifier returns nil at once.
func (n *Notifier) Notify(ctx context.Context, states ...string) error {
	if n == nil || len(states) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unixgram", n.addr)
	if err != nil {
		return sendError(err)
	}
	defer conn.Close()            // nothing buffered: the datagram is sent or the write failed
	deadline, _ := ctx.Deadline() // always set by WithTimeout
	if err := conn.SetWriteDeadline(deadline); err != nil {
		return sendError(err)
	}
	if _, err := conn.Write([]byte(strings.Join(states, "\n"))); err != nil {
		return sendError(err)
	}
	return nil
}

// sendError drops the address that a *net.OpError carries: it comes from
// the environment and must not reach the logs.
func sendError(err error) error {
	var op *net.OpError
	if errors.As(err, &op) {
		return fmt.Errorf("sd_notify: %s: %w", op.Op, op.Err)
	}
	return fmt.Errorf("sd_notify: %w", err)
}

// Status returns a STATUS= assignment with text on one line: control
// characters become spaces, so the text cannot add assignments of its
// own, and it is cut to a length systemd shows.
func Status(text string) string {
	text = strings.Map(func(r rune) rune {
		if isControl(r) {
			return ' '
		}
		return r
	}, text)
	if len(text) > maxStatus {
		text = strings.ToValidUTF8(text[:maxStatus], "")
	}
	return "STATUS=" + text
}

// isControl reports ASCII control characters, NUL and newline included.
func isControl(r rune) bool { return r < 0x20 || r == 0x7f }
