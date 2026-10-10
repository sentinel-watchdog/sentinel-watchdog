package config

import (
	"fmt"
	"strings"
	"testing"
)

func TestRedacted(t *testing.T) {
	cfg := mustLoad(t, map[string]string{"sentinel.yaml": `
version: 1
notifications:
  channels:
    - name: w
      type: webhook
      url: https://hooks.example.org/services/T000/SECRET
      headers: {Authorization: Bearer tok, User-Agent: sentinel}
`}, nil)
	r := cfg.Redacted()
	ch := r.Notifications.Channels[0]
	if strings.Contains(ch.URL, "SECRET") || ch.Headers["Authorization"] == "Bearer tok" || ch.Headers["User-Agent"] != "sentinel" {
		t.Errorf("redacted channel = %+v", ch)
	}
	if orig := cfg.Notifications.Channels[0]; !strings.Contains(orig.URL, "SECRET") || orig.Headers["Authorization"] != "Bearer tok" {
		t.Error("Redacted modified the original configuration")
	}
}

// RedactText masks the values the configuration holds that may be
// secret: environment values expanded in any file, webhook URLs and
// header values, wherever they appear in a text (a module's error, for
// the daemon's log).
func TestRedactText(t *testing.T) {
	cfg := mustLoad(t, map[string]string{"sentinel.yaml": `version: 1
notifications:
  channels:
    - name: ops
      type: webhook
      url: https://hooks.example.com/T0/literal-path-token
      headers: {X-Api-Key: literal-header-key, Authorization: "Bearer ${TOKEN}"}
`}, map[string]string{"TOKEN": "env-token-123"})
	got := cfg.RedactText("failed: https://hooks.example.com/T0/literal-path-token key=literal-header-key Bearer env-token-123")
	for _, secret := range []string{"literal-path-token", "literal-header-key", "env-token-123"} {
		if strings.Contains(got, secret) {
			t.Errorf("%q not masked in %q", secret, got)
		}
	}
	if !strings.HasPrefix(got, "failed: ") {
		t.Errorf("text around the secrets lost: %q", got)
	}
	if strings.Contains(fmt.Sprintf("%+v", cfg.Redacted()), "env-token-123") {
		t.Error("the redacted view carries the secrets")
	}
}

// Review 2c-1 (second) R1: RedactText masks a secret quoted by %q and the
// whole of a secret that has another secret as its prefix.
func TestRedactTextQuotedAndOverlappingSecrets(t *testing.T) {
	tests := []struct {
		name    string
		secrets []string
		text    string
		want    string
	}{
		{"quoted", []string{`token\private-value`}, fmt.Sprintf("%q", `token\private-value`), `"[REDACTED]"`},
		{"overlapping", []string{"prefix-secret", "prefix-secret-suffix"}, "prefix-secret-suffix", "[REDACTED]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{secrets: tt.secrets}
			if got := cfg.RedactText(tt.text); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
