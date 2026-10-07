package config

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// checkOwnership enforces ADR-0015 rule 8 on a configuration file or
// directory: it must be owned by root or by the user running sentineld
// (euid), and must not be writable by group or others. Otherwise another
// local user could change what a root daemon executes.
func checkOwnership(info fs.FileInfo, euid int) error {
	if perm := info.Mode().Perm(); perm&0o022 != 0 {
		return fmt.Errorf("is writable by group or others (mode %04o); remove the write bits with chmod go-w", perm)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil // no owner information on this platform
	}
	if uid := int(st.Uid); uid != 0 && uid != euid {
		return fmt.Errorf("is owned by uid %d; it must be owned by root or by the user running sentineld (uid %d)", uid, euid)
	}
	return nil
}

// checkAncestors applies the ownership rule to every directory above dir,
// up to "/". dir must be absolute and free of symbolic links. A directory
// writable by group or others is accepted only with the sticky bit (such
// as /tmp): there, other users cannot rename or replace entries they do
// not own. Without this check, the owner of a parent directory could swap
// the configuration directory for another one between checks.
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
	writable := info.Mode().Perm()&0o022 != 0
	if writable && info.Mode()&fs.ModeSticky == 0 {
		return fmt.Errorf("is writable by group or others (mode %04o) without the sticky bit", info.Mode().Perm())
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		if uid := int(st.Uid); uid != 0 && uid != euid {
			return fmt.Errorf("is owned by uid %d; it must be owned by root or by the user running sentineld (uid %d)", uid, euid)
		}
	}
	return nil
}
