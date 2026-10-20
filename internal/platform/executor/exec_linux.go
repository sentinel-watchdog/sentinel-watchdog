package executor

import (
	"os"
	"slices"
	"syscall"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/platform/privilege"
)

// sysProcAttr puts the child in its own process group, kills it when the
// forking thread (sentineld) dies, and sets its identity. Go sets the
// parent-death signal after the identity change, so the kernel's reset of
// that signal on a credential change does not apply.
func sysProcAttr(c *privilege.Cred) (*syscall.SysProcAttr, error) {
	attr := &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	switch {
	case c != nil:
		// A nil Groups calls setgroups with no groups: supplementary groups
		// are dropped unless configured.
		attr.Credential = &syscall.Credential{Uid: c.UID, Gid: c.GID, Groups: slices.Clone(c.Groups)}
	case os.Geteuid() == 0:
		// Same identity as sentineld, without its supplementary groups.
		attr.Credential = &syscall.Credential{Uid: 0, Gid: uint32(os.Getegid())} //nolint:gosec // getegid is never negative
	}
	return attr, nil
}
