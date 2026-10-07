package config

import (
	"fmt"
	"io/fs"
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
