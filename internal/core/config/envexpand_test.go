package config

import (
	"strings"
	"testing"
)

func TestExpandString(t *testing.T) {
	vars := env(map[string]string{
		"TOKEN": "s3cr3t",
		"HOST":  "example.org",
		"EMPTY": "",
		"NEST":  "${TOKEN}",
	})
	tests := []struct {
		in, want, wantErr string
	}{
		{"plain", "plain", ""},
		{"Bearer ${TOKEN}", "Bearer s3cr3t", ""},
		{"https://${HOST}/${TOKEN}", "https://example.org/s3cr3t", ""},
		{"${EMPTY}", "", ""},
		{"cost $5", "cost $5", ""},
		{"$HOST", "$HOST", ""},
		{"$${TOKEN}", "${TOKEN}", ""},
		{"${NEST}", "${TOKEN}", ""}, // not recursive
		{"${MISSING}", "", `"MISSING" is not set`},
		{"${1BAD}", "", "invalid variable name"},
		{"${}", "", "invalid variable name"},
		{"${TOKEN", "", "unterminated"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := expandString(tt.in, vars)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExpandStringReportsAllMissing(t *testing.T) {
	_, err := expandString("${A} ${B}", env(nil))
	if err == nil || !strings.Contains(err.Error(), `"A"`) || !strings.Contains(err.Error(), `"B"`) {
		t.Fatalf("expected both variables reported, got %v", err)
	}
}
