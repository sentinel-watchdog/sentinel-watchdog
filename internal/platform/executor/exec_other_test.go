//go:build !linux

package executor

import (
	"context"
	"errors"
	"testing"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/platform/privilege"
)

func runParentHelper() {}

// Outside Linux an identity change is refused, never silently ignored.
func TestCredentialsUnsupported(t *testing.T) {
	spec := helper(t, "exit:0", nil)
	spec.Cred = &privilege.Cred{UID: 1, GID: 1}
	if _, err := newExecutor(t).Start(context.Background(), spec); !errors.Is(err, ErrUnsupported) {
		t.Errorf("err = %v", err)
	}
}
