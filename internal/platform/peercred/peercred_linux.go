package peercred

import (
	"errors"
	"fmt"
	"net"
	"syscall"
	"unsafe"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/authz"
)

// soPeerGroups is SO_PEERGROUPS (Linux 4.13), which package syscall does not
// define: the supplementary groups the peer had when it connected.
const soPeerGroups = 59

// maxGroups bounds the supplementary groups read for one peer: the Linux
// limit (NGROUPS_MAX).
const maxGroups = 65536

// Read returns the uid, gid and supplementary groups of conn's peer, from
// SO_PEERCRED and SO_PEERGROUPS. Both are snapshots taken by the kernel at
// connect(): no race with the peer exiting or a pid being reused, and no
// dependency on the user database. A kernel without SO_PEERGROUPS is an
// error: the groups decide the operate and admin tiers.
func Read(conn *net.UnixConn) (authz.Peer, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return authz.Peer{}, fmt.Errorf("peercred: %w", err)
	}
	var peer authz.Peer
	var opErr error
	if err := raw.Control(func(fd uintptr) { peer, opErr = read(int(fd)) }); err != nil {
		return authz.Peer{}, fmt.Errorf("peercred: %w", err)
	}
	return peer, opErr
}

func read(fd int) (authz.Peer, error) {
	cred, err := syscall.GetsockoptUcred(fd, syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	if err != nil {
		return authz.Peer{}, fmt.Errorf("peercred: SO_PEERCRED: %w", err)
	}
	groups, err := peerGroups(fd)
	if err != nil {
		return authz.Peer{}, err
	}
	return authz.Peer{UID: cred.Uid, GID: cred.Gid, Groups: groups}, nil
}

// peerGroups reads SO_PEERGROUPS. The kernel answers ERANGE with the size
// it needs when the buffer is too small; the size is checked and bounded
// before the second try.
func peerGroups(fd int) ([]uint32, error) {
	capacity := uint32(64) // groups the buffer holds
	for range 2 {
		buf := make([]uint32, capacity)
		size := capacity * 4
		err := getsockopt(fd, soPeerGroups, unsafe.Pointer(&buf[0]), &size) //nolint:gosec // buf outlives the call; size bounds the kernel's write
		switch {
		case err == nil:
			if size%4 != 0 || size/4 > capacity {
				return nil, fmt.Errorf("peercred: SO_PEERGROUPS returned %d bytes", size)
			}
			return buf[:size/4], nil
		case errors.Is(err, syscall.ERANGE) && size%4 == 0 && size/4 <= maxGroups && size/4 > capacity:
			capacity = size / 4
		case errors.Is(err, syscall.ENOPROTOOPT):
			return nil, errors.New("peercred: the kernel does not report peer groups (SO_PEERGROUPS needs Linux 4.13)")
		default:
			return nil, fmt.Errorf("peercred: SO_PEERGROUPS: %w (size %d)", err, size)
		}
	}
	return nil, errors.New("peercred: SO_PEERGROUPS: group list kept growing")
}

// getsockopt is the raw system call: package syscall has no wrapper for an
// array-valued option. size is in bytes, in and out.
func getsockopt(fd, opt int, val unsafe.Pointer, size *uint32) error {
	_, _, errno := syscall.Syscall6(syscall.SYS_GETSOCKOPT, uintptr(fd), uintptr(syscall.SOL_SOCKET),
		uintptr(opt), uintptr(val), uintptr(unsafe.Pointer(size)), 0) //nolint:gosec // getsockopt(2) takes pointers to the caller's buffers
	if errno != 0 {
		return errno
	}
	return nil
}
