//go:build !linux

package privilege

import "errors"

// ErrUnsupported is returned outside Linux: capabilities are a Linux
// feature, and sentineld runs processes on Linux only (D-084); other
// systems are for development.
var ErrUnsupported = errors.New("capabilities are supported on Linux only")

// Current returns ErrUnsupported outside Linux.
func Current() (Caps, error) { return 0, ErrUnsupported }
