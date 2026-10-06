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

func TestEnvExpansionInConfig(t *testing.T) {
	cfg := mustLoad(t, baseConfig(`
  - name: svc
    type: systemd
    service: ${UNIT}
    recovery:
      max_attempts: ${ATTEMPTS}
`), map[string]string{"UNIT": "nginx.service", "ATTEMPTS": "7"})
	m, _ := cfg.Monitor("svc")
	if m.Systemd.Service != "nginx.service" {
		t.Errorf("service = %q", m.Systemd.Service)
	}
	if m.Systemd.Recovery.MaxAttempts != 7 {
		t.Errorf("plain scalar not re-resolved as int: %d", m.Systemd.Recovery.MaxAttempts)
	}
}

func TestEnvExpansionDoesNotInjectStructure(t *testing.T) {
	// A value containing YAML syntax must stay a single string.
	cfg := mustLoad(t, baseConfig(`
  - name: api
    type: http
    url: https://example.org/health
    method: POST
    body: ${EVIL}
`), map[string]string{"EVIL": "x\nmonitors: []\nfoo: [bar"})
	m, _ := cfg.Monitor("api")
	if m.HTTP.Body != "x\nmonitors: []\nfoo: [bar" {
		t.Fatalf("unexpected value %q", m.HTTP.Body)
	}
}

func TestEnvExpansionNotInKeys(t *testing.T) {
	_, err := loadString(t, baseConfig(`
  - name: api
    type: http
    url: https://example.org/health
    ${KEY}: value
`), map[string]string{"KEY": "method"})
	requireProblem(t, err, `unknown field "${KEY}"`)
}

func TestEnvExpansionUndefinedFails(t *testing.T) {
	_, err := loadString(t, baseConfig(`
  - name: svc
    type: systemd
    service: ${UNDEFINED_UNIT}
`), nil)
	requireProblem(t, err, "UNDEFINED_UNIT", "is not set", "line 10")
}
