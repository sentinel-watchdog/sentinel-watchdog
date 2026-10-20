package executor

import (
	"context"
	"errors"
	"io"
	"syscall"
	"testing"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/clock"
)

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// Review R1 (3b-1): discarded output is /dev/null, not a closed
// descriptor: a program that checks its writes still succeeds.
func TestDiscardAllowsWrites(t *testing.T) {
	p, err := newExecutor(t).Start(context.Background(), Spec{
		Path: "/bin/sh",
		Args: []string{"-c", "printf x && printf y >&2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if ex := waitDone(t, p); ex.Code != 0 {
		t.Fatalf("discarded writes failed: %v", ex)
	}
}

// Review R2 (3b-1): a failing sink (a full disk) does not block the
// program on a full pipe: its output is discarded and the error kept.
func TestSinkFailureDoesNotBlockChild(t *testing.T) {
	p, err := newExecutor(t).Start(context.Background(), Spec{
		Path: "/bin/sh",
		Args: []string{"-c", `i=0; while [ "$i" -lt 32768 ]; do printf '%064d' 0 || exit 1; i=$((i+1)); done`},
		Stdout: writerFunc(func([]byte) (int, error) {
			return 0, io.ErrClosedPipe
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = p.Stop(context.Background(), syscall.SIGKILL, 0) }()
	if ex := waitDone(t, p); ex.Code != 0 {
		t.Errorf("exit %v", ex)
	}
	if p.OutputErr() == nil {
		t.Error("the sink error was lost")
	}
}

// Review R3 (3b-1): a writer blocked in Write does not keep Done from
// coming once the drain timeout has passed.
func TestDrainBoundsBlockedWriter(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	o, err := newOutput(writerFunc(func(p []byte) (int, error) {
		close(entered)
		<-release
		return len(p), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o.child.Write([]byte("x")); err != nil {
		o.abort()
		t.Fatal(err)
	}
	o.started()
	<-entered
	done := make(chan struct{})
	go func() {
		o.drain(clock.Real(), 10*time.Millisecond)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second): // a safety net
		t.Fatal("drain exceeded its timeout")
	}
	// The abandoned copy is reported, not hidden (re-review R3).
	if !errors.Is(o.err(), ErrSinkBlocked) {
		t.Errorf("OutputErr = %v, want ErrSinkBlocked", o.err())
	}
}
