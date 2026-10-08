// Package notify delivers events to the notification channels of the
// configuration: a Dispatcher routes each event to its channels and keeps
// one bounded queue and one worker per channel; a Webhook sends one
// payload over HTTP (ADR-0002, D-072).
package notify

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/config"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/fstrust"
)

const (
	// maxCABytes bounds the CA bundle a channel may name.
	maxCABytes = 1 << 20
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
	tlsConfig := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		ServerName:         ch.TLS.ServerName,
		InsecureSkipVerify: ch.TLS.InsecureSkipVerify, //nolint:gosec // explicit opt-in; config validation warns
	}
	if ch.TLS.CAFile != "" {
		pool, err := loadCA(ch.TLS.CAFile)
		if err != nil {
			return nil, fmt.Errorf("notify: channel %q: %w", ch.Name, err)
		}
		tlsConfig.RootCAs = pool
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsConfig
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

// loadCA reads a PEM bundle. Whoever can change it can intercept the
// channel's traffic and its token, so it is checked like configuration:
// every directory on its path (links included) by fstrust.Resolve, its
// own directory strictly, and the opened file. It is opened without
// following a last link (Resolve has already resolved them) and without
// blocking (a FIFO must not hang the daemon).
func loadCA(path string) (*x509.CertPool, error) {
	euid := os.Geteuid()
	resolved, err := fstrust.Resolve(path, euid)
	if err != nil {
		return nil, fmt.Errorf("CA bundle %s: %w", path, err)
	}
	dirInfo, err := os.Lstat(filepath.Dir(resolved))
	if err != nil {
		return nil, fmt.Errorf("CA bundle %s: %w", path, err)
	}
	if err := fstrust.CheckOwnership(dirInfo, euid); err != nil {
		return nil, fmt.Errorf("CA bundle directory %s %w", filepath.Dir(resolved), err)
	}
	f, err := os.OpenFile(resolved, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("read CA bundle: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("read CA bundle: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("CA bundle %s is not a regular file", path)
	}
	if err := fstrust.CheckOwnership(info, euid); err != nil {
		return nil, fmt.Errorf("CA bundle %s %w", path, err)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxCABytes+1))
	if err != nil {
		return nil, fmt.Errorf("read CA bundle: %w", err)
	}
	if len(data) > maxCABytes {
		return nil, fmt.Errorf("CA bundle %s exceeds %d bytes", path, maxCABytes)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		return nil, fmt.Errorf("CA bundle %s holds no PEM certificate", path)
	}
	return pool, nil
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
