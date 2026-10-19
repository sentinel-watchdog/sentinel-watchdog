package supervisor

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/tlsconfig"
)

const (
	userAgent = "sentinel-watchdog"
	// maxResponseHeaderBytes bounds the response headers a check reads.
	maxResponseHeaderBytes = 64 << 10
)

var (
	errRedirectRefused  = errors.New("redirect to another origin")
	errTooManyRedirects = errors.New("too many redirects")
)

// httpChecker checks an http service (D-080 (4), D-082). It has no
// timeout of its own: the loop cancels ctx after the service's timeout,
// on the injected clock.
type httpChecker struct {
	client *http.Client
	url    string
	spec   HTTP
}

// newHTTPChecker builds the checker of s, reading its CA bundle now so
// that `sentinelctl validate` reports it. Its errors name keys, never
// values: they become configuration problems.
func newHTTPChecker(s Service) (Checker, error) {
	// The dial and handshake timeouts are a backstop on the real clock:
	// the loop's timeout cancels the check, and Check then cancels the
	// connection attempts net/http keeps running after a cancelled request.
	transport := &http.Transport{
		Proxy:                  nil, // the proxy environment is ignored (D-080 (4))
		DisableKeepAlives:      true,
		MaxResponseHeaderBytes: maxResponseHeaderBytes,
		TLSHandshakeTimeout:    s.Timeout,
	}
	dialer := &net.Dialer{Timeout: s.Timeout}
	transport.DialContext = dialer.DialContext
	if s.HTTP.Address != "" {
		// Every request goes to the URL's origin (redirects only within
		// it), so every connection goes to the configured address.
		transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, s.HTTP.Address)
		}
	}
	if s.URL.Scheme == "https" {
		cfg, err := tlsconfig.Client(s.HTTP.TLS)
		if err != nil {
			return nil, caError(err)
		}
		cfg.MinVersion = s.HTTP.TLSMinVersion
		transport.TLSClientConfig = cfg
	}
	origin := s.URL
	follow := s.HTTP.FollowRedirects
	client := &http.Client{
		Transport: transport,
		// Redirects are off by default; when on, only within the origin, so
		// neither the headers nor a replayed body reach another host.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			switch {
			case !follow:
				return http.ErrUseLastResponse
			case len(via) > maxRedirects:
				return errTooManyRedirects
			case !sameOrigin(req.URL, origin):
				return errRedirectRefused
			}
			return nil
		},
	}
	return &httpChecker{client: client, url: s.URL.String(), spec: s.HTTP}, nil
}

// caError turns a CA bundle failure into a message without the paths the
// underlying error names.
func caError(err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return errors.New("tls.ca_file: the file or a directory on its path does not exist")
	case errors.Is(err, tlsconfig.ErrUnsafe):
		return errors.New("tls.ca_file is on an unsafe path: the file, its directory and their parents must be " +
			"owned by root or by the user running sentineld and not writable by group or others, " +
			"and a link may only name a file of its own directory")
	case errors.Is(err, tlsconfig.ErrInvalid):
		return fmt.Errorf("tls.ca_file must be a regular file of at most %d bytes holding PEM certificates", tlsconfig.MaxCABytes)
	case errors.Is(err, fs.ErrPermission):
		return errors.New("tls.ca_file cannot be read: permission denied")
	}
	return errors.New("tls.ca_file cannot be read")
}

// sameOrigin reports whether u has a's scheme, host and port.
func sameOrigin(u, a *url.URL) bool {
	return strings.EqualFold(u.Scheme, a.Scheme) && strings.EqualFold(u.Hostname(), a.Hostname()) &&
		portOf(u) == portOf(a)
}

// Check implements Checker: one request, judged by its status code and,
// with body_contains, by the first maxBodyRead bytes of its body.
func (c *httpChecker) Check(ctx context.Context) Result {
	var body io.Reader
	if c.spec.Method == http.MethodPost || c.spec.Method == http.MethodPut {
		body = strings.NewReader(c.spec.Body) // replayable on a same-origin 307/308
	}
	req, err := http.NewRequestWithContext(ctx, c.spec.Method, c.url, body)
	if err != nil {
		return Result{Failure: FailureRequest}
	}
	req.Header.Set("User-Agent", userAgent)
	for name, value := range c.spec.Headers {
		req.Header.Set(name, value)
	}
	// net/http finishes a dial or a TLS handshake after its request is
	// cancelled, to pool the connection; closing idle connections also
	// cancels those attempts, so none outlives the check (review R1). A
	// service runs one check at a time, so no other request is affected.
	defer c.client.CloseIdleConnections()
	resp, err := c.client.Do(req)
	if err != nil {
		return Result{Failure: classify(err)}
	}
	defer resp.Body.Close() // no keep-alive: nothing to drain
	if !c.accepted(resp.StatusCode) {
		return Result{Failure: FailureStatus}
	}
	if c.spec.BodyContains == "" {
		return Result{}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyRead))
	switch {
	case err != nil:
		return Result{Failure: FailureBodyRead}
	case !bytes.Contains(data, []byte(c.spec.BodyContains)):
		return Result{Failure: FailureBody}
	}
	return Result{}
}

func (c *httpChecker) accepted(status int) bool {
	if c.spec.SuccessCodes == nil {
		return status >= 200 && status < 300
	}
	_, found := slices.BinarySearch(c.spec.SuccessCodes, status)
	return found
}

// classify maps a failed request to a fixed code; the error's text, which
// quotes the URL, goes nowhere.
func classify(err error) Failure {
	var (
		dnsErr    *net.DNSError
		opErr     *net.OpError
		verifyErr *tls.CertificateVerificationError
		recordErr tls.RecordHeaderError
	)
	switch {
	case errors.Is(err, errRedirectRefused):
		return FailureRedirectRefused
	case errors.Is(err, errTooManyRedirects):
		return FailureRedirects
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return FailureTimeout
	case errors.As(err, &dnsErr):
		return FailureDNS
	case errors.As(err, &verifyErr), errors.As(err, &recordErr):
		return FailureTLS
	case errors.As(err, &opErr) && opErr.Op == "remote error": // a TLS alert from the server
		return FailureTLS
	case errors.As(err, &opErr) && opErr.Op == "dial":
		return FailureConnect
	case strings.Contains(err.Error(), "tls: "),
		strings.Contains(err.Error(), "server gave HTTP response to HTTPS client"):
		// crypto/tls reports a refused handshake (such as a version below
		// tls.min_version) with untyped errors, all prefixed "tls: ", and
		// net/http turns a plain HTTP answer to a handshake into text.
		return FailureTLS
	}
	return FailureRequest
}
