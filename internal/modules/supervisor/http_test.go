package supervisor

import (
	"context"
	"crypto/tls"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/config"
)

// get is the HTTP spec of a plain GET.
func get() HTTP { return HTTP{Method: http.MethodGet, TLSMinVersion: tls.VersionTLS12} }

// checkOnce builds the checker of rawURL with spec and runs one check.
func checkOnce(t *testing.T, rawURL string, spec HTTP) Result {
	t.Helper()
	return checkCtx(t, context.Background(), rawURL, spec)
}

func checkCtx(t *testing.T, ctx context.Context, rawURL string, spec HTTP) Result {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	c, err := newHTTPChecker(Service{Name: "web", Type: "http", URL: u, HTTP: spec})
	if err != nil {
		t.Fatalf("newHTTPChecker: %v", err)
	}
	return c.Check(ctx)
}

// caFile writes the certificate of a TLS test server as a PEM bundle.
func caFile(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.pem")
	data := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// D-082: the request carries the method, headers and body of the
// configuration; the status and the body decide the result.
func TestHTTPCheckRequestAndExpectations(t *testing.T) {
	type seen struct {
		method, body, ua, token string
		close                   bool
	}
	var (
		mu  sync.Mutex
		got seen
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = seen{r.Method, string(body), r.UserAgent(), r.Header.Get("X-Token"), r.Close}
		mu.Unlock()
		switch r.URL.Path {
		case "/created":
			w.WriteHeader(http.StatusCreated)
		case "/fail":
			w.WriteHeader(http.StatusInternalServerError)
		case "/moved":
			http.Redirect(w, r, "/", http.StatusMovedPermanently)
		case "/big":
			_, _ = io.WriteString(w, strings.Repeat("x", maxBodyRead)+"needle")
		default:
			_, _ = io.WriteString(w, `{"status":"ok"}`)
		}
	}))
	defer srv.Close()
	with := func(f func(*HTTP)) HTTP { h := get(); f(&h); return h }
	tests := []struct {
		name, path string
		spec       HTTP
		want       Failure
		seen       seen
	}{
		{"GET", "/", get(), "", seen{method: "GET", ua: userAgent, close: true}},
		{"HEAD", "/", with(func(h *HTTP) { h.Method = "HEAD" }), "", seen{method: "HEAD", ua: userAgent, close: true}},
		{"POST with headers and body", "/created", with(func(h *HTTP) {
			h.Method, h.Body, h.SuccessCodes = "POST", `{"probe":1}`, []int{201}
			h.Headers = map[string]string{"X-Token": "t0k", "User-Agent": "probe/1"}
		}), "", seen{method: "POST", body: `{"probe":1}`, ua: "probe/1", token: "t0k", close: true}},
		{"PUT", "/", with(func(h *HTTP) { h.Method, h.Body = "PUT", "b" }), "", seen{method: "PUT", body: "b", ua: userAgent, close: true}},
		{"5xx", "/fail", get(), FailureStatus, seen{method: "GET", ua: userAgent, close: true}},
		{"code not listed", "/created", with(func(h *HTTP) { h.SuccessCodes = []int{200, 204} }), FailureStatus, seen{method: "GET", ua: userAgent, close: true}},
		{"redirect not followed by default", "/moved", get(), FailureStatus, seen{method: "GET", ua: userAgent, close: true}},
		{"redirect expected", "/moved", with(func(h *HTTP) { h.SuccessCodes = []int{301} }), "", seen{method: "GET", ua: userAgent, close: true}},
		{"body contains", "/", with(func(h *HTTP) { h.BodyContains = `"status":"ok"` }), "", seen{method: "GET", ua: userAgent, close: true}},
		{"body does not contain", "/", with(func(h *HTTP) { h.BodyContains = "down" }), FailureBody, seen{method: "GET", ua: userAgent, close: true}},
		{"needle past the read limit", "/big", with(func(h *HTTP) { h.BodyContains = "needle" }), FailureBody, seen{method: "GET", ua: userAgent, close: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if r := checkOnce(t, srv.URL+tt.path, tt.spec); r.Failure != tt.want {
				t.Errorf("failure %q, want %q", r.Failure, tt.want)
			}
			mu.Lock()
			defer mu.Unlock()
			if got != tt.seen {
				t.Errorf("server saw %+v, want %+v", got, tt.seen)
			}
		})
	}
}

