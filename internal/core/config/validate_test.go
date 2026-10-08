package config

import (
	"strings"
	"testing"
)

// Most rules are exercised end to end by the loader tests; these cases
// cover branches a YAML file reaches only awkwardly.
func TestValidatorPathsAndURLs(t *testing.T) {
	tests := []struct {
		name  string
		check func(v *validator)
		want  string // "" = no problem
	}{
		{"clean absolute path", func(v *validator) { v.absPath("p", "/var/lib/sentinel") }, ""},
		{"empty path", func(v *validator) { v.absPath("p", "") }, "is required"},
		{"NUL in path", func(v *validator) { v.absPath("p", "/var/\x00x") }, "NUL byte"},
		{"relative path", func(v *validator) { v.absPath("p", "var/lib") }, "absolute path"},
		{"unclean path", func(v *validator) { v.absPath("p", "/var/lib/../x") }, `use "/var/x"`},
		{"https URL", func(v *validator) { v.httpURL("u", "https://hooks.example.org/x") }, ""},
		{"unparseable URL", func(v *validator) { v.httpURL("u", "https://exa mple.org:x/s3cret") }, "is not a valid URL"},
		{"other scheme", func(v *validator) { v.httpURL("u", "ftp://example.org") }, "scheme must be http or https"},
		{"missing host", func(v *validator) { v.httpURL("u", "https:///path") }, "host is missing"},
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
