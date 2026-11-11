package journald

import (
	"bytes"
	"encoding/binary"
	"errors"
	"slices"
	"strings"
	"testing"
)

// decode parses an entry as journald's native protocol does
// (journald-native.c, systemd 239 to 259): a field is NAME=value\n, or
// NAME\n, a little-endian 64-bit length, the value and \n. It is written from
// the protocol, not from Encode, so that the two can disagree.
func decode(entry []byte) ([]Field, error) {
	var fields []Field
	for len(entry) > 0 {
		nl := bytes.IndexByte(entry, '\n')
		if nl < 0 {
			return nil, errors.New("a field without its end")
		}
		if eq := bytes.IndexByte(entry[:nl], '='); eq >= 0 {
			fields = append(fields, Field{Name: string(entry[:eq]), Value: string(entry[eq+1 : nl])})
			entry = entry[nl+1:]
			continue
		}
		name := string(entry[:nl])
		entry = entry[nl+1:]
		if len(entry) < 8 {
			return nil, errors.New("a binary field without its length")
		}
		n := binary.LittleEndian.Uint64(entry)
		entry = entry[8:]
		if uint64(len(entry)) < n+1 || entry[n] != '\n' {
			return nil, errors.New("a binary field shorter than its length")
		}
		fields = append(fields, Field{Name: name, Value: string(entry[:n])})
		entry = entry[n+1:]
	}
	return fields, nil
}

// Every value survives the protocol whole, whatever bytes it holds: a value
// ending a field at a newline would let a program's line add fields.
func TestEncodeRoundTrip(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{"plain", "server started"},
		{"empty", ""},
		{"newline", "a\nSENTINEL_SOURCE=other"},
		{"control bytes", "\x00\x01\x1b[31m\r"},
		{"invalid UTF-8", "\xff\xfe"},
		{"equals sign", "a=b"},
		{"long", strings.Repeat("x", 5000)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := []Field{{"MESSAGE", tc.value}, {"SENTINEL_SOURCE", "web"}}
			got, err := decode(Encode(in))
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, in) {
				t.Errorf("decoded %q, want %q", got, in)
			}
		})
	}
}

func FuzzEncode(f *testing.F) {
	for _, s := range []string{"", "line", "a\nB=c", "\x00\xff", "X\n\x05\x00\x00\x00\x00\x00\x00\x00hello\n"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, message string) {
		in := []Field{{"MESSAGE", message}, {"SENTINEL_SOURCE", "web"}, {"SENTINEL_STREAM", "stdout"}}
		got, err := decode(Encode(in))
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, in) {
			t.Fatalf("decoded %q, want %q", got, in)
		}
	})
}

// ValidName holds what journald stores from a client: upper-case letters,
// digits and '_', a letter first (a leading '_' is a trusted field, which
// journald ignores from a client), at most 64 bytes.
func TestValidName(t *testing.T) {
	tests := []struct {
		name string
		ok   bool
	}{
		{"MESSAGE", true},
		{"SENTINEL_RUN_ID", true},
		{"A", true},
		{"A1_", true},
		{strings.Repeat("A", 64), true},
		{strings.Repeat("A", 65), false},
		{"", false},
		{"_UID", false},
		{"1A", false},
		{"message", false},
		{"A-B", false},
		{"A=B", false},
		{"A\nB", false},
		{"A\x00", false},
		{"É", false},
	}
	for _, tc := range tests {
		if got := ValidName(tc.name); got != tc.ok {
			t.Errorf("ValidName(%q) = %v, want %v", tc.name, got, tc.ok)
		}
	}
}
