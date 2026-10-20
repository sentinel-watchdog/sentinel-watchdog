package executor

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/fstrust"
)

// Errors of OpenAppend; their text names no path.
var (
	ErrUnsafeOutputFile = errors.New("output file is not a regular file with one link, owned by root or sentineld's user and not writable by group or others")
	ErrUnsafeOutputMode = errors.New("output file mode must not be writable by group or others")
)

// OpenAppend opens the output file of a service (D-009 `file`) for
// appending, creating it with mode if needed. sentineld writes it as root,
// so it is opened like a trusted file: the directories on its path are
// checked (fstrust), the file is opened inside its directory without
// following a link and without blocking (a FIFO fails instead of hanging),
// and the opened file must be a regular file with a single link (a hard
// link would redirect root's writes to another file), owned by root or
// sentineld's user and not writable by group or others. An existing
// file's mode is never changed.
func OpenAppend(path string, mode fs.FileMode) (*os.File, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("output file path must be absolute")
	}
	if mode&0o022 != 0 || mode&^fs.ModePerm != 0 {
		return nil, ErrUnsafeOutputMode
	}
	euid := os.Geteuid()
	dir, err := fstrust.Resolve(filepath.Dir(path), euid)
	if err != nil {
		return nil, fmt.Errorf("output file directory: %w", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("output file directory: %w", err)
	}
	defer root.Close()
	info, err := root.Stat(".")
	if err != nil {
		return nil, fmt.Errorf("output file directory: %w", err)
	}
	if err := fstrust.CheckOwnership(info, euid); err != nil {
		return nil, fmt.Errorf("output file directory %w", err)
	}
	f, err := root.OpenFile(filepath.Base(path), os.O_WRONLY|os.O_APPEND|os.O_CREATE|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, mode)
	if err != nil {
		return nil, fmt.Errorf("open output file: %w", err)
	}
	st, err := f.Stat()
	if err == nil && (!st.Mode().IsRegular() || links(st) != 1 || fstrust.CheckOwnership(st, euid) != nil) {
		err = ErrUnsafeOutputFile
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// links returns the number of hard links of a file.
func links(info fs.FileInfo) uint64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Nlink) // uint16 on darwin, uint64 on linux
	}
	return 0
}
