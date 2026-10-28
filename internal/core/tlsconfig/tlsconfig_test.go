package tlsconfig

import (
	"context"
	"crypto/tls"
	"encoding/pem"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/config"
)

func TestClient(t *testing.T) {
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	defer srv.Close()
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		in   config.TLSConfig
		ca   bool
	}{
		{"defaults", config.TLSConfig{}, false},
		{"CA bundle and server name", config.TLSConfig{CAFile: caFile, ServerName: "example.org"}, true},
		{"verification disabled", config.TLSConfig{InsecureSkipVerify: true}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Client(tt.in)
			if err != nil {
				t.Fatal(err)
			}
			if got.MinVersion != tls.VersionTLS12 {
				t.Errorf("MinVersion = %#x, want TLS 1.2", got.MinVersion)
			}
			if got.ServerName != tt.in.ServerName || got.InsecureSkipVerify != tt.in.InsecureSkipVerify {
				t.Errorf("ServerName/InsecureSkipVerify = %q/%v, want %q/%v",
					got.ServerName, got.InsecureSkipVerify, tt.in.ServerName, tt.in.InsecureSkipVerify)
			}
			if (got.RootCAs != nil) != tt.ca {
				t.Errorf("RootCAs set = %v, want %v", got.RootCAs != nil, tt.ca)
			}
		})
	}
	if _, err := Client(config.TLSConfig{CAFile: filepath.Join(t.TempDir(), "missing.pem")}); err == nil {
		t.Error("Client accepted a missing CA bundle")
	}
}

func TestLoadCARejectsBadBundles(t *testing.T) {
	dir := t.TempDir()
	notPEM := filepath.Join(dir, "x.pem")
	if err := os.WriteFile(notPEM, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	big := filepath.Join(dir, "big.pem")
	if err := os.WriteFile(big, make([]byte, MaxCABytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, path string
		kind       error
	}{
		{"missing", filepath.Join(dir, "missing.pem"), fs.ErrNotExist},
		{"missing directory", filepath.Join(dir, "none", "ca.pem"), fs.ErrNotExist},
		{"not PEM", notPEM, ErrInvalid},
		{"too large", big, ErrInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadCA(tt.path)
			if !errors.Is(err, tt.kind) {
				t.Errorf("err = %v, want %v", err, tt.kind)
			}
		})
	}
}

// R2: a CA "file" that is a FIFO must not hang LoadCA. The check runs
// in a child process so that a hang cannot leave a goroutine behind.
func TestCABundleMustBeARegularFile(t *testing.T) {
	const marker = "SENTINEL_TEST_CA_FIFO"
	if path := os.Getenv(marker); path != "" {
		if _, err := LoadCA(path); !errors.Is(err, ErrInvalid) {
			t.Fatalf("FIFO: err = %v, want ErrInvalid", err)
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
	other := filepath.Join(base, "other")
	if err := os.Mkdir(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "ca.pem"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(safe, "outside.pem")
	if err := os.Symlink("../other/ca.pem", outside); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"writable directory": ca, "link through it": link,
		"link to another safe directory": outside, // same rule as configuration
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadCA(path); !errors.Is(err, ErrUnsafe) {
				t.Fatalf("CA bundle on an unsafe path: err = %v, want ErrUnsafe", err)
			}
		})
	}
	// A link to another file of the same safe directory is fine. A link
	// to another directory is not (above), even a system bundle's such as
	// RHEL's /etc/pki/tls/certs/ca-bundle.crt: name the bundle itself, or
	// omit ca_file for the system roots (D-092).
	good := filepath.Join(safe, "real.pem")
	if err := os.WriteFile(good, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.pem", filepath.Join(safe, "alias.pem")); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCA(filepath.Join(safe, "alias.pem")); err != nil {
		t.Fatalf("link to a safe bundle: %v", err)
	}
}
