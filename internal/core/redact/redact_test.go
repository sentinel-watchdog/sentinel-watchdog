package redact

import (
	"testing"
)

func TestIsSensitiveKey(t *testing.T) {
	tests := []struct {
		key  string
		want bool
	}{
		{"Authorization", true},
		{"X-Api-Key", true},
		{"api_key", true},
		{"DB_PASSWORD", true},
		{"SENTINEL_WEBHOOK_TOKEN", true},
		{"client_secret", true},
		{"Content-Type", false},
		{"APP_ENV", false},
		{"supervisor", false},
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			if got := IsSensitiveKey(tt.key); got != tt.want {
				t.Errorf("IsSensitiveKey(%q) = %v, want %v", tt.key, got, tt.want)
			}
		})
	}
}

func TestHeaders(t *testing.T) {
	got := Headers(map[string]string{
		"Content-Type":  "application/json",
		"Authorization": "Bearer abc",
		"X-Custom":      "value",
	})
	if got["Content-Type"] != "application/json" {
		t.Errorf("Content-Type redacted: %q", got["Content-Type"])
	}
	for _, k := range []string{"Authorization", "X-Custom"} {
		if got[k] != Placeholder {
			t.Errorf("%s not redacted: %q", k, got[k])
		}
	}
	if Headers(nil) != nil {
		t.Error("Headers(nil) should be nil")
	}
}

func TestEnvironment(t *testing.T) {
	got := Environment(map[string]string{"APP_ENV": "prod", "DB_PASSWORD": "pw"})
	if got["APP_ENV"] != "prod" || got["DB_PASSWORD"] != Placeholder {
		t.Errorf("unexpected redaction: %v", got)
	}
}

func TestURL(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"https://example.org/health", "https://example.org/health"},
		{"https://user:pw@example.org/x", "https://user:[REDACTED]@example.org/x"},
		// A user name alone is often a token (https://<token>@host).
		{"https://ghp_s3cret@github.com/x", "https://[REDACTED]@github.com/x"},
		{"https://example.org/x?token=abc&a=b", "https://example.org/x?a=[REDACTED]&token=[REDACTED]"},
		{"://bad", Placeholder},
	}
	for _, tt := range tests {
		if got := URL(tt.in); got != tt.want {
			t.Errorf("URL(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestWebhookURL(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"https://hooks.example.org/services/T000/B000/XXXX", "https://hooks.example.org/[REDACTED]"},
		{"https://hooks.example.org", "https://hooks.example.org"},
		{"https://hooks.example.org/?k=v", "https://hooks.example.org/[REDACTED]"},
		{"not a url", Placeholder},
	}
	for _, tt := range tests {
		if got := WebhookURL(tt.in); got != tt.want {
			t.Errorf("WebhookURL(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestText(t *testing.T) {
	got := Text("auth failed for token s3cr3t-value", "s3cr3t-value", "", "ab")
	if got != "auth failed for token [REDACTED]" {
		t.Errorf("Text = %q", got)
	}
}
