package executor

import (
	"fmt"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/clock"
)

// LineWriter turns a child's output into lines for a sink such as the log
// (D-009 `log`), bounded so that a chatty or crash-looping child cannot
// flood it: a line longer than MaxLine is cut, and beyond Rate lines per
// second (with bursts of Burst) lines are dropped and counted. It is
// written by one goroutine (the stream's copy); Dropped may be read
// concurrently.
type LineWriter struct {
	emit  func(line string)
	clock clock.Clock
	opts  LineOptions

	buf      []byte
	cutting  bool // the current line was cut: skip to its end
	tokens   float64
	last     time.Time
	dropped  atomic.Uint64
	reported uint64 // drops already announced
}

// LineOptions bound a LineWriter.
type LineOptions struct {
	MaxLine int     // bytes per line; longer lines are cut
	Rate    float64 // lines per second, sustained
	Burst   int     // lines at once
}

// DefaultLineOptions suit the log: 4 KiB lines, 100 lines/s, bursts of 1000.
var DefaultLineOptions = LineOptions{MaxLine: 4 << 10, Rate: 100, Burst: 1000}

// NewLineWriter returns a writer that calls emit for each complete line,
// without its newline.
func NewLineWriter(c clock.Clock, opts LineOptions, emit func(line string)) *LineWriter {
	return &LineWriter{emit: emit, clock: c, opts: opts, tokens: float64(opts.Burst), last: c.Now()}
}

// Write implements io.Writer; it never fails.
func (w *LineWriter) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := indexNewline(p)
		chunk := p
		if i >= 0 {
			chunk = p[:i]
		}
		if !w.cutting {
			room := w.opts.MaxLine - len(w.buf)
			if len(chunk) > room {
				w.buf = append(w.buf, chunk[:room]...)
				w.line(true)
				w.cutting = true
			} else {
				w.buf = append(w.buf, chunk...)
			}
		}
		if i < 0 {
			break
		}
		if !w.cutting {
			w.line(false)
		}
		w.cutting = false
		p = p[i+1:]
	}
	return n, nil
}

// Close emits a last line without a newline, if any.
func (w *LineWriter) Close() error {
	if len(w.buf) > 0 && !w.cutting {
		w.line(false)
	}
	w.buf, w.cutting = w.buf[:0], false
	return nil
}

// Dropped returns how many lines were dropped by the rate limit.
func (w *LineWriter) Dropped() uint64 { return w.dropped.Load() }

func (w *LineWriter) line(cut bool) {
	s := string(w.buf)
	w.buf = w.buf[:0]
	if !w.allow() {
		w.dropped.Add(1)
		return
	}
	if d := w.dropped.Load(); d > w.reported {
		w.emit(fmt.Sprintf("[%d lines dropped: output faster than %g lines/s]", d-w.reported, w.opts.Rate))
		w.reported = d
	}
	if cut {
		s = trimToRune(s) + " [line cut]"
	}
	w.emit(s)
}

// allow takes a token from the bucket, refilled at Rate per second.
func (w *LineWriter) allow() bool {
	now := w.clock.Now()
	w.tokens = min(float64(w.opts.Burst), w.tokens+now.Sub(w.last).Seconds()*w.opts.Rate)
	w.last = now
	if w.tokens < 1 {
		return false
	}
	w.tokens--
	return true
}

func indexNewline(p []byte) int {
	for i, b := range p {
		if b == '\n' {
			return i
		}
	}
	return -1
}

// trimToRune drops a UTF-8 sequence cut in the middle at the end of s.
func trimToRune(s string) string {
	for i := 0; i < utf8.UTFMax && len(s) > 0; i++ {
		r, size := utf8.DecodeLastRuneInString(s)
		if r != utf8.RuneError || size != 1 {
			break
		}
		s = s[:len(s)-1]
	}
	return s
}
