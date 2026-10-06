package config

import (
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestRedacted(t *testing.T) {
	cfg := mustLoad(t, `version: 1
notifications:
  - name: hook
    type: webhook
    url: https://hooks.example.org/services/${HOOK_SECRET}
    headers:
      Authorization: Bearer ${TOKEN}
supervisors:
  - name: api
    type: http
    url: https://user:${PASSWORD}@example.org/health?key=${API_KEY}
    method: POST
    body: '{"password":"${PASSWORD}"}'
  - name: job
    type: cron
    schedule: "@hourly"
    command: /usr/local/bin/job
    environment:
      DB_PASSWORD: ${PASSWORD}
      MODE: fast
`, map[string]string{
		"HOOK_SECRET": "hooksecret1",
		"TOKEN":       "tokensecret2",
		"PASSWORD":    "passwordsecret3",
		"API_KEY":     "apikeysecret4",
	})

	out, err := yaml.Marshal(cfg.Redacted())
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	for _, secret := range []string{"hooksecret1", "tokensecret2", "passwordsecret3", "apikeysecret4"} {
		if strings.Contains(text, secret) {
			t.Errorf("secret %q leaked:\n%s", secret, text)
		}
	}
	for _, kept := range []string{"MODE: fast", "Content-Type: application/json", "example.org", "/usr/local/bin/job"} {
		if !strings.Contains(text, kept) {
			t.Errorf("expected %q to be kept:\n%s", kept, text)
		}
	}

	// The original configuration must not be modified.
	ch, _ := cfg.Channel("hook")
	if !strings.Contains(ch.Headers["Authorization"], "tokensecret2") {
		t.Error("Redacted mutated the source configuration")
	}
}

func TestMarshalRoundTrip(t *testing.T) {
	cfg := mustLoad(t, baseConfig(`
  - name: svc
    type: systemd
    service: nginx.service
    recovery: {max_attempts: 3}
    notifications: [hook]
`), nil)
	out, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	again, err := parseBytes(out, env(nil))
	if err != nil {
		t.Fatalf("marshalled config does not parse strictly: %v\n%s", err, out)
	}
	if again.Supervisors[0].Systemd == nil || again.Supervisors[0].Systemd.Recovery.MaxAttempts != 3 {
		t.Errorf("round trip lost data:\n%s", out)
	}
}
