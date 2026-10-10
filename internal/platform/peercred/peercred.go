// Package peercred reads the identity of the process at the other end of a
// Unix socket, as the kernel recorded it when that process connected
// (ADR-0012). It is the platform side of the control socket's
// authorization: internal/core/authz decides, this package only reads.
package peercred

import "errors"

// ErrUnsupported reports a platform without peer credentials support.
// Sentinel's control plane runs on Linux; on other systems (development
// machines) every request is refused with this error.
var ErrUnsupported = errors.New("peer credentials are supported on Linux only")
