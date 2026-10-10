package transport

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/fstrust"
)

// parentMode is the mode of a socket directory sentineld creates: others
// may traverse it; the socket's own mode decides who connects.
const parentMode = 0o755

// probeTimeout bounds the connection attempt that tells a stale socket from
// a live one.
const probeTimeout = time.Second

// socket is the socket file, its checked parent directory and the lock
// that makes this daemon its only owner.
type socket struct {
	dir      *os.Root
	name     string
	path     string
	lock     *os.File
	listener *net.UnixListener
}

// listen creates the socket file at path:
//
//  1. its parent is checked like configuration (fstrust: owned by root or
//     the daemon user, not writable by group or others, on the whole
//     path), created if missing, and opened once;
//  2. <name>.lock in it is locked (flock): two daemons never race on the
//     next steps, and a live daemon's socket is never removed;
//  3. an existing entry is removed only if it is a socket nobody listens
//     on (a crashed daemon's); a live socket, a regular file, a directory
//     or a symbolic link is an error and stays;
//  4. after bind, the mode is narrowed to 0600, the group is set, then the
//     final mode: nobody outside the group can connect at any moment.
//     Between bind and the first chmod the socket has the process umask,
//     027 in sentineld, so only the owner can connect.
func listen(path string, mode fs.FileMode, gid int) (*socket, error) {
	euid := os.Geteuid()
	dir, err := fstrust.OpenDir(filepath.Dir(path), parentMode, euid)
	if err != nil {
		return nil, fmt.Errorf("transport: socket directory: %w", err)
	}
	s := &socket{dir: dir, name: filepath.Base(path), path: path}
	if err := s.acquire(euid); err != nil {
		s.release()
		return nil, err
	}
	if err := s.removeStale(); err != nil {
		s.release()
		return nil, err
	}
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		s.release()
		return nil, fmt.Errorf("transport: listen on %s: %w", path, err)
	}
	l.SetUnlinkOnClose(false) // close removes it, under the lock, if it is still a socket
	s.listener = l
	if err := s.setAccess(mode, gid); err != nil {
		_ = l.Close()
		return nil, errors.Join(err, s.close())
	}
	return s, nil
}

func (s *socket) acquire(euid int) error {
	name := s.name + ".lock"
	f, err := s.dir.OpenFile(name, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return fmt.Errorf("transport: lock %s: %w", s.path, err)
	}
	info, err := f.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = errors.New("is not a regular file")
	}
	if err == nil {
		err = fstrust.CheckOwnership(info, euid)
	}
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("transport: lock file %s.lock %w", s.path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return fmt.Errorf("transport: %s is in use by another sentineld", s.path)
		}
		return fmt.Errorf("transport: lock %s: %w", s.path, err)
	}
	s.lock = f
	return nil
}

func (s *socket) removeStale() error {
	info, err := s.dir.Lstat(s.name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("transport: %s: %w", s.path, err)
	}
	if info.Mode()&fs.ModeSocket == 0 {
		return fmt.Errorf("transport: %s exists and is not a socket (%s); it was not removed", s.path, info.Mode().Type())
	}
	conn, err := net.DialTimeout("unix", s.path, probeTimeout)
	if err == nil {
		_ = conn.Close()
		return fmt.Errorf("transport: another process is serving %s", s.path)
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Errorf("transport: %s: cannot tell whether it is in use, so it was not removed: %w", s.path, err)
	}
	if err := s.dir.Remove(s.name); err != nil {
		return fmt.Errorf("transport: remove stale socket %s: %w", s.path, err)
	}
	return nil
}

func (s *socket) setAccess(mode fs.FileMode, gid int) error {
	if err := s.dir.Chmod(s.name, 0o600); err != nil {
		return fmt.Errorf("transport: chmod %s: %w", s.path, err)
	}
	if gid >= 0 {
		if err := s.dir.Lchown(s.name, -1, gid); err != nil {
			return fmt.Errorf("transport: chown %s to gid %d: %w", s.path, gid, err)
		}
	}
	if err := s.dir.Chmod(s.name, mode.Perm()); err != nil {
		return fmt.Errorf("transport: chmod %s: %w", s.path, err)
	}
	return nil
}

// close removes the socket file if it is still a socket (the lock is held,
// so it is this daemon's) and releases the lock and the directory. The
// listener must be closed already.
func (s *socket) close() error {
	var err error
	if info, lerr := s.dir.Lstat(s.name); lerr == nil && info.Mode()&fs.ModeSocket != 0 {
		err = s.dir.Remove(s.name)
	}
	s.release()
	return err
}

// release unlocks (closing the lock file) and closes the directory. The
// lock file stays: removing it would let two daemons lock two files.
func (s *socket) release() {
	if s.lock != nil {
		_ = s.lock.Close()
	}
	_ = s.dir.Close()
}
