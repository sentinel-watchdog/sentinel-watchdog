// Package tlsconfig builds the TLS configuration of the connections
// sentineld opens (webhooks, HTTP checks). It holds the knowledge they
// share: the minimum protocol version and how a CA bundle named in the
// configuration is read safely.
package tlsconfig

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/config"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/fstrust"
)

// MaxCABytes bounds the CA bundle a configuration may name.
const MaxCABytes = 1 << 20

// The kinds of LoadCA failures, matched with errors.Is. A caller that must
// not quote paths (a module's diagnostics name keys, never values) turns
// the kind into its own message; a missing file is fs.ErrNotExist.
var (
	// ErrUnsafe: the bundle, its directory or a parent may be changed by
	// someone other than root or the daemon's user, or a link leaves the
	// bundle's directory.
	ErrUnsafe = errors.New("unsafe CA bundle path")
	// ErrInvalid: not a regular file, larger than MaxCABytes, or no PEM
	// certificate.
	ErrInvalid = errors.New("invalid CA bundle")
)

// kindError tags err with one of the kinds above without changing its
// text, so the webhook's messages stay as they are.
type kindError struct{ kind, err error }

func (e kindError) Error() string   { return e.err.Error() }
func (e kindError) Unwrap() []error { return []error{e.kind, e.err} }

// tagUnsafe tags err as ErrUnsafe unless the path is missing or unreadable,
// which are reported as such.
func tagUnsafe(err error) error {
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
		return err
	}
	return kindError{ErrUnsafe, err}
}

// Client returns the client configuration for c: TLS 1.2 or later, the
// server name and the CA bundle of c, verification disabled only when c
// asks for it. It fails if the CA bundle cannot be read (LoadCA).
func Client(c config.TLSConfig) (*tls.Config, error) {
	cfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		ServerName:         c.ServerName,
		InsecureSkipVerify: c.InsecureSkipVerify, //nolint:gosec // explicit opt-in; config validation warns
	}
	if c.CAFile != "" {
		pool, err := LoadCA(c.CAFile)
		if err != nil {
			return nil, err
		}
		cfg.RootCAs = pool
	}
	return cfg, nil
}

// LoadCA reads a PEM bundle. Whoever can change it can intercept the
// connections that trust it, and the tokens they carry, so it is read like configuration: the
// directories on the path to its directory are checked, that directory is
// opened as an os.Root and checked strictly, the bundle may be a link only
// to another file of that directory, and the opened file is checked. It is
// opened without blocking (a FIFO must not hang the daemon). Failures
// match ErrUnsafe, ErrInvalid or fs.ErrNotExist.
func LoadCA(path string) (*x509.CertPool, error) {
	euid := os.Geteuid()
	dir, err := fstrust.Resolve(filepath.Dir(path), euid)
	if err != nil {
		return nil, tagUnsafe(fmt.Errorf("CA bundle %s: %w", path, err))
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("CA bundle %s: %w", path, err)
	}
	defer root.Close()
	dirInfo, err := root.Stat(".")
	if err != nil {
		return nil, fmt.Errorf("CA bundle %s: %w", path, err)
	}
	if err := fstrust.CheckOwnership(dirInfo, euid); err != nil {
		return nil, tagUnsafe(fmt.Errorf("CA bundle directory %s %w", filepath.Dir(path), err))
	}
	name := filepath.Base(path)
	if err := fstrust.CheckLink(root, name); err != nil {
		return nil, tagUnsafe(fmt.Errorf("CA bundle %s %w", path, err))
	}
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("read CA bundle: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("read CA bundle: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, kindError{ErrInvalid, fmt.Errorf("CA bundle %s is not a regular file", path)}
	}
	if err := fstrust.CheckOwnership(info, euid); err != nil {
		return nil, tagUnsafe(fmt.Errorf("CA bundle %s %w", path, err))
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxCABytes+1))
	if err != nil {
		return nil, fmt.Errorf("read CA bundle: %w", err)
	}
	if len(data) > MaxCABytes {
		return nil, kindError{ErrInvalid, fmt.Errorf("CA bundle %s exceeds %d bytes", path, MaxCABytes)}
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		return nil, kindError{ErrInvalid, fmt.Errorf("CA bundle %s holds no PEM certificate", path)}
	}
	return pool, nil
}
