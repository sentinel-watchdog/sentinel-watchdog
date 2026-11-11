// Package journald writes entries to the journal through journald's native
// socket, with fields of their own (D-100). It is a platform adapter: the
// supervisor decides what to send, this package encodes and sends.
//
// Send never waits: an entry journald cannot take at once is refused with
// ErrFull, and the caller decides whether to retry (the supervisor retries for
// at most 20 ms per line). It is supported on Linux only, where sentineld runs.
package journald

import (
	"encoding/binary"
	"errors"
)

// Socket is journald's native socket.
const Socket = "/run/systemd/journal/socket"

// ErrFull reports an entry journald's socket could not take at once
// (EAGAIN): the entry was not sent.
var ErrFull = errors.New("journald: socket full")

// ErrUnsupported is returned by Send outside Linux. Nothing should reach it:
// sentineld sends entries only when its stderr is journald's stream.
var ErrUnsupported = errors.New("journald: supported on Linux only")

// maxName is the longest field name journald stores.
const maxName = 64

// Field is one field of an entry. Name must satisfy ValidName.
type Field struct {
	Name  string
	Value string
}

// ValidName reports whether journald stores a field of this name from a
// client: upper-case ASCII letters, digits and '_', a letter first, at most 64
// bytes. A leading '_' marks the fields journald sets itself (_UID, _PID),
// which it ignores when a client sends them.
func ValidName(name string) bool {
	if name == "" || len(name) > maxName || name[0] < 'A' || name[0] > 'Z' {
		return false
	}
	for i := range len(name) {
		c := name[i]
		if (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
}

// Encode builds one entry of the native protocol from fields, whose names the
// caller has checked with ValidName. Every value takes the binary form: the
// name, a newline, the value's length as a little-endian 64-bit integer, the
// value and a newline. The plain form, NAME=value, ends a value at its first
// newline, and the rest would be read as more fields: a program's output could
// then set any field of its entry.
func Encode(fields []Field) []byte {
	size := 0
	for _, f := range fields {
		size += len(f.Name) + 1 + 8 + len(f.Value) + 1
	}
	entry := make([]byte, 0, size)
	for _, f := range fields {
		entry = append(entry, f.Name...)
		entry = append(entry, '\n')
		entry = binary.LittleEndian.AppendUint64(entry, uint64(len(f.Value)))
		entry = append(entry, f.Value...)
		entry = append(entry, '\n')
	}
	return entry
}
