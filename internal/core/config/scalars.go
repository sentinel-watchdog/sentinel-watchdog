package config

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"math"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Duration is a time.Duration written as a Go duration string ("15s",
// "10m", "2h"). Bare integers are rejected because their unit is ambiguous.
type Duration time.Duration

// Std returns d as a time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// String implements fmt.Stringer.
func (d Duration) String() string { return time.Duration(d).String() }

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode || n.ShortTag() != "!!str" {
		return fmt.Errorf("line %d: duration must be a string such as \"30s\" or \"5m\", got %q", n.Line, n.Value)
	}
	v, err := time.ParseDuration(strings.TrimSpace(n.Value))
	if err != nil {
		return fmt.Errorf("line %d: invalid duration %q: %w", n.Line, n.Value, err)
	}
	*d = Duration(v)
	return nil
}

// MarshalYAML implements yaml.Marshaler.
func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

// MarshalJSON implements json.Marshaler.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

// ByteSize is a size in bytes. YAML accepts a non-negative integer or a
// string with a unit: B, KB, MB, GB, TB (powers of 1000) or KiB, MiB, GiB,
// TiB (powers of 1024).
type ByteSize uint64

var byteUnits = []struct {
	suffix string
	mult   uint64
}{
	// Longest suffixes first so "MiB" is not parsed as "B".
	{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30}, {"TiB", 1 << 40},
	{"KB", 1e3}, {"MB", 1e6}, {"GB", 1e9}, {"TB", 1e12},
	{"B", 1},
}

// ParseByteSize parses "1073741824", "512MiB" or "1GB".
func ParseByteSize(s string) (ByteSize, error) {
	s = strings.TrimSpace(s)
	mult := uint64(1)
	num := s
	for _, u := range byteUnits {
		if strings.HasSuffix(s, u.suffix) {
			mult = u.mult
			num = strings.TrimSpace(strings.TrimSuffix(s, u.suffix))
			break
		}
	}
	n, err := strconv.ParseUint(num, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q: use an integer number of bytes or a unit such as 512MiB", s)
	}
	if n > math.MaxUint64/mult {
		return 0, fmt.Errorf("size %q overflows", s)
	}
	return ByteSize(n * mult), nil
}

// UnmarshalYAML implements yaml.Unmarshaler.
func (b *ByteSize) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: size must be a scalar", n.Line)
	}
	v, err := ParseByteSize(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*b = v
	return nil
}

// FileMode is a Unix permission mode written as a quoted octal string
// ("0660"). Integers are rejected: YAML 1.2 reads 0660 as decimal.
type FileMode fs.FileMode

// Perm returns the mode as fs.FileMode.
func (m FileMode) Perm() fs.FileMode { return fs.FileMode(m) }

// String renders the mode as a 4-digit octal string.
func (m FileMode) String() string { return fmt.Sprintf("%04o", uint32(m)) }

// UnmarshalYAML implements yaml.Unmarshaler.
func (m *FileMode) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode || n.ShortTag() != "!!str" {
		return fmt.Errorf("line %d: file mode must be a quoted octal string such as \"0640\"", n.Line)
	}
	v, err := strconv.ParseUint(strings.TrimPrefix(n.Value, "0o"), 8, 32)
	if err != nil || v > 0o777 {
		return fmt.Errorf("line %d: invalid file mode %q: expected octal permissions between 0000 and 0777", n.Line, n.Value)
	}
	*m = FileMode(v)
	return nil
}

// MarshalYAML implements yaml.Marshaler.
func (m FileMode) MarshalYAML() (any, error) { return m.String(), nil }

// MarshalJSON implements json.Marshaler.
func (m FileMode) MarshalJSON() ([]byte, error) { return json.Marshal(m.String()) }
