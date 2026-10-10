package daemon

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"time"
)

// errAbandoned reports a module call that did not return in time.
var errAbandoned = errors.New("did not return in time")

// panicError reports a recovered panic. It keeps where the panic happened,
// never the panic value: the value may hold a secret (D-071), and so may
// the arguments a full stack trace prints.
type panicError struct {
	frames []string
}

func (e *panicError) Error() string { return "panicked" }

// maxFrames bounds the frames kept from a panicking goroutine.
const maxFrames = 16

// call runs op of the named module in its own goroutine, recovers a panic
// and waits at most timeout. When ctx ends (a signal during start), the
// module's ctx is cancelled and the call still waits, so the module can
// return cleanly. A call that does not return in time is abandoned: Go cannot stop a goroutine, so it may still
// run; its result goes to a buffered channel nobody reads, so it never
// blocks. Every failure is reported (log and daemon_error).
func (d *Daemon) call(ctx context.Context, name, op string, timeout time.Duration, f func(context.Context) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		defer func() {
			if recover() != nil {
				result <- &panicError{frames: panicFrames()}
			}
		}()
		result <- f(ctx)
	}()
	timer := d.clock.NewTimer(timeout)
	defer timer.Stop()
	var err error
	select {
	case err = <-result:
	case <-timer.C():
		err = errAbandoned
	}
	if err != nil {
		d.report(name, op, timeout, err)
	}
	return err
}

// panicFrames returns "function file:line" for the frames of a panicking
// goroutine, from the panic site outwards, without the runtime's own.
// Called from the deferred function that recovered.
func panicFrames() []string {
	pcs := make([]uintptr, maxFrames+8)
	n := runtime.Callers(3, pcs) // skip Callers, panicFrames, the deferred func
	frames := runtime.CallersFrames(pcs[:n])
	var out []string
	for {
		f, more := frames.Next()
		if !strings.HasPrefix(f.Function, "runtime.") {
			out = append(out, fmt.Sprintf("%s %s:%d", f.Function, f.File, f.Line))
		}
		if !more || len(out) == maxFrames {
			return out
		}
	}
}
