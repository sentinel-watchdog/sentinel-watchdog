package state

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/fstrust"
)

// lockFile holds the state directory's lock. A module directory cannot
// have this name (module names have no dot), nor can an audit file.
const lockFile = "sentineld.lock"

// lock takes the state directory's exclusive lock (flock) in root: one
// daemon owns a state directory, whatever socket it listens on (D-092),
// because the audit chain, module state and process-group records allow a
// single writer. The file is opened like the socket's lock: inside the
// checked directory, without following a link or blocking on a FIFO, and
// checked regular, owned and private (0600). The lock ends with the descriptor, so a
// daemon that dies leaves no stale lock.
func lock(root *os.Root, path string, euid int) (*os.File, error) {
	f, err := root.OpenFile(lockFile, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, fileMode)
	if err != nil {
		return nil, fmt.Errorf("state: lock %s: %w", path, err)
	}
	info, err := f.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = errors.New("is not a regular file")
	}
	if err == nil {
		err = fstrust.CheckOwnership(info, euid)
	}
	// flock needs only an open descriptor: a lock file others can read
	// lets them hold the lock and keep sentineld from starting.
	if err == nil && info.Mode().Perm()&0o077 != 0 {
		err = fmt.Errorf("is readable by group or others (mode %04o); chmod 600 it", info.Mode().Perm())
	}
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("state: lock file %s/%s %w", path, lockFile, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("state: state directory %s is in use by another sentineld", path)
		}
		return nil, fmt.Errorf("state: lock %s: %w", path, err)
	}
	return f, nil
}
