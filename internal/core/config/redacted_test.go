package config

import (
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
