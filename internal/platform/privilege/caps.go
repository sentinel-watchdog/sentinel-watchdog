package privilege

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strconv"
)

// Capability is a Linux capability number (capabilities(7)).
type Capability uint

// The capabilities the supervisor needs.
const (
	CapKill   Capability = 5 // signal processes of other users
	CapSetGID Capability = 6 // change gid and supplementary groups
	CapSetUID Capability = 7 // change uid
)

// Caps is a set of effective capabilities.
type Caps uint64

// Has reports whether c is in the set.
func (s Caps) Has(c Capability) bool { return c < 64 && s&(1<<c) != 0 }

// maxStatusBytes bounds what is read of /proc/<pid>/status (a few KiB on
// every kernel).
const maxStatusBytes = 64 << 10

// ErrNoCapEff is returned when the status has no valid CapEff line.
var ErrNoCapEff = errors.New("no valid CapEff line in the process status")

// ParseStatus reads the effective capabilities from a /proc/<pid>/status
// file: the line "CapEff:\t<16 hex digits>". At most maxStatusBytes are
// read.
func ParseStatus(r io.Reader) (Caps, error) {
	sc := bufio.NewScanner(io.LimitReader(r, maxStatusBytes))
	for sc.Scan() {
		value, ok := bytes.CutPrefix(sc.Bytes(), []byte("CapEff:"))
		if !ok {
			continue
		}
		v, err := strconv.ParseUint(string(bytes.TrimSpace(value)), 16, 64)
		if err != nil {
			return 0, ErrNoCapEff
		}
		return Caps(v), nil
	}
	return 0, ErrNoCapEff
}
