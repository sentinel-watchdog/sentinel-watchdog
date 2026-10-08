// Package fstrust holds the checks on files and directories that a root
// daemon trusts: configuration it reads and state it writes. Whoever can
// write one of them, or a directory on the way to it, could choose what
// sentineld does, so ownership and write bits are checked on every
// directory a path goes through (D-070, ADR-0011).
package fstrust

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// maxLinkHops bounds how many symbolic links Resolve follows in one path.
const maxLinkHops = 8

// CheckOwnership requires a file or directory the daemon trusts to be owned
// by root or by the user running sentineld (euid), and not writable by
// group or others. Otherwise another local user could change what a root
// daemon reads or writes (ADR-0015 rule 8, ADR-0011).
func CheckOwnership(info fs.FileInfo, euid int) error {
	if perm := info.Mode().Perm(); perm&0o022 != 0 {
		return fmt.Errorf("is writable by group or others (mode %04o); remove the write bits with chmod go-w", perm)
	}
	return checkOwner(info, euid)
}

// Resolve resolves the absolute path dir like filepath.EvalSymlinks,
// one component at a time, and applies the ancestor rule to every
// directory it looks into: the parents of the written path, every
// directory a symbolic link leads through, and the parents of the final
// directory. A directory writable by group or others is accepted only with
// the sticky bit (such as /tmp), where other users cannot rename or
// replace entries they do not own. Without these checks, whoever can write
// one of those directories could swap a link or a directory and choose
// what the root daemon reads or writes. The final directory itself is
// checked by the caller, on its opened descriptor (CheckOwnership).
func Resolve(dir string, euid int) (string, error) {
	resolved := "/"
	pending := strings.Split(filepath.Clean(dir), "/")
	links := 0
	for len(pending) > 0 {
		name := pending[0]
		pending = pending[1:]
		switch name {
		case "", ".":
			continue
		case "..":
			// Looking up ".." looks into the current directory too.
			if err := checkParentDir(resolved, euid); err != nil {
				return "", err
			}
			resolved = filepath.Dir(resolved)
			continue
		}
		if err := checkParentDir(resolved, euid); err != nil {
			return "", err
		}
		next := filepath.Join(resolved, name)
		info, err := os.Lstat(next)
		if err != nil {
			return "", err
		}
		if info.Mode()&fs.ModeSymlink == 0 {
			if !info.IsDir() {
				return "", fmt.Errorf("%s is not a directory", next)
			}
			resolved = next
			continue
		}
		if err := checkOwner(info, euid); err != nil {
			return "", fmt.Errorf("symbolic link %s %w", next, err)
		}
		if links++; links > maxLinkHops {
			return "", fmt.Errorf("more than %d symbolic links in %s", maxLinkHops, dir)
		}
		target, err := os.Readlink(next)
		if err != nil {
			return "", err
		}
		if filepath.IsAbs(target) {
			resolved = "/"
		}
		pending = append(strings.Split(target, "/"), pending...)
	}
	return resolved, nil
}

// checkParentDir applies the ancestor rule to directory dir.
func checkParentDir(dir string, euid int) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if err := checkAncestor(info, euid); err != nil {
		return fmt.Errorf("parent directory %s %w", dir, err)
	}
	return nil
}

// checkAncestor applies the ancestor rule to one directory or link.
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
