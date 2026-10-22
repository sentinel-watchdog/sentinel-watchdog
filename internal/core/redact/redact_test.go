package redact

import (
	"strings"
	"testing"
	"time"
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

// Review 3b-2 R4: secrets that overlap are masked as a whole, whatever
// their order, and every occurrence is masked.
func TestTextOverlaps(t *testing.T) {
	tests := []struct {
		s       string
		secrets []string
		want    string
	}{
		{"prefix-private-token", []string{"prefix-private", "private-token"}, "[REDACTED]"},
		{"prefix-private-token", []string{"private-token", "prefix-private"}, "[REDACTED]"},
		{"x prefix-private-token y", []string{"prefix", "prefix-private-token"}, "x [REDACTED] y"},
		{"aaaaaa", []string{"aaaa"}, "[REDACTED]"},
		{"tok1 and tok1", []string{"tok1"}, "[REDACTED] and [REDACTED]"},
		{"tok1tok2", []string{"tok1", "tok2"}, "[REDACTED]"},
		{"nothing here", []string{"secret"}, "nothing here"},
	}
	for _, tt := range tests {
		if got := Text(tt.s, tt.secrets...); got != tt.want {
			t.Errorf("Text(%q, %q) = %q, want %q", tt.s, tt.secrets, got, tt.want)
		}
	}
}

// FuzzText: whatever the text and two secrets, no secret of four bytes or
// more is left in what the result keeps of the text (the pieces between
// placeholders: a secret may be spelled by a kept piece and the
// placeholder's own characters, which reveals nothing).
func FuzzText(f *testing.F) {
	f.Add("prefix-private-token", "prefix-private", "private-token")
	f.Add("aaaaaaaa", "aaaa", "aaa")
	f.Fuzz(func(t *testing.T, s, a, b string) {
		got := Text(s, a, b)
		for _, sec := range []string{a, b} {
			if len(sec) < 4 {
				continue
			}
			for _, piece := range strings.Split(got, Placeholder) {
				if strings.Contains(piece, sec) {
					t.Fatalf("Text(%q, %q, %q) = %q keeps %q", s, a, b, got, sec)
				}
			}
		}
	})
}

// Review 3b-2 (fixes, R1): overlapping occurrences of a repetitive secret
// cost linear time, not len(s)×len(secret).
func TestTextRepetitiveWorkBudget(t *testing.T) {
	s := strings.Repeat("a", 2<<20)
	secret := strings.Repeat("a", 64<<10)
	start := time.Now()
	if got := Text(s+"b", secret, strings.Repeat("ab", 3)); got != Placeholder+"b" {
		t.Fatalf("unexpected result of %d bytes", len(got))
	}
	if d := time.Since(start); d > 5*time.Second { // quadratic work takes minutes
		t.Errorf("took %s", d)
	}
}
