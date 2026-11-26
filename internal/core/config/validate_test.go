package config

import (
	"strings"
	"testing"
)

// B-027: header values may hold TAB but no other control byte, and names
// that differ only in case are one header.
func TestValidatorHeaders(t *testing.T) {
	tests := []struct {
		name    string
		headers map[string]string
		want    string // "" = no problem
	}{
		{"TAB", map[string]string{"X-A": "a\tb"}, ""},
		{"printable and non-ASCII", map[string]string{"X-A": "~ é"}, ""},
		{"NUL", map[string]string{"X-A": "a\x00b"}, "control character"},
		{"unit separator 0x1f", map[string]string{"X-A": "a\x1fb"}, "control character"},
		{"DEL 0x7f", map[string]string{"X-A": "a\x7fb"}, "control character"},
		{"LF", map[string]string{"X-A": "a\nX-B: b"}, "control character"},
		{"different names", map[string]string{"X-A": "a", "X-B": "b"}, ""},
		{"Host and host", map[string]string{"Host": "a", "host": "b"}, `header "host" repeats "Host": header names are case-insensitive; keep one`},
		{"X-A and x-a", map[string]string{"X-A": "a", "x-a": "b"}, `header "x-a" repeats "X-A"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := &validator{file: "f.yaml"}
			v.headers(loc{path: "h"}, tt.headers)
			errs := v.problems.errs
			switch {
			case tt.want == "" && len(errs) != 0:
				t.Errorf("unexpected problems %v", errs)
			case tt.want != "" && (len(errs) != 1 || !strings.Contains(errs[0].Message, tt.want)):
				t.Errorf("problems = %v, want one containing %q", errs, tt.want)
			}
		})
	}
}

// Most rules are exercised end to end by the loader tests; these cases
// cover branches a YAML file reaches only awkwardly.
func TestValidatorPathsAndURLs(t *testing.T) {
	tests := []struct {
		name  string
		check func(v *validator)
		want  string // "" = no problem
	}{
		{"clean absolute path", func(v *validator) { v.absPath(loc{path: "p"}, "/var/lib/sentinel") }, ""},
		{"empty path", func(v *validator) { v.absPath(loc{path: "p"}, "") }, "is required"},
		{"NUL in path", func(v *validator) { v.absPath(loc{path: "p"}, "/var/\x00x") }, "NUL byte"},
		{"relative path", func(v *validator) { v.absPath(loc{path: "p"}, "var/lib") }, "absolute path"},
		{"unclean path", func(v *validator) { v.absPath(loc{path: "p"}, "/var/lib/../x") }, `use "/var/x"`},
		{"https URL", func(v *validator) { v.httpURL(loc{path: "u"}, "https://hooks.example.org/x") }, ""},
		{"unparseable URL", func(v *validator) { v.httpURL(loc{path: "u"}, "https://exa mple.org:x/s3cret") }, "is not a valid URL"},
		{"other scheme", func(v *validator) { v.httpURL(loc{path: "u"}, "ftp://example.org") }, "scheme must be http or https"},
		{"missing host", func(v *validator) { v.httpURL(loc{path: "u"}, "https:///path") }, "host is missing"},
		{"port 1", func(v *validator) { v.httpURL(loc{path: "u"}, "https://x.org:1/s3cret") }, ""},
		{"port 65535", func(v *validator) { v.httpURL(loc{path: "u"}, "https://x.org:65535/s3cret") }, ""},
		{"empty port", func(v *validator) { v.httpURL(loc{path: "u"}, "https://x.org:/s3cret") }, ""},
		{"port 0", func(v *validator) { v.httpURL(loc{path: "u"}, "https://x.org:0/s3cret") }, "port is out of range (1 - 65535)"},
		{"port 65536", func(v *validator) { v.httpURL(loc{path: "u"}, "https://x.org:65536/s3cret") }, "port is out of range"},
		{"port beyond int", func(v *validator) { v.httpURL(loc{path: "u"}, "https://x.org:99999999999999999999/s3cret") }, "port is out of range"},
		{"root as a file or directory", func(v *validator) { v.absPath(loc{path: "p"}, "/") }, "is the root directory"},
		{"a directory under root", func(v *validator) { v.absPath(loc{path: "p"}, "/srv") }, ""},
		{"group name", func(v *validator) { v.account(loc{path: "g"}, "sentinel") }, ""},
		{"numeric group", func(v *validator) { v.account(loc{path: "g"}, "1001") }, ""},
		{"reserved group id", func(v *validator) { v.account(loc{path: "g"}, "4294967295") }, "invalid group"},     // (gid_t)-1
		{"group id out of range", func(v *validator) { v.account(loc{path: "g"}, "9999999999") }, "invalid group"}, // over 32 bits
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := &validator{file: "f.yaml"}
			tt.check(v)
			errs := v.problems.errs
			if tt.want == "" {
				if len(errs) != 0 {
					t.Fatalf("unexpected problems %v", errs)
				}
				return
			}
			if len(errs) != 1 || !strings.Contains(errs[0].Message, tt.want) {
				t.Fatalf("problems = %v, want one containing %q", errs, tt.want)
			}
			if strings.Contains(errs[0].Message, "s3cret") {
				t.Errorf("URL problem quotes the URL: %v", errs[0])
			}
		})
	}
}
