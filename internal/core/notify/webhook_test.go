package notify

import (
	"context"
	"encoding/pem"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/config"
)

// webhookChannel returns a channel for url with the configuration defaults
// the loader would apply.
func webhookChannel(url string) config.Channel {
	return config.Channel{
		Name: "ops", Type: config.ChannelWebhook, URL: url, Method: http.MethodPost,
		Timeout: config.Duration(2 * time.Second),
		Headers: map[string]string{"Content-Type": "application/json", "Authorization": "Bearer s3cret"},
	}
}

func newWebhook(t *testing.T, ch config.Channel) *Webhook {
	t.Helper()
	w, err := NewWebhook(ch)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestWebhookDelivers(t *testing.T) {
	var got *http.Request
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, body = r, must(io.ReadAll(r.Body))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	if err := newWebhook(t, webhookChannel(srv.URL+"/hook")).Send(context.Background(), []byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	if got.Method != http.MethodPost || got.URL.Path != "/hook" || string(body) != `{"a":1}` ||
		got.Header.Get("Authorization") != "Bearer s3cret" || got.Header.Get("User-Agent") != userAgent {
		t.Errorf("request %s %s %q, headers %v", got.Method, got.URL.Path, body, got.Header)
	}
}

func TestWebhookStatusCodes(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		success []int
		want    string // "" = delivered
	}{
		{"2xx by default", http.StatusAccepted, nil, ""},
		{"error status", http.StatusInternalServerError, nil, "answered 500"},
		{"listed code", http.StatusOK, []int{http.StatusOK}, ""},
		{"unlisted 2xx", http.StatusAccepted, []int{http.StatusOK}, "answered 202"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()
			ch := webhookChannel(srv.URL)
			ch.SuccessStatusCodes = tt.success
			err := newWebhook(t, ch).Send(context.Background(), nil)
			if tt.want == "" && err != nil || tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)) {
				t.Errorf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

// A redirect could carry the Authorization header to another host.
func TestWebhookDoesNotFollowRedirects(t *testing.T) {
	var reached atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached.Store(true) }))
	defer other.Close()
	srv := httptest.NewServer(http.RedirectHandler(other.URL, http.StatusTemporaryRedirect))
	defer srv.Close()
	err := newWebhook(t, webhookChannel(srv.URL)).Send(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "redirects are not followed") || reached.Load() {
		t.Errorf("err = %v, redirect target reached = %v", err, reached.Load())
	}
}

// Errors never quote the URL, which often holds a token.
func TestWebhookErrorsDoNotQuoteTheURL(t *testing.T) {
	block := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-block }))
	defer slow.Close()
	defer close(block) // runs first: Close waits for the blocked handler
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close() // connections are refused

	tests := []struct{ name, url, want string }{
		{"timeout", slow.URL + "/hooks/s3cret-token", "timed out"},
		{"connection refused", closed.URL + "/hooks/s3cret-token", "request failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ch := webhookChannel(tt.url)
			ch.Timeout = config.Duration(100 * time.Millisecond)
			err := newWebhook(t, ch).Send(context.Background(), nil)
			if err == nil || !strings.Contains(err.Error(), tt.want) || strings.Contains(err.Error(), "s3cret") {
				t.Errorf("err = %v, want %q without the URL", err, tt.want)
			}
		})
	}
}

func TestWebhookTLS(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0) // the rejected handshake is expected
	srv.StartTLS()
	defer srv.Close()
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	pemData := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(caFile, pemData, 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		configure func(*config.Channel)
		ok        bool
	}{
		{"unknown CA", func(*config.Channel) {}, false},
		{"CA bundle", func(c *config.Channel) { c.TLS.CAFile = caFile }, true},
		{"verification disabled", func(c *config.Channel) { c.TLS.InsecureSkipVerify = true }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ch := webhookChannel(srv.URL)
			tt.configure(&ch)
			err := newWebhook(t, ch).Send(context.Background(), nil)
			if (err == nil) != tt.ok {
				t.Errorf("err = %v, want success %v", err, tt.ok)
			}
		})
	}
}

func TestWebhookRejectsBadCABundles(t *testing.T) {
	dir := t.TempDir()
	notPEM := filepath.Join(dir, "x.pem")
	if err := os.WriteFile(notPEM, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{"missing": filepath.Join(dir, "missing.pem"), "not PEM": notPEM} {
		t.Run(name, func(t *testing.T) {
			ch := webhookChannel("https://example.org")
			ch.TLS.CAFile = path
			if _, err := NewWebhook(ch); err == nil {
				t.Error("NewWebhook accepted the CA bundle")
			}
		})
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
