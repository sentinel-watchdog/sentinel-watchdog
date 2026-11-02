package systemd

import (
	"strings"
	"testing"
)

func TestParseUnitName(t *testing.T) {
	long := strings.Repeat("a", 247) // + ".service" = 255 bytes
	tests := []struct {
		in   string
		want UnitName
		ok   bool
	}{
		{"nginx.service", UnitName{Prefix: "nginx", Type: "service"}, true},
		{"getty@tty1.service", UnitName{Prefix: "getty", Instance: "tty1", Type: "service"}, true},
		{"foo@a@b.service", UnitName{Prefix: "foo", Instance: "a@b", Type: "service"}, true},
		{"foo..bar.service", UnitName{Prefix: "foo..bar", Type: "service"}, true},
		{`a\x2db.service`, UnitName{Prefix: `a\x2db`, Type: "service"}, true},
		{"dev-sda1.device", UnitName{Prefix: "dev-sda1", Type: "device"}, true},
		{"backup.timer", UnitName{Prefix: "backup", Type: "timer"}, true},
		{"a:b_c-d.service", UnitName{Prefix: "a:b_c-d", Type: "service"}, true},
		{long + ".service", UnitName{Prefix: long, Type: "service"}, true},

		{"", UnitName{}, false},
		{long + "a.service", UnitName{}, false}, // 256 bytes
		{"nginx", UnitName{}, false},            // no suffix
		{"nginx.", UnitName{}, false},
		{".service", UnitName{}, false},
		{"nginx.unknown", UnitName{}, false},
		{"foo@.service", UnitName{}, false}, // a template
		{"@foo.service", UnitName{}, false},
		{"a/b.service", UnitName{}, false},
		{"a b.service", UnitName{}, false},
		{"a\nb.service", UnitName{}, false},
		{"a\x00b.service", UnitName{}, false},
		{"nginx.Service", UnitName{}, false},
		{"ä.service", UnitName{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseUnitName(tt.in)
			if (err == nil) != tt.ok {
				t.Fatalf("ParseUnitName(%q) error = %v, want ok %v", tt.in, err, tt.ok)
			}
			if !tt.ok {
				return
			}
			if got != tt.want {
				t.Errorf("ParseUnitName(%q) = %+v, want %+v", tt.in, got, tt.want)
			}
			if got.String() != tt.in {
				t.Errorf("String() = %q, want %q", got.String(), tt.in)
			}
		})
	}
}

// validUnitBytes is an oracle written apart from ParseUnitName: the bytes
// systemd's unit-name.c accepts.
func validUnitBytes(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte(":-_.\\@", c) >= 0:
		default:
			return false
		}
	}
	return true
}

func FuzzParseUnitName(f *testing.F) {
	for _, s := range []string{"nginx.service", "getty@tty1.service", "foo@a@b.service", "foo@.service", "a/b.service", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		n, err := ParseUnitName(s)
		if err != nil {
			return
		}
		if len(s) > maxUnitName || !validUnitBytes(s) {
			t.Fatalf("accepted %q", s)
		}
		if n.String() != s {
			t.Fatalf("round trip %q → %q", s, n.String())
		}
		if n.Prefix == "" || strings.Contains(n.Prefix, "@") || n.Type == "" {
			t.Fatalf("bad parts %+v of %q", n, s)
		}
	})
}
