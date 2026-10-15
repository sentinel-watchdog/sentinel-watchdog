package sdnotify

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	long := "/" + strings.Repeat("a", maxAddr)
	tests := []struct {
		name  string
		value string
		ok    bool // a Notifier is returned
		err   bool
	}{
		{"unset", "", false, false},
		{"absolute path", "/run/systemd/notify", true, false},
		{"abstract name", "@/org/freedesktop/systemd1/notify/1", true, false},
		{"longest path", long[:maxAddr], true, false},
		{"too long", long, false, true},
		{"relative path", "run/notify", false, true},
		{"bare at", "@", false, true},
		{"vsock", "vsock:2:1234", false, true},
		{"newline", "/run/notify\nREADY=1", false, true},
		{"NUL", "/run/no\x00tify", false, true},
		{"DEL", "/run/no\x7ftify", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n, err := Parse(tt.value)
			if (n != nil) != tt.ok || (err != nil) != tt.err {
				t.Fatalf("Parse(%q) = %v, %v; want notifier %v, error %v", tt.value, n, err, tt.ok, tt.err)
			}
			if err != nil {
				if !errors.Is(err, ErrInvalidSocket) {
					t.Errorf("error %v is not ErrInvalidSocket", err)
				}
				if strings.Contains(err.Error(), tt.value) {
					t.Errorf("error %q echoes the value", err)
				}
			}
		})
	}
}

// FuzzParse: never panics, accepts only what a socket address can hold, and
// its error never carries the environment value into the logs.
func FuzzParse(f *testing.F) {
	for _, s := range []string{"", "/run/notify", "@abs", "@", "rel", "vsock:1:2", "/a\nb", "/a\x00b"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, value string) {
		n, err := Parse(value)
		if err != nil {
			if n != nil {
				t.Fatal("notifier with an error")
			}
			if len(value) > 4 && strings.Contains(err.Error(), value) {
				t.Fatalf("error %q echoes the value", err)
			}
			return
		}
		if n == nil {
			if value != "" {
				t.Fatalf("no notifier and no error for %q", value)
			}
			return
		}
		if len(value) > maxAddr || (value[0] != '/' && value[0] != '@') || strings.ContainsFunc(value, isControl) {
			t.Fatalf("accepted %q", value)
		}
	})
}

func TestStatus(t *testing.T) {
	tests := []struct {
		name, text, want string
	}{
		{"plain", "ready: 2 modules", "STATUS=ready: 2 modules"},
		{"newline cannot add an assignment", "ok\nREADY=1", "STATUS=ok READY=1"},
		{"control characters", "a\x00b\tc", "STATUS=a b c"},
		{"cut", strings.Repeat("x", maxStatus+10), "STATUS=" + strings.Repeat("x", maxStatus)},
		{"cut keeps valid UTF-8", strings.Repeat("x", maxStatus-1) + "é", "STATUS=" + strings.Repeat("x", maxStatus-1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Status(tt.text); got != tt.want {
				t.Errorf("Status(%q) = %q, want %q", tt.text, got, tt.want)
			}
		})
	}
}

// Outside systemd (no NOTIFY_SOCKET) Notify does nothing.
func TestNilNotifierSendsNothing(t *testing.T) {
	var n *Notifier
	if err := n.Notify(context.Background(), Ready); err != nil {
		t.Fatal(err)
	}
}

// listen opens a datagram socket at a short path (macOS limits socket paths
// to 104 bytes, and t.TempDir is long there).
func listen(t *testing.T) (*net.UnixConn, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "sdn")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "n.sock")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn, path
}

// receive reads one datagram, failing the test after a while.
func receive(t *testing.T, conn *net.UnixConn) string {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	return string(buf[:n])
}

func TestNotifySendsOneDatagram(t *testing.T) {
	conn, path := listen(t)
	n, err := Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Notify(context.Background(), Ready, Status("ready")); err != nil {
		t.Fatal(err)
	}
	if got, want := receive(t, conn), "READY=1\nSTATUS=ready"; got != want {
		t.Errorf("datagram %q, want %q", got, want)
	}
}

func TestNotifyWithoutListenerFails(t *testing.T) {
	_, path := listen(t)
	n, err := Parse(filepath.Join(filepath.Dir(path), "missing.sock"))
	if err != nil {
		t.Fatal(err)
	}
	err = n.Notify(context.Background(), Ready)
	if err == nil {
		t.Fatal("no error without a listener")
	}
	if strings.Contains(err.Error(), filepath.Dir(path)) {
		t.Errorf("error %q echoes the socket address", err)
	}
}

// A systemd that stops reading never holds the caller: once the receiver's
// queue is full, Notify returns an error within its bound.
func TestNotifyIsBoundedWhenTheReceiverStopsReading(t *testing.T) {
	_, path := listen(t) // never read
	n, err := Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	payload := Status(strings.Repeat("x", maxStatus))
	for i := range 100_000 {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		start := time.Now()
		err := n.Notify(ctx, payload)
		cancel()
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("send %d took %v", i, elapsed)
		}
		if err != nil {
			return // the full queue was reported, in time
		}
	}
	t.Fatal("the receiver's queue never filled")
}
