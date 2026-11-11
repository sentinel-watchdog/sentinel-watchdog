package journald

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// listen opens a datagram socket at a filesystem path, as journald's. Never an
// abstract name: a datagram socket at one hangs a kernel of the matrix in a
// container (R-024).
func listen(t *testing.T) (*net.UnixConn, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "j.sock")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn, path
}

func TestSendSendsOneDatagram(t *testing.T) {
	conn, path := listen(t)
	entry := Encode([]Field{{"MESSAGE", "a\nb"}})
	if err := Send(path, entry); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[:n]) != string(entry) {
		t.Errorf("received %q, want %q", buf[:n], entry)
	}
}

// A missing socket is an error to fall back on, not a full one.
func TestSendWithoutListener(t *testing.T) {
	err := Send(filepath.Join(t.TempDir(), "missing.sock"), Encode([]Field{{"MESSAGE", "x"}}))
	if !errors.Is(err, syscall.ENOENT) || errors.Is(err, ErrFull) {
		t.Fatalf("error %v, want ENOENT and not ErrFull", err)
	}
}

// fill sends until the receiver's queue is full and returns the error of the
// send that found it full. The cap bounds the test whatever the kernel's limit
// (net.unix.max_dgram_qlen, 512 by default).
func fill(t *testing.T, path string) error {
	t.Helper()
	entry := Encode([]Field{{"MESSAGE", "x"}})
	for range 5000 {
		if err := Send(path, entry); err != nil {
			return err
		}
	}
	t.Fatal("the receiver's queue never filled")
	return nil
}

// A journald that stops reading refuses at once with ErrFull, still matching
// the errno: the caller never waits for it.
func TestSendFullQueue(t *testing.T) {
	_, path := listen(t) // never read
	start := time.Now()
	err := fill(t, path)
	if !errors.Is(err, ErrFull) || !errors.Is(err, syscall.EAGAIN) {
		t.Fatalf("error %v, want ErrFull wrapping EAGAIN", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("filling took %v", elapsed)
	}
}

// Every send closes its socket, failed ones included.
func TestSendLeaksNoDescriptor(t *testing.T) {
	_, full := listen(t)
	_ = fill(t, full)
	_, ok := listen(t)
	missing := filepath.Join(t.TempDir(), "missing.sock")
	entry := Encode([]Field{{"MESSAGE", "x"}})
	before := openFDs(t)
	for range 100 {
		_ = Send(full, entry)
		_ = Send(ok, entry)
		_ = Send(missing, entry)
	}
	if after := openFDs(t); after != before {
		t.Errorf("open descriptors %d, before %d", after, before)
	}
}

func openFDs(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}
