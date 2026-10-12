package audit

import (
	"crypto/rand"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strconv"
	"syscall"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/fstrust"
)

// Files in the state directory (ADR-0011 as amended by D-077).
const (
	activeFile = "audit.jsonl"
	headFile   = "audit_head.json"
	fileMode   = 0o600
	// maxHeadBytes bounds the head file; it holds two short points.
	maxHeadBytes = 4 << 10
	headVersion  = 1
)

// fileName returns the name of the i-th file: the active file for 0, a
// rotated file (1 the newest) otherwise.
func fileName(i int) string {
	if i == 0 {
		return activeFile
	}
	return activeFile + "." + strconv.Itoa(i)
}

// point is a position in the chain: a record's sequence number and hash.
type point struct {
	Seq  uint64 `json:"seq"`
	Hash string `json:"hash"`
}

// head is persisted after every record. Last is the last record written;
// Anchor is the record just before the oldest retained one (the genesis
// point until the first file expires), so the removal of a retained file
// is told apart from expiry.
type head struct {
	Version int   `json:"version"`
	Last    point `json:"last"`
	Anchor  point `json:"anchor"`
}

var genesis = point{Seq: 0, Hash: genesisHash}

var errNotRegular = errors.New("is not a regular file")

// openFile opens name in root without following a symbolic link, without
// blocking on a FIFO, and checks the opened file: regular, owned by root
// or euid, not writable by group or others. A missing file is reported as
// fs.ErrNotExist.
func openFile(root *os.Root, name string, flag int, euid int) (*os.File, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s %w", name, errNotRegular)
	}
	f, err := root.OpenFile(name, flag|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	info, err = f.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = fmt.Errorf("%s %w", name, errNotRegular)
	}
	if err == nil {
		if cerr := fstrust.CheckOwnership(info, euid); cerr != nil {
			err = fmt.Errorf("%s %w", name, cerr)
		}
	}
	if err != nil {
		_ = f.Close() // the check error is the one to report
		return nil, err
	}
	return f, nil
}

// createFile creates name, which must not exist, with mode 0600.
func createFile(root *os.Root, name string) (*os.File, error) {
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_APPEND|syscall.O_NOFOLLOW, fileMode)
	if err != nil {
		return nil, err
	}
	// The mode passed to OpenFile is reduced by the umask: set it exactly.
	if err := f.Chmod(fileMode); err != nil {
		_ = f.Close() // the chmod error is the one to report
		return nil, err
	}
	return f, nil
}

// readHead reads the head file strictly. It reports fs.ErrNotExist when
// there is none; any other problem means the head cannot be trusted.
func readHead(root *os.Root, euid int) (head, error) {
	f, err := openFile(root, headFile, os.O_RDONLY, euid)
	if err != nil {
		return head{}, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxHeadBytes+1))
	if err != nil {
		return head{}, err
	}
	if len(data) > maxHeadBytes {
		return head{}, errors.New("too large")
	}
	var h head
	if err := json.Unmarshal(data, &h, json.RejectUnknownMembers(true)); err != nil {
		return head{}, err
	}
	switch {
	case h.Version != headVersion:
		return head{}, fmt.Errorf("unsupported version %d", h.Version)
	case !validHash(h.Last.Hash) || !validHash(h.Anchor.Hash) || h.Anchor.Seq > h.Last.Seq:
		return head{}, errors.New("invalid points")
	}
	return h, nil
}

// writeHead replaces the head file atomically: temporary file, fsync,
// rename, fsync of the directory.
func writeHead(root *os.Root, h head) (err error) {
	h.Version = headVersion
	data, err := json.Marshal(h)
	if err != nil {
		return err
	}
	tmp := "." + headFile + "." + rand.Text() + ".tmp"
	f, err := createFile(root, tmp)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = f.Close()        // already failing; the first error is reported
			_ = root.Remove(tmp) // best effort: a leftover .tmp is harmless
		}
	}()
	if _, err = f.Write(append(data, '\n')); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = root.Rename(tmp, headFile); err != nil {
		return err
	}
	return syncDir(root)
}

// syncDir makes renames, creations and removals in root durable.
func syncDir(root *os.Root) error {
	d, err := root.Open(".")
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// exists reports whether name exists in root, without following a link.
func exists(root *os.Root, name string) (bool, error) {
	_, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}
