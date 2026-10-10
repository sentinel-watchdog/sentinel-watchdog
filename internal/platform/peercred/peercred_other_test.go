//go:build !linux

package peercred

import (
	"errors"
	"testing"
)

func TestReadIsUnsupported(t *testing.T) {
	if _, err := Read(nil); !errors.Is(err, ErrUnsupported) {
		t.Errorf("err = %v", err)
	}
}
