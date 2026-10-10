//go:build !linux

package peercred

import (
	"net"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/authz"
)

// Read always fails outside Linux (ErrUnsupported).
func Read(*net.UnixConn) (authz.Peer, error) {
	return authz.Peer{}, ErrUnsupported
}
