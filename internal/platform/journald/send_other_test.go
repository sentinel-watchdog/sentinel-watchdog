//go:build !linux

package journald

import (
	"errors"
	"testing"
)

func TestSendUnsupported(t *testing.T) {
	if err := Send(Socket, Encode([]Field{{"MESSAGE", "x"}})); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("error %v, want ErrUnsupported", err)
	}
}
