package executor

import (
	"errors"
	"math"
	"strings"
	"testing"
	"testing/fstest"
)

func TestParseStatm(t *testing.T) {
	tests := []struct {
		name  string
		line  string
		want  uint64
		valid bool
	}{
		{"fields of a process", "6127 1536 1280 6 0 158 0\n", 1536, true},
		{"two fields are enough", "1 2", 2, true},
		{"largest value", "1 18446744073709551615", math.MaxUint64, true},
		{"one field", "6127\n", 0, false},
		{"empty", "", 0, false},
		{"negative", "1 -2", 0, false},
		{"not a number", "1 x", 0, false},
		{"too large", "1 18446744073709551616", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseStatm([]byte(tt.line))
			if (err == nil) != tt.valid || got != tt.want {
				t.Errorf("parseStatm(%q) = %d, %v", tt.line, got, err)
			}
		})
	}
}

func FuzzParseStatm(f *testing.F) {
	f.Add([]byte("6127 1536 1280 6 0 158 0\n"))
	f.Add([]byte("1 18446744073709551616"))
	f.Fuzz(func(t *testing.T, b []byte) {
		if _, err := parseStatm(b); err == nil && len(strings.Fields(string(b))) < 2 {
			t.Fatalf("accepted %q", b)
		}
	})
}

// D-084 (7): only the recorded leader is sampled; any other process at
// its pid, a zombie, or a missing directory is ErrGone, never zero.
func TestLeaderRSS(t *testing.T) {
	leader := statLine(100, "worker", 'S', 100, 50, 1000)
	tests := []struct {
		name  string
		stat  string // "": no such file
		statm string // "": no such file
		page  uint64
		want  uint64
		err   error // nil: success; errAny: any other error
	}{
		{"the leader", leader, "6127 1536 1280 6 0 158 0\n", 4096, 1536 * 4096, nil},
		{"another start time", statLine(100, "worker", 'S', 100, 50, 1001), "1 1", 4096, 0, ErrGone},
		{"another session", statLine(100, "worker", 'S', 100, 51, 1000), "1 1", 4096, 0, ErrGone},
		{"another group", statLine(100, "worker", 'S', 99, 50, 1000), "1 1", 4096, 0, ErrGone},
		{"another pid", statLine(101, "worker", 'S', 100, 50, 1000), "1 1", 4096, 0, ErrGone},
		{"a zombie", statLine(100, "worker", 'Z', 100, 50, 1000), "0 0", 4096, 0, ErrGone},
		{"ended before statm", leader, "", 4096, 0, ErrGone},
		{"no stat", "", "1 1", 4096, 0, ErrGone},
		{"stat unreadable", "garbage", "1 1", 4096, 0, errAny},
		{"statm unreadable", leader, "1", 4096, 0, errAny},
		{"statm too long", leader, strings.Repeat("1 ", maxStatmBytes), 4096, 0, errAny},
		{"overflow", leader, "1 18446744073709551615", 4096, 0, errAny},
		{"no page size", leader, "1 1", 0, 0, errAny},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := fstest.MapFS{}
			if tt.stat != "" {
				dir["stat"] = &fstest.MapFile{Data: []byte(tt.stat)}
			}
			if tt.statm != "" {
				dir["statm"] = &fstest.MapFile{Data: []byte(tt.statm)}
			}
			got, err := leaderRSS(dir, rec, tt.page)
			switch {
			case tt.err == nil && (err != nil || got != tt.want):
				t.Errorf("leaderRSS = %d, %v; want %d", got, err, tt.want)
			case errors.Is(tt.err, ErrGone) && !errors.Is(err, ErrGone):
				t.Errorf("err = %v, want ErrGone", err)
			case errors.Is(tt.err, errAny) && (err == nil || errors.Is(err, ErrGone)):
				t.Errorf("err = %v, want an error other than ErrGone", err)
			}
		})
	}
}

// errAny stands for any error but ErrGone in table tests.
var errAny = errors.New("any error")
