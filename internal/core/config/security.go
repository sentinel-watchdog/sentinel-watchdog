package config

// The filesystem trust boundary of the loader: every check on files and
// directories a root daemon reads lives in this file (ADR-0015 rule 8,
// D-070).

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// maxLinkHops bounds how many symbolic links in a row checkLink follows.
const maxLinkHops = 8

// checkOwnership enforces ADR-0015 rule 8 on a configuration file or
// directory: it must be owned by root or by the user running sentineld
// (euid), and must not be writable by group or others. Otherwise another
// local user could change what a root daemon executes.
func checkOwnership(info fs.FileInfo, euid int) error {
	if perm := info.Mode().Perm(); perm&0o022 != 0 {
		return fmt.Errorf("is writable by group or others (mode %04o); remove the write bits with chmod go-w", perm)
	}
	return checkOwner(info, euid)
}

// checkAncestors applies the ownership rule to every directory above dir,
// up to "/". dir must be absolute. A directory writable by group or others
// is accepted only with the sticky bit (such as /tmp): there, other users
// cannot rename or replace entries they do not own. Without this check,
// the owner of a parent directory could swap the configuration directory
// for another one between checks.
func checkAncestors(dir string, euid int) error {
	for p := filepath.Dir(dir); ; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if err := checkAncestor(info, euid); err != nil {
			return fmt.Errorf("parent directory %s %w", p, err)
		}
		if p == filepath.Dir(p) {
			return nil
		}
	}
}

func checkAncestor(info fs.FileInfo, euid int) error {
	// A symbolic link's own mode is always 0777 and means nothing: who can
	// replace it is decided by its parent, which is checked too.
	isLink := info.Mode()&fs.ModeSymlink != 0
	writable := info.Mode().Perm()&0o022 != 0
	if !isLink && writable && info.Mode()&fs.ModeSticky == 0 {
		return fmt.Errorf("is writable by group or others (mode %04o) without the sticky bit", info.Mode().Perm())
	}
	return checkOwner(info, euid)
}

// checkOwner requires info to be owned by root or by euid. Without owner
// information the check fails: the daemon runs on Linux, where it is
// always available, and a missing check must not look like a pass.
func checkOwner(info fs.FileInfo, euid int) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("has no owner information on this platform")
	}
	if uid := int(st.Uid); uid != 0 && uid != euid {
		return fmt.Errorf("is owned by uid %d; it must be owned by root or by the user running sentineld (uid %d)", uid, euid)
	}
	return nil
}

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

// readFile opens name inside root without blocking (a FIFO must not hang
// the loader), then checks the opened file before reading at most
// maxFileSize bytes: the file that is checked is the file that is read.
func readFile(root *os.Root, name string, check func(os.FileInfo) error) ([]byte, error) {
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
	data, err := io.ReadAll(io.LimitReader(f, maxFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxFileSize {
		return nil, fmt.Errorf("file exceeds %d bytes", maxFileSize)
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
