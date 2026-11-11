//go:build !linux

package journald

// Send is not implemented outside Linux: sentineld runs on Linux, and a
// development machine has no journald to send to.
func Send(string, []byte) error { return ErrUnsupported }
