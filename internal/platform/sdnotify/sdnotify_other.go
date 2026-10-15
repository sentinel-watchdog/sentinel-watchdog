//go:build !linux

package sdnotify

// abstractNames: "@name" would be a relative path here, not an abstract
// socket, so Parse rejects it.
const abstractNames = false

// send is not implemented outside Linux: sentineld runs on Linux, and a
// development machine has no systemd to notify.
func send(string, []byte) error { return ErrUnsupported }
