package executor

import (
	"errors"
	"io/fs"
	"os"
	"strconv"
	"syscall"
)

// SamplesMemory reports whether LeaderRSS can sample on this system.
const SamplesMemory = true

// LeaderRSS returns the resident memory in bytes of the leader of g, a
// group recorded by Start on this boot (D-084 (7)). It is ErrGone once
// that process has ended, even if its pid names another process by then:
// /proc/<pid> is opened once as an os.Root and both of its files are read
// through it.
func (e *Executor) LeaderRSS(g Group) (uint64, error) {
	if err := e.bootErr; err != nil {
		return 0, err
	}
	if g.IsZero() || g.Boot != e.boot {
		return 0, ErrGone
	}
	root, err := os.OpenRoot("/proc/" + strconv.Itoa(g.Pgid))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			return 0, ErrGone
		}
		return 0, err
	}
	defer root.Close()
	return leaderRSS(root.FS(), g, uint64(os.Getpagesize())) //nolint:gosec // the page size is never negative
}