// D-080 (4): redirects are followed only when asked, within the origin
// and for at most maxRedirects hops; another origin never sees the
// request, its headers or its body.
func TestHTTPCheckRedirects(t *testing.T) {
	var foreign atomic.Int64
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { foreign.Add(1) }))
	defer other.Close()
	var replayed atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/hops/"):
			var n int
			_, _ = fmt.Sscanf(r.URL.Path, "/hops/%d", &n)
			if n > 0 {
				http.Redirect(w, r, fmt.Sprintf("/hops/%d", n-1), http.StatusFound)
			}
		case r.URL.Path == "/away":
			http.Redirect(w, r, other.URL+"/", http.StatusTemporaryRedirect)
		case r.URL.Path == "/https":
			http.Redirect(w, r, "https://"+r.Host+"/", http.StatusFound)
		case r.URL.Path == "/again":
			http.Redirect(w, r, "/landing", http.StatusPermanentRedirect)
		case r.URL.Path == "/landing":
			body, _ := io.ReadAll(r.Body)
			replayed.Store(string(body))
		}
	}))
	defer srv.Close()
	follow := get()
	follow.FollowRedirects = true
	post := follow
	post.Method, post.Body, post.Headers = "POST", "secret-body", map[string]string{"Authorization": "Bearer t0k"}
	tests := []struct {
		name, path string
		spec       HTTP
		want       Failure
	}{
		{"same origin", "/hops/1", follow, ""},
		{"five hops", fmt.Sprintf("/hops/%d", maxRedirects), follow, ""},
		{"six hops", fmt.Sprintf("/hops/%d", maxRedirects+1), follow, FailureRedirects},
		{"another host and port", "/away", follow, FailureRedirectRefused},
		{"another scheme", "/https", follow, FailureRedirectRefused},
		{"POST to another origin", "/away", post, FailureRedirectRefused},
		{"not followed", "/hops/1", get(), FailureStatus},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if r := checkOnce(t, srv.URL+tt.path, tt.spec); r.Failure != tt.want {
				t.Errorf("failure %q, want %q", r.Failure, tt.want)
			}
		})
	}
	if n := foreign.Load(); n != 0 {
		t.Errorf("another origin received %d requests", n)
	}
	// A 308 within the origin replays the body, as the method requires.
	if r := checkOnce(t, srv.URL+"/again", post); !r.OK() || replayed.Load() != "secret-body" {
		t.Errorf("308: %q, body %v", r.Failure, replayed.Load())
	}
}

// D-080 (4): the proxy environment is ignored, so a check never goes
// through a proxy its configuration does not name. Asserted on the
// transport: a request through an environment proxy would need a
// non-loopback host, so DNS, which unit tests do not use.
func TestHTTPCheckIgnoresProxyEnvironment(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://proxy.example:3128")
	t.Setenv("HTTPS_PROXY", "http://proxy.example:3128")
	for _, rawURL := range []string{"http://probe.example/", "https://probe.example/"} {
		u, _ := url.Parse(rawURL)
		c, err := newHTTPChecker(Service{Name: "web", Type: "http", URL: u, HTTP: get()})
		if err != nil {
			t.Fatal(err)
		}
		if tr := c.(*httpChecker).client.Transport.(*http.Transport); tr.Proxy != nil {
			t.Errorf("%s: the transport has a proxy function", rawURL)
		}
	}
}

// address connects to the given IP while Host, SNI and the certificate
// follow the URL (httptest's certificate is for example.com).
func TestHTTPCheckAddress(t *testing.T) {
	var host atomic.Value
	srv := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { host.Store(r.Host) }))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	spec := get()
	spec.Address = srv.Listener.Addr().String()
	spec.TLS = config.TLSConfig{CAFile: caFile(t, srv)}
	if r := checkOnce(t, "https://example.com:"+port+"/", spec); !r.OK() || host.Load() != "example.com:"+port {
		t.Errorf("failure %q, Host %v", r.Failure, host.Load())
	}
	// The certificate is verified against the URL's host, not the address.
	if r := checkOnce(t, "https://other.example:"+port+"/", spec); r.Failure != FailureTLS {
		t.Errorf("wrong host: failure %q, want %q", r.Failure, FailureTLS)
	}
}

// D-082: TLS settings are enforced: the CA bundle, the server name and
// the minimum version.
func TestHTTPCheckTLS(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	old := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	old.TLS = &tls.Config{MaxVersion: tls.VersionTLS12}
	old.StartTLS()
	defer old.Close()
	trust := func(s *httptest.Server, f func(*HTTP)) HTTP {
		h := get()
		h.TLS = config.TLSConfig{CAFile: caFile(t, s)}
		if f != nil {
			f(&h)
		}
		return h
	}
	plain := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer plain.Close()
	tests := []struct {
		name, url string
		spec      HTTP
		want      Failure
	}{
		{"trusted CA", srv.URL, trust(srv, nil), ""},
		{"system roots only", srv.URL, get(), FailureTLS},
		{"insecure", srv.URL, func() HTTP { h := get(); h.TLS.InsecureSkipVerify = true; return h }(), ""},
		{"server name matches", srv.URL, trust(srv, func(h *HTTP) { h.TLS.ServerName = "example.com" }), ""},
		{"server name differs", srv.URL, trust(srv, func(h *HTTP) { h.TLS.ServerName = "other.example" }), FailureTLS},
		{"TLS 1.2 server, 1.2 minimum", old.URL, trust(old, nil), ""},
		{"TLS 1.2 server, 1.3 minimum", old.URL, trust(old, func(h *HTTP) { h.TLSMinVersion = tls.VersionTLS13 }), FailureTLS},
		{"plain HTTP server", strings.Replace(plain.URL, "http:", "https:", 1), get(), FailureTLS},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if r := checkOnce(t, tt.url, tt.spec); r.Failure != tt.want {
				t.Errorf("failure %q, want %q", r.Failure, tt.want)
			}
		})
	}
}

