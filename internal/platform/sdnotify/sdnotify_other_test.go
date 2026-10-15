//go:build !linux

package sdnotify

import (
	"context"
	"errors"
	"testing"
)

// Outside Linux a configured NOTIFY_SOCKET is reported, never sent to.
func TestNotifyIsUnsupported(t *testing.T) {
	n, err := Parse("/run/notify-test-never-used")
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Notify(context.Background(), Ready); !errors.Is(err, ErrUnsupported) {
		t.Errorf("error %v, want ErrUnsupported", err)
	}
}
