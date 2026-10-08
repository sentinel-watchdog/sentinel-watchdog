package notify

import (
	"context"
	"encoding/pem"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
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

// R7: a 3xx is a failure even if listed as a success code.
func TestRedirectIsNeverASuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "/elsewhere")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()
	ch := webhookChannel(srv.URL)
	ch.SuccessStatusCodes = []int{http.StatusFound}
	if err := newWebhook(t, ch).Send(context.Background(), nil); err == nil {
		t.Fatal("redirect counted as delivered")
	}
}

// R2: a CA "file" that is a FIFO must not hang NewWebhook. The check runs
// in a child process so that a hang cannot leave a goroutine behind.
func TestCABundleMustBeARegularFile(t *testing.T) {
	const marker = "SENTINEL_TEST_CA_FIFO"
	if path := os.Getenv(marker); path != "" {
		ch := webhookChannel("https://example.org")
		ch.TLS.CAFile = path
		if _, err := NewWebhook(ch); err == nil {
			t.Fatal("FIFO accepted as a CA bundle")
		}
		return
	}
	path := filepath.Join(t.TempDir(), "ca-fifo")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	t.Setenv(marker, path)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCABundleMustBeARegularFile$").CombinedOutput()
	if ctx.Err() != nil {
		t.Fatal("loading the CA bundle blocked")
	}
	if err != nil {
		t.Fatalf("child: %v\n%s", err, out)
	}
}

// The CA bundle's directory is checked too, and a link cannot lead it
// through an unchecked directory.
func TestCABundleOnUnsafePaths(t *testing.T) {
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	defer srv.Close()
	data := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	base := t.TempDir()
	safe, unsafeDir := filepath.Join(base, "safe"), filepath.Join(base, "unsafe")
	for _, dir := range []string{safe, unsafeDir} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	ca := filepath.Join(unsafeDir, "ca.pem")
	if err := os.WriteFile(ca, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unsafeDir, 0o777); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unsafeDir, 0o700) })
	link := filepath.Join(safe, "ca.pem")
	if err := os.Symlink("../unsafe/ca.pem", link); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{"writable directory": ca, "link through it": link} {
		t.Run(name, func(t *testing.T) {
			if _, err := loadCA(path); err == nil {
				t.Fatal("CA bundle on an unsafe path accepted")
			}
		})
	}
	// A link to a bundle in a safe directory is fine (RHEL's system bundle
	// is a link).
	good := filepath.Join(safe, "real.pem")
	if err := os.WriteFile(good, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.pem", filepath.Join(safe, "alias.pem")); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCA(filepath.Join(safe, "alias.pem")); err != nil {
		t.Fatalf("link to a safe bundle: %v", err)
	}
}