// A closed port is connect_failed; oversized response headers are
// request_failed (bounded, never read whole).
func TestHTTPCheckTransportFailures(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := l.Addr().String()
	_ = l.Close() // nothing listens there now
	if r := checkOnce(t, "http://"+closed+"/", get()); r.Failure != FailureConnect {
		t.Errorf("closed port: failure %q", r.Failure)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Big", strings.Repeat("h", maxResponseHeaderBytes+1))
	}))
	defer srv.Close()
	if r := checkOnce(t, srv.URL, get()); r.Failure != FailureRequest {
		t.Errorf("big headers: failure %q", r.Failure)
	}
}

// No keep-alive: every check opens its own connection.
func TestHTTPCheckDoesNotReuseConnections(t *testing.T) {
	var conns atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	c, err := newHTTPChecker(Service{Name: "web", Type: "http", URL: u, HTTP: get()})
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if r := c.Check(context.Background()); !r.OK() {
			t.Fatal(r.Failure)
		}
	}
	if n := conns.Load(); n != 3 {
		t.Errorf("%d connections for 3 checks", n)
	}
}

// The checker honours cancellation while it waits for the headers and
// while it reads the body: the loop's timeout ends it (D-081 (1)).
func TestHTTPCheckHonoursCancellation(t *testing.T) {
	release := make(chan struct{})
	reached := make(chan string, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/body" {
			_, _ = io.WriteString(w, "partial")
			w.(http.Flusher).Flush()
		}
		reached <- r.URL.Path
		<-release
	}))
	defer srv.Close()
	defer close(release)
	spec := get()
	spec.BodyContains = "never"
	for _, path := range []string{"/headers", "/body"} {
		t.Run(path, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan Result, 1)
			go func() { done <- checkCtx(t, ctx, srv.URL+path, spec) }()
			<-reached
			cancel()
			select {
			case r := <-done:
				if r.OK() {
					t.Error("a cancelled check succeeded")
				}
			case <-time.After(10 * time.Second): // a safety net, not a timing assumption
				t.Fatal("the check ignored its cancellation")
			}
		})
	}
}

// Failures map to fixed codes, whatever the error says.
func TestClassify(t *testing.T) {
	wrap := func(err error) error { return &url.Error{Op: "Get", URL: "https://u:p@h/?k=v", Err: err} }
	tests := []struct {
		err  error
		want Failure
	}{
		{wrap(errRedirectRefused), FailureRedirectRefused},
		{wrap(errTooManyRedirects), FailureRedirects},
		{wrap(context.DeadlineExceeded), FailureTimeout},
		{wrap(&net.OpError{Op: "dial", Err: &net.DNSError{Err: "no such host", Name: "h"}}), FailureDNS},
		{wrap(&net.OpError{Op: "dial", Err: errors.New("connection refused")}), FailureConnect},
		{wrap(&net.OpError{Op: "remote error", Err: errors.New("tls: handshake failure")}), FailureTLS},
		{wrap(&tls.CertificateVerificationError{Err: errors.New("x")}), FailureTLS},
		{wrap(errors.New("tls: server selected unsupported protocol version 303")), FailureTLS},
		{wrap(io.ErrUnexpectedEOF), FailureRequest},
	}
	for _, tt := range tests {
		if got := classify(tt.err); got != tt.want {
			t.Errorf("classify(%v) = %q, want %q", tt.err, got, tt.want)
		}
		if !tt.want.known() {
			t.Errorf("%q is not a known code", tt.want)
		}
	}
}

// Review R1 (3a-4): net/http detaches dialing from the request, so a
// cancelled check must still close a connection stuck in its TLS
// handshake; otherwise each check against such an endpoint leaves a
// socket and goroutines behind, past its slot and past Stop (D-081 (1)).
func TestHTTPCheckCancellationClosesPendingTLS(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_ = ln.(*net.TCPListener).SetDeadline(time.Now().Add(10 * time.Second)) // safety net
	u, _ := url.Parse("https://" + ln.Addr().String())
	c, err := newHTTPChecker(Service{Name: "web", Type: "http", URL: u, Timeout: time.Minute, HTTP: get()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan Result, 1)
	go func() { done <- c.Check(ctx) }()
	conn, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var hello [1]byte
	if _, err := io.ReadFull(conn, hello[:]); err != nil { // the ClientHello started: the handshake waits for us
		t.Fatal(err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second): // a safety net, not a timing assumption
		t.Fatal("Check did not return")
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second)) // safety net
	if _, err := io.Copy(io.Discard, conn); err != nil {
		t.Fatalf("the connection outlived the cancelled check: %v", err)
	}
}
