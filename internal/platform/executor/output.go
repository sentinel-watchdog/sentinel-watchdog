package executor

import (
	"errors"
	"io"
	"os"
	"sync"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/clock"
)

// ErrSinkBlocked is reported by Process.OutputErr when a sink's Write was
// still blocked after the drain timeouts and its copy was abandoned.
var ErrSinkBlocked = errors.New("executor: output sink blocked; its copy was abandoned")

// output is one stream of a child: a pipe whose write end the child gets
// and whose read end one goroutine copies into the caller's writer. A pipe
// of our own, rather than exec.Cmd's, lets the reaper kill the group as
// soon as the leader exits and then bound the reading (drain).
type output struct {
	child  *os.File // the write end, the child's stdout or stderr; nil: /dev/null
	read   *os.File
	w      io.Writer
	copied chan struct{}

	mu      sync.Mutex
	sinkErr error // the first write error of w
}

// newOutput prepares a stream for w; nil discards it.
func newOutput(w io.Writer) (*output, error) {
	if w == nil {
		return &output{}, nil
	}
	r, wr, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	return &output{child: wr, read: r, w: w, copied: make(chan struct{})}, nil
}

// started closes the parent's copy of the write end (only the child's
// remains, so reading ends with the group) and starts copying.
func (o *output) started() {
	if o.read == nil {
		return
	}
	_ = o.child.Close()
	go func() {
		defer close(o.copied)
		// Reading goes on after a sink error, discarding: a pipe nobody
		// reads would block the program once full (review R2).
		_, _ = io.Copy(sinkWriter{o}, o.read)
	}()
}

// abort releases a stream whose process did not start.
func (o *output) abort() {
	if o.read == nil {
		return
	}
	_ = o.child.Close()
	_ = o.read.Close()
}

// drain waits for the copy to reach the end of the output, at most
// timeout: a descendant that left the group may hold the pipe open. Then
// the read end is closed, which ends a copy blocked reading. A copy blocked
// in the sink's Write is waited for another timeout, then abandoned and
// reported as ErrSinkBlocked: Go cannot interrupt an arbitrary Write, and
// the copy ends when it returns (review R3). The executor's sinks are the
// log and a local file; a sink blocked for good blocks sentineld's own
// logging too, which is the failure to fix.
func (o *output) drain(c clock.Clock, timeout time.Duration) {
	if o.read == nil {
		return
	}
	wait := func() bool {
		timer := c.NewTimer(timeout)
		defer timer.Stop()
		select {
		case <-o.copied:
			return true
		case <-timer.C():
			return false
		}
	}
	if !wait() {
		_ = o.read.Close()
		if !wait() {
			o.mu.Lock()
			if o.sinkErr == nil {
				o.sinkErr = ErrSinkBlocked
			}
			o.mu.Unlock()
		}
	}
	_ = o.read.Close() // also after a complete copy: release the descriptor
}

// err returns the first write error of the sink.
func (o *output) err() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.sinkErr
}

// sinkWriter writes to the sink until its first error, then discards.
type sinkWriter struct{ o *output }

func (s sinkWriter) Write(p []byte) (int, error) {
	if s.o.err() != nil {
		return len(p), nil
	}
	n, err := s.o.w.Write(p)
	if err == nil && n < len(p) {
		err = io.ErrShortWrite
	}
	if err != nil {
		s.o.mu.Lock()
		s.o.sinkErr = err
		s.o.mu.Unlock()
	}
	return len(p), nil
}
