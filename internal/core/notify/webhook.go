// Package notify delivers events to the notification channels of the
// configuration: a Dispatcher routes each event to its channels and keeps
// one bounded queue and one worker per channel; a Webhook sends one
// payload over HTTP (ADR-0002, D-072).
package notify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/config"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/tlsconfig"
)

const (
	// maxDrainBytes bounds how much of a response body is read (and
	// discarded) so the connection can be reused.
	maxDrainBytes = 64 << 10
	userAgent     = "sentinel-watchdog"
)

// Webhook sends payloads to one webhook channel.
type Webhook struct {
	client  *http.Client
	url     string
	method  string
	headers map[string]string
	success []int
	timeout time.Duration
}

// NewWebhook prepares the HTTP client of ch. Redirects are never followed:
// a redirect could carry the channel's headers, often a token, to another
// host. It fails if the CA bundle cannot be read.
func NewWebhook(ch config.Channel) (*Webhook, error) {
	tlsConfig, err := tlsconfig.Client(ch.TLS)
	if err != nil {
		return nil, fmt.Errorf("notify: channel %q: %w", ch.Name, err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsConfig
	// The proxy environment is ignored, as for HTTP checks (D-080 (4),
	// D-092): a variable in sentineld's environment must not send a
	// channel's headers, often a token, through a host nobody configured.
	transport.Proxy = nil
	return &Webhook{
		client: &http.Client{
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		url:     ch.URL,
		method:  ch.Method,
		headers: ch.Headers,
		success: ch.SuccessStatusCodes,
		timeout: ch.Timeout.Std(),
	}, nil
}

// Send makes one delivery attempt of body, bounded by the channel's
// timeout. The error never contains the URL, which often holds a token.
func (w *Webhook) Send(ctx context.Context, body []byte) error {
	ctx, cancel := context.WithTimeout(ctx, w.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, w.method, w.url, bytes.NewReader(body))
	if err != nil {
		return errors.New("invalid webhook request") // the error would quote the URL
	}
	req.Header.Set("User-Agent", userAgent)
	for k, v := range w.headers {
		req.Header.Set(k, v)
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return withoutURL(err)
	}
	defer resp.Body.Close()
	// Reading a bounded part lets the connection be reused; the content is
	// not used.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrainBytes))
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return fmt.Errorf("webhook answered %d: redirects are not followed", resp.StatusCode)
	}
	if !w.accepted(resp.StatusCode) {
		return fmt.Errorf("webhook answered %d", resp.StatusCode)
	}
	return nil
}

func (w *Webhook) accepted(status int) bool {
	if len(w.success) == 0 {
		return status >= 200 && status < 300
	}
	return slices.Contains(w.success, status)
}

// withoutURL removes the URL that net/http puts in its errors.
func withoutURL(err error) error {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err
	}
	if ue.Timeout() {
		return fmt.Errorf("webhook request timed out: %w", ue.Err)
	}
	return fmt.Errorf("webhook request failed: %w", ue.Err)
}
