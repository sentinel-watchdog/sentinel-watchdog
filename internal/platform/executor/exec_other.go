//go:build !linux

package executor

import (
	"errors"
	"syscall"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/platform/privilege"
)

// ErrUnsupported is returned for an identity change outside Linux:
// sentineld runs processes on Linux only (D-084); other systems are for
// development.
var ErrUnsupported = errors.New("executor: running as another user is supported on Linux only")

// sysProcAttr puts the child in its own process group. There is no
// parent-death signal outside Linux.
func sysProcAttr(c *privilege.Cred) (*syscall.SysProcAttr, error) {
	if c != nil {
		return nil, ErrUnsupported
	}
	return &syscall.SysProcAttr{Setpgid: true}, nil
}
