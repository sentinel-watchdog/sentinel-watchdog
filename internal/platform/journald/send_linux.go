package journald

import (
	"errors"
	"fmt"
	"syscall"
)

// Send sends entry to the socket at path as one datagram: a non-blocking
// socket of its own, close-on-exec from creation, one sendto, close. No
// connect and no write(2), as sdnotify (D-079, R-024). A socket per entry has
// no lifetime to share among the streams that send, so a closed descriptor can
// never be written to after its number is reused. MSG_DONTWAIT makes a full
// queue an immediate EAGAIN, returned as ErrFull. Errors are errnos with what
// was being done: they never carry the path.
func Send(path string, entry []byte) error {
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_DGRAM|syscall.SOCK_NONBLOCK|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("journald: socket: %w", err)
	}
	// A close error is ignored: on Linux the descriptor is released anyway,
	// and a datagram socket has nothing buffered to lose.
	defer func() { _ = syscall.Close(fd) }()
	err = syscall.Sendto(fd, entry, syscall.MSG_DONTWAIT|syscall.MSG_NOSIGNAL, &syscall.SockaddrUnix{Name: path})
	switch {
	case errors.Is(err, syscall.EAGAIN):
		return fmt.Errorf("%w: %w", ErrFull, err)
	case err != nil:
		return fmt.Errorf("journald: sendto: %w", err)
	}
	return nil
}
