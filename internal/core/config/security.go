package config

// The filesystem side of the loader: links, bounded reads and directory
// listings. Ownership and path checks are in internal/core/fstrust.

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"strings"
	"syscall"
)

// maxLinkHops bounds how many symbolic links in a row checkLink follows.
const maxLinkHops = 8

// checkLink allows a symbolic link in r only when it names an entry of the
// same directory (a single path component, such as alpha.yaml ->
// alpha-v2.yaml). A target in another directory would make that directory
// part of the trusted path without checking it. A missing name is
// reported as fs.ErrNotExist.
func checkLink(r *os.Root, name string) error {
	for range maxLinkHops {
		info, err := r.Lstat(name)
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink == 0 {
			return nil
		}
		target, err := r.Readlink(name)
		if err != nil {
			return err
		}
		if target == "." || target == ".." || strings.ContainsRune(target, '/') {
			return fmt.Errorf("is a symbolic link to %q; a link may only name an entry of its own directory", target)
		}
		name = target
	}
	return fmt.Errorf("more than %d symbolic links in a row", maxLinkHops)
}

// errLimit reports that a file holds more bytes than readFile may read.
var errLimit = errors.New("read limit exceeded")

// readFile opens name inside root without blocking (a FIFO must not hang
// the loader), then checks the opened file before reading at most limit
// bytes: the file that is checked is the file that is read. More bytes
// than limit (the file may grow after the check) is errLimit. The bytes
// read are returned even with an error, so the caller can account for
// them.
func readFile(root *os.Root, name string, limit int, check func(os.FileInfo) error) ([]byte, error) {
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	if err := check(info); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil {
		return data, err
	}
	if len(data) > limit {
		return data, errLimit
	}
	return data, nil
}

// readDirNames returns the names in the root directory of r, sorted. The
// listing is read in batches and fails as soon as it has more than limit
// entries of any kind, before anything is sorted or inspected.
func readDirNames(r *os.Root, limit int) ([]string, error) {
	d, err := r.Open(".")
	if err != nil {
		return nil, err
	}
	defer d.Close()
	var names []string
	for {
		batch, err := d.Readdirnames(128)
		names = append(names, batch...)
		if len(names) > limit {
			return nil, fmt.Errorf("has more than %d entries", limit)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	slices.Sort(names)
	return names, nil
}
