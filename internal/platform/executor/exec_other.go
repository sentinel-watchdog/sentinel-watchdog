//go:build !linux

package executor

import (
	"syscall"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/platform/privilege"
)

// sysProcAttr puts the child in its own process group. There is no
// parent-death signal outside Linux.
func sysProcAttr(c *privilege.Cred) (*syscall.SysProcAttr, error) {
	if c != nil {
		return nil, ErrUnsupported
	}
	return &syscall.SysProcAttr{Setpgid: true}, nil
}
