package sdnotify

import (
	"fmt"
	"syscall"
)

// abstractNames: Linux has the abstract socket namespace ("@name").
const abstractNames = true

// send makes one attempt: a non-blocking datagram socket of its own,
// close-on-exec from creation, one sendto to the address, close. No
// connect and no write(2): on kernel 7.0.0-1014-aws a write(2) on a
// connected datagram socket under AppArmor dereferenced NULL in
// update_sk_ctx and hung the host (D-079); sendto takes the may_send
// mediation instead, as libsystemd's sd_notify does. MSG_DONTWAIT returns
// EAGAIN when systemd's queue is full instead of waiting. The error is an
// errno: it never carries the address, which comes from the environment.
func send(addr string, msg []byte) error {
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_DGRAM|syscall.SOCK_NONBLOCK|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("sd_notify: socket: %w", err)
	}
	// A close error is ignored and never retried: on Linux the descriptor is
	// released anyway, and a datagram socket has nothing buffered to lose.
	defer func() { _ = syscall.Close(fd) }()
	// SockaddrUnix maps a leading '@' to the abstract namespace.
	if err := syscall.Sendto(fd, msg, syscall.MSG_DONTWAIT|syscall.MSG_NOSIGNAL, &syscall.SockaddrUnix{Name: addr}); err != nil {
		return fmt.Errorf("sd_notify: sendto: %w", err)
	}
	return nil
}
