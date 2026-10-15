package sdnotify

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
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
		{"abstract name", "@/org/freedesktop/systemd1/notify/1", abstractNames, !abstractNames},
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
// its error never carries the environment value into the logs: every error
// is one of a few fixed texts. (Checking that the error does not contain
// the value failed on values that are part of a fixed text, like " sock".)
func FuzzParse(f *testing.F) {
	for _, s := range []string{"", "/run/notify", "@abs", "@", "rel", "vsock:1:2", "/a\nb", "/a\x00b"} {
		f.Add(s)
	}
	fixed := map[string]bool{}
	for _, reason := range []string{
		fmt.Sprintf("longer than %d bytes", maxAddr), "abstract names exist on Linux only",
		"neither an absolute path nor an abstract name", "control characters",
	} {
		fixed[ErrInvalidSocket.Error()+": "+reason] = true
	}
	f.Fuzz(func(t *testing.T, value string) {
		n, err := Parse(value)
		if err != nil {
			if n != nil {
				t.Fatal("notifier with an error")
			}
			if !fixed[err.Error()] {
				t.Fatalf("error %q is not a fixed text: it may echo the value", err)
			}
			return
		}
		if n == nil {
			if value != "" {
				t.Fatalf("no notifier and no error for %q", value)
			}
			return
		}
		if len(value) > maxAddr || (value[0] != '/' && value[0] != '@') || strings.ContainsFunc(value, isControl) ||
			(value[0] == '@' && !abstractNames) {
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

// A ctx already done sends nothing and says why.
func TestNotifyWithDoneContext(t *testing.T) {
	n, err := Parse("/run/notify-test-never-used")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := n.Notify(ctx, Ready); !errors.Is(err, context.Canceled) {
		t.Errorf("error %v, want context.Canceled", err)
	}
}
