// Package sdnotify tells systemd about the service's state, as sd_notify(3)
// does: newline-separated assignments sent as one datagram to the socket
// that NOTIFY_SOCKET names (D-079). Without the variable nothing is sent,
// so sentineld behaves the same outside systemd. Sending is supported on
// Linux only, where sentineld runs.
//
// It is a platform adapter: cmd/sentineld decides when to notify, this
// package only validates the address and sends.
package sdnotify

import (
	"context"
	"errors"
	"fmt"
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

// Retries when systemd's queue is full: a lost READY=1 would leave a
// Type=notify unit starting until its timeout, so a few short retries are
// worth it; past retryFor the notification is given up.
const (
	retryEvery = 50 * time.Millisecond
	retryFor   = time.Second
)

// maxAddr is the longest address a sockaddr_un holds (107 bytes on Linux,
// 103 on macOS): one byte stays for the terminating NUL of a path.
const maxAddr = len(syscall.RawSockaddrUnix{}.Path) - 1

// maxStatus bounds a STATUS= text: systemd shows it on one line.
const maxStatus = 200

// ErrInvalidSocket reports a NOTIFY_SOCKET value that is not an absolute
// path or an abstract name. Its text never contains the value: the
// environment is untrusted and may carry anything into the logs.
var ErrInvalidSocket = errors.New(EnvSocket + " is not a valid socket address")

// ErrUnsupported reports a system where sentineld does not notify systemd.
var ErrUnsupported = errors.New("sd_notify is supported on Linux only")

// Notifier sends state to systemd. A nil *Notifier sends nothing.
type Notifier struct {
	addr string
	// Retry policy; tests shorten it.
	retryEvery, retryFor time.Duration
}

// Parse validates the value of NOTIFY_SOCKET. An empty value gives a nil
// Notifier, which sends nothing. Accepted: an absolute path, or (Linux) an
// abstract name starting with '@'. Rejected with ErrInvalidSocket: relative
// paths, other address families (systemd's "vsock:"), a bare '@', control
// characters and values longer than a socket address holds.
func Parse(value string) (*Notifier, error) {
	if value == "" {
		return nil, nil
	}
	if len(value) > maxAddr {
		return nil, fmt.Errorf("%w: longer than %d bytes", ErrInvalidSocket, maxAddr)
	}
	switch {
	case value[0] == '@' && !abstractNames:
		return nil, fmt.Errorf("%w: abstract names exist on Linux only", ErrInvalidSocket)
	case value == "@" || (value[0] != '/' && value[0] != '@'):
		return nil, fmt.Errorf("%w: neither an absolute path nor an abstract name", ErrInvalidSocket)
	}
	if strings.ContainsFunc(value, isControl) {
		return nil, fmt.Errorf("%w: control characters", ErrInvalidSocket)
	}
	return &Notifier{addr: value, retryEvery: retryEvery, retryFor: retryFor}, nil
}

// Notify sends the states as one datagram. It never waits inside the
// kernel: each attempt is a non-blocking send on a socket of its own, so a
// systemd that does not read cannot hold the caller. While systemd's queue
// is full it retries for at most one second. A ctx already done sends
// nothing; ctx ending between retries stops them. A nil Notifier returns
// nil at once.
func (n *Notifier) Notify(ctx context.Context, states ...string) error {
	if n == nil || len(states) == 0 {
		return nil
	}
	msg := []byte(strings.Join(states, "\n"))
	deadline := time.NewTimer(n.retryFor)
	defer deadline.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("sd_notify: %w", err)
		}
		err := send(n.addr, msg)
		if !errors.Is(err, syscall.EAGAIN) {
			return err
		}
		retry := time.NewTimer(n.retryEvery)
		select {
		case <-retry.C:
		case <-deadline.C:
			retry.Stop()
			return err // the queue stayed full
		case <-ctx.Done():
			retry.Stop()
			return fmt.Errorf("sd_notify: %w", ctx.Err())
		}
	}
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
