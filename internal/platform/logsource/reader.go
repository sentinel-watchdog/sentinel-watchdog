// Package logsource reads the recent output of a service: from the journal
// through the executor's runner, or from its output file (D-093, D-099).
// Everything it reads may come from any local user or from the service, so
// everything is bounded and passes through the caller's redaction, and a
// result that says nothing was found is never the result of a failure.
package logsource

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/platform/executor"
)

// Bounds of one read (D-099).
const (
	maxSlots     = 4
	maxFileTail  = 1 << 20
	maxRecord    = 64 << 10
	maxLine      = 4 << 10
	maxResponse  = 256 << 10
	lineOverhead = 64 // a line's JSON beyond its text: the keys and the time
	tooLong      = "[entry over 4 KiB not shown]"
)

// Errors of a read. The sentinels name no path, but what they wrap may: a
// caller must not show err.Error() to a client.
var (
	ErrUnavailable = errors.New("logsource: not available")
	ErrBusy        = errors.New("logsource: too many reads at once")
	ErrUnsafeFile  = errors.New("logsource: the output file is not safe to read")
	errNoRedactor  = errors.New("logsource: a read needs a redactor")
)

// Runner runs a journal query. The variation it isolates is the real
// journalctl versus the canned output of the tests, which need no journal.
type Runner interface {
	Journal(ctx context.Context, q executor.JournalQuery) (executor.JournalOutput, error)
}

// Line is one line of output. Time is zero when the source has none (a file).
type Line struct {
	Time time.Time
	Text string
}

// Result is what a read found, oldest line first. Lines is never nil.
// Truncated: lines the caller asked for may be missing.
type Result struct {
	Lines     []Line
	Truncated bool
}

// Reader reads logs, a bounded number at once. A nil Runner means no journal
// on this host: Unit answers ErrUnavailable and File still works.
type Reader struct {
	run   Runner
	log   *slog.Logger
	slots chan struct{}
	open  func(path string) (*os.File, error) // executor.OpenOutput, but in the tests
}

// NewReader returns a Reader that runs journal queries through r.
func NewReader(r Runner, log *slog.Logger) *Reader {
	return &Reader{run: r, log: log, slots: make(chan struct{}, maxSlots), open: executor.OpenOutput}
}

// Named resolves to a unit whose name is known.
func Named(unit string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) { return unit, nil }
}

// Unit returns the last n entries of the unit resolve names, as lines. Finding
// that name is part of the read, under its slot. An empty journal answer
// proves nothing: journalctl also exits 0 with no output when it finds no
// journal file (measured on systemd 239 and 259), so a probe must find
// something readable before "no lines" is the answer (D-099).
func (r *Reader) Unit(ctx context.Context, resolve func(context.Context) (string, error), n int, redact func(string) string) (Result, error) {
	if redact == nil {
		return Result{}, errNoRedactor
	}
	if r.run == nil {
		return Result{}, fmt.Errorf("%w: no journal runner", ErrUnavailable)
	}
	release, ok := r.acquire()
	if !ok {
		return Result{}, ErrBusy
	}
	defer release()
	unit, err := resolve(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("%w: the unit's name: %w", ErrUnavailable, err)
	}
	out, err := r.query(ctx, executor.JournalQuery{Unit: unit, Lines: n}, redact)
	if err != nil {
		return Result{}, err
	}
	records, entries, skipped := parseJournal(out.Stdout, out.Truncated)
	warned := len(out.Stderr) > 0
	if warned {
		// journalctl read what it could: a truncated or corrupt file is when
		// an operator most needs the others (D-099).
		r.log.Warn("journal read with warnings", "unit", unit, "stderr", redact(string(out.Stderr)))
	}
	if entries == 0 && skipped == 0 && !out.Truncated {
		probe, err := r.query(ctx, executor.JournalQuery{Lines: 1}, redact)
		if err != nil {
			return Result{}, err
		}
		if _, found, _ := parseJournal(probe.Stdout, probe.Truncated); found == 0 {
			return Result{}, fmt.Errorf("%w: the journal has nothing readable", ErrUnavailable)
		}
	}
	res := shape(records, n, redact)
	res.Truncated = res.Truncated || warned || skipped > 0 || (out.Truncated && len(res.Lines) < n)
	return res, nil
}

// query runs q. A refused query stays what it is; any other failure, an exit
// status included, is ErrUnavailable, with the details only in the log.
func (r *Reader) query(ctx context.Context, q executor.JournalQuery, redact func(string) string) (executor.JournalOutput, error) {
	out, err := r.run.Journal(ctx, q)
	switch {
	case errors.Is(err, executor.ErrJournalUnavailable):
		return executor.JournalOutput{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	case err != nil:
		return executor.JournalOutput{}, err
	case out.Exit.Code != 0 || out.Exit.Signal != "":
		r.log.Warn("journal read failed", "unit", q.Unit, "exit", out.Exit.String(), "stderr", redact(string(out.Stderr)))
		return executor.JournalOutput{}, fmt.Errorf("%w: journalctl: %s", ErrUnavailable, out.Exit)
	}
	return out, nil
}

// File returns the last n lines of the output file at path; a missing file
// has none. The whole read runs in a goroutine that keeps its slot until it
// ends, and the caller stops waiting when ctx ends: Go cannot interrupt a read
// of a stalled filesystem, and Daemon.stop waits for commands before it stops
// a module. A stuck read therefore holds one slot and one goroutine and nothing
// else, and each Reader has at most maxSlots of them (D-099).
func (r *Reader) File(ctx context.Context, path string, n int, redact func(string) string) (Result, error) {
	if redact == nil {
		return Result{}, errNoRedactor
	}
	release, ok := r.acquire()
	if !ok {
		return Result{}, ErrBusy
	}
	type outcome struct {
		res Result
		err error
	}
	done := make(chan outcome, 1) // the goroutine never waits for its reader
	go func() {
		defer release()
		res, err := r.readFile(path, n, redact)
		done <- outcome{res, err}
	}()
	select {
	case o := <-done:
		return o.res, o.err
	case <-ctx.Done():
		return Result{}, fmt.Errorf("%w: %w", ErrUnavailable, ctx.Err())
	}
}

func (r *Reader) readFile(path string, n int, redact func(string) string) (Result, error) {
	data, cut, err := r.readTail(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return shape(nil, n, redact), nil
	case err != nil:
		return Result{}, err
	}
	return shapeFile(data, cut, n, redact), nil
}

// readTail reads at most the last maxFileTail bytes of path. It does not
// chase what is appended meanwhile. cut: the read does not start at the
// beginning of the file.
func (r *Reader) readTail(path string) ([]byte, bool, error) {
	f, err := r.open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, err
	}
	if err != nil {
		return nil, false, fmt.Errorf("%w: %w", ErrUnsafeFile, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, false, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	offset := max(0, info.Size()-maxFileTail)
	data, err := io.ReadAll(io.NewSectionReader(f, offset, maxFileTail))
	if err != nil {
		return nil, false, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return data, offset > 0, nil
}

// acquire takes a slot without waiting; release gives it back.
func (r *Reader) acquire() (release func(), ok bool) {
	select {
	case r.slots <- struct{}{}:
		return func() { <-r.slots }, true
	default:
		return nil, false
	}
}
