// Package audit keeps sentineld's audit log (ADR-0011, D-077): what was
// asked of the daemon and what it did, one JSON record per line in
// audit.jsonl under the state directory, each record chained to the
// previous one by its SHA-256 hash.
//
// The log is tamper-evident, not tamper-proof: root can rewrite it. A
// head file (audit_head.json) remembers the last record and the record
// before the oldest one retained, so truncation, replacement and removal
// of a retained file are detected at start-up. A broken chain is reported
// and never repaired: the log then refuses every new record, so the
// daemon refuses operate and admin requests, until the operator archives
// the evidence and restarts sentineld. Each record is also logged, so a
// copy leaves the host through the journal.
package audit

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"sync"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/clock"
)

// Size bounds: the active file and four rotated ones of at most 10 MiB
// each (ADR-0011 defaults, fixed).
const (
	maxFileBytes  = 10 << 20
	retainedFiles = 5
)

// Integrity states.
const (
	StateOK     = "ok"
	StateBroken = "broken"
)

// Reasons for a broken chain. They are fixed codes: status shows them
// without any content of the files.
const (
	ReasonUnreadable   = "unreadable"     // a file could not be read, or is not a safe regular file
	ReasonBadRecord    = "bad_record"     // a line is malformed or its hash does not match it
	ReasonTornTail     = "torn_tail"      // a file does not end with a complete line
	ReasonGap          = "gap"            // a record does not follow the previous one
	ReasonAnchor       = "anchor_missing" // the oldest retained records were removed
	ReasonHeadMissing  = "head_missing"   // records without a head file
	ReasonHeadCorrupt  = "head_corrupt"   // the head file cannot be trusted
	ReasonHeadAhead    = "head_ahead"     // records the head knows are gone: truncation or replacement
	ReasonHeadMismatch = "head_mismatch"  // the last record is not the one the head knows
	ReasonWriteFailed  = "write_failed"   // a write failed, or an outcome was lost: the end of the chain is uncertain

	reasonCancelled = "cancelled" // Tail's caller gave up; never an integrity state
)

var (
	// ErrBroken reports that the chain is broken: nothing is recorded.
	ErrBroken = errors.New("audit: the audit chain is broken; no record is written until the evidence is restored and sentineld restarted")
	// ErrClosed reports a record after Close.
	ErrClosed = errors.New("audit: log closed")
)

// Integrity is the result of the last check of the chain.
type Integrity struct {
	State string
	// Reason is one of the Reason codes when State is StateBroken; Where
	// names the file and line, never their content.
	Reason, Where string
	// Seq is the sequence number of the last record written.
	Seq uint64
}

// Options configure a Log.
type Options struct {
	// Clock stamps the records; defaults to clock.Real().
	Clock clock.Clock
	// Logger receives a copy of every record at info, and the log's own
	// failures; defaults to slog.Default().
	Logger *slog.Logger
}

// Log is the audit log. It is safe for concurrent use.
type Log struct {
	root    *os.Root
	euid    int
	clock   clock.Clock
	log     *slog.Logger
	maxFile int64
	// syncDir makes a change of the directory durable; tests make it fail.
	syncDir func(*os.Root) error

	// sem serialises file access; unlike a mutex, waiting for it honours
	// a context. A write blocked in the kernel still holds it: Go cannot
	// interrupt a write or an fsync, so the next callers give up at their
	// deadline instead of piling up.
	sem    chan struct{}
	active *os.File // nil while broken or closed
	size   int64
	head   head

	mu        sync.Mutex // guards integrity and closed
	integrity Integrity
	// closed is set when Close begins: no record starts after it, even if
	// Close gives up waiting for a write in progress.
	closed bool
}

// Open checks the chain in root, the state directory, and opens the log
// for appending. Root becomes the Log's: Close closes it. A broken chain is
// not an error: the Log reports it in Integrity and refuses records. Open
// fails only if the files cannot be created or opened.
func Open(root *os.Root, opts Options) (*Log, error) {
	return open(root, opts, maxFileBytes)
}

func open(root *os.Root, opts Options, maxFile int64) (*Log, error) {
	if opts.Clock == nil {
		opts.Clock = clock.Real()
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	l := &Log{root: root, euid: os.Geteuid(), clock: opts.Clock, log: opts.Logger, maxFile: maxFile,
		syncDir: syncDir, sem: make(chan struct{}, 1)}
	h, integrity, err := l.check()
	if err != nil {
		return nil, err
	}
	l.integrity = integrity
	if integrity.State == StateBroken {
		l.log.Error("audit chain broken: operate and admin requests are refused until the evidence is restored",
			"reason", integrity.Reason, "where", integrity.Where)
		return l, nil
	}
	l.head = h
	if err := l.openActive(); err != nil {
		return nil, fmt.Errorf("audit: open %s: %w", activeFile, err)
	}
	return l, nil
}

// check verifies the retained files against the head and returns the head
// to continue from. A fresh state directory gets the genesis head.
func (l *Log) check() (head, Integrity, error) {
	broken := func(reason, where string) (head, Integrity, error) {
		return head{}, Integrity{State: StateBroken, Reason: reason, Where: where}, nil
	}
	h, err := readHead(l.root, l.euid)
	haveHead := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return broken(ReasonHeadCorrupt, headFile)
	}
	if !haveHead {
		h = head{Last: genesis, Anchor: genesis}
	}
	var prev Record
	count, anchorSeen := 0, false
	for i := retainedFiles - 1; i >= 0; i-- {
		name := fileName(i)
		reason, where := l.eachLine(name, func(n int, line []byte) (string, string) {
			r, err := parse(line)
			if err != nil {
				return ReasonBadRecord, fmt.Sprintf("%s line %d", name, n)
			}
			if count > 0 && (r.Seq != prev.Seq+1 || r.PrevHash != prev.Hash) {
				return ReasonGap, fmt.Sprintf("%s line %d", name, n)
			}
			// The anchor is the record before the oldest one kept, or one
			// of the records of a file whose expiry was interrupted.
			if (r.Seq == h.Anchor.Seq+1 && r.PrevHash == h.Anchor.Hash) || (r.Seq == h.Anchor.Seq && r.Hash == h.Anchor.Hash) {
				anchorSeen = true
			}
			prev, count = r, count+1
			return "", ""
		})
		if reason != "" {
			return broken(reason, where)
		}
	}
	switch {
	case !haveHead && count > 0:
		return broken(ReasonHeadMissing, headFile)
	case !haveHead:
		if err := writeHead(l.root, h); err != nil {
			return head{}, Integrity{}, fmt.Errorf("audit: create %s: %w", headFile, err)
		}
	case count == 0 && h.Last != h.Anchor:
		return broken(ReasonHeadAhead, "no record")
	case count == 0:
	case !anchorSeen:
		return broken(ReasonAnchor, "oldest retained file")
	case prev.Seq == h.Last.Seq && prev.Hash == h.Last.Hash:
	case prev.Seq == h.Last.Seq+1 && prev.PrevHash == h.Last.Hash:
		// A crash between writing a record and its head: the record is
		// durable and chained, so the head catches up.
		h.Last = point{prev.Seq, prev.Hash}
		if err := writeHead(l.root, h); err != nil {
			return head{}, Integrity{}, fmt.Errorf("audit: update %s: %w", headFile, err)
		}
		l.log.Warn("audit head was one record behind after an interrupted write; updated", "seq", prev.Seq)
	case prev.Seq < h.Last.Seq:
		return broken(ReasonHeadAhead, fmt.Sprintf("last record %d, head %d", prev.Seq, h.Last.Seq))
	default:
		return broken(ReasonHeadMismatch, fmt.Sprintf("last record %d, head %d", prev.Seq, h.Last.Seq))
	}
	return h, Integrity{State: StateOK, Seq: h.Last.Seq}, nil
}

// eachLine calls visit with every line of name (1-based number, without
// the newline), reading at most the file's size when opened. A missing
// file has no lines. It returns the first reason visit returns, or one of
// its own: the file is not safe to read, too large, has a line longer than
// MaxLineBytes, or does not end with a newline.
func (l *Log) eachLine(name string, visit func(n int, line []byte) (reason, where string)) (string, string) {
	return l.lines(name, false, visit)
}

// lines is eachLine; with tolerant, a line longer than MaxLineBytes is
// read to its end and passed to visit truncated (it never parses), so a
// reader of a damaged file still sees the records after it.
func (l *Log) lines(name string, tolerant bool, visit func(n int, line []byte) (reason, where string)) (string, string) {
	f, err := openFile(l.root, name, os.O_RDONLY, l.euid)
	if errors.Is(err, fs.ErrNotExist) {
		return "", ""
	}
	if err != nil {
		return ReasonUnreadable, name
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() > l.maxFile+MaxLineBytes {
		return ReasonUnreadable, name
	}
	r := bufio.NewReaderSize(io.LimitReader(f, info.Size()), MaxLineBytes)
	for n := 1; ; n++ {
		line, err := r.ReadSlice('\n')
		switch {
		case err == io.EOF && len(line) == 0:
			return "", ""
		case err == io.EOF:
			return ReasonTornTail, fmt.Sprintf("%s line %d", name, n)
		case errors.Is(err, bufio.ErrBufferFull) && tolerant:
			for errors.Is(err, bufio.ErrBufferFull) { // skip the rest of the line
				_, err = r.ReadSlice('\n')
			}
			if err != nil {
				return ReasonTornTail, fmt.Sprintf("%s line %d", name, n)
			}
			line = []byte{'\n'} // an empty line: never a record
		case errors.Is(err, bufio.ErrBufferFull):
			return ReasonBadRecord, fmt.Sprintf("%s line %d", name, n)
		case err != nil:
			return ReasonUnreadable, name
		}
		if reason, where := visit(n, line[:len(line)-1]); reason != "" {
			return reason, where
		}
	}
}

// openActive opens the active file for appending, creating it if it is
// missing (a fresh directory, or a crash during rotation).
func (l *Log) openActive() error {
	f, err := openFile(l.root, activeFile, os.O_WRONLY|os.O_APPEND, l.euid)
	if errors.Is(err, fs.ErrNotExist) {
		if f, err = createFile(l.root, activeFile); err == nil {
			err = l.syncDir(l.root)
		}
	}
	if err != nil {
		if f != nil {
			_ = f.Close() // the open error is the one to report
		}
		return err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close() // the stat error is the one to report
		return err
	}
	l.active, l.size = f, info.Size()
	return nil
}

// Integrity returns the result of the last check of the chain.
func (l *Log) Integrity() Integrity {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.integrity
}

// acquire waits for the files, at most until ctx ends; an ended ctx never
// gets them.
func (l *Log) acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("audit: log busy: %w", err)
	}
	select {
	case l.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("audit: log busy: %w", ctx.Err())
	}
}

func (l *Log) release() { <-l.sem }

// Record appends e and returns the record written, once it is durable: the
// line is written and synced, then the head. On any failure the end of the
// chain is uncertain, so the log marks itself broken (ReasonWriteFailed)
// and refuses every later record. So does an outcome (ResultOK,
// ResultFailed) that is not recorded for any other reason (invalid, too
// large, or it cannot even wait for the files): the action it reports has
// run, and the log must not go on as if it were complete. A rejected
// intent or denial changes nothing, since no action ran. A broken log
// returns ErrBroken.
func (l *Log) Record(ctx context.Context, e Entry) (Record, error) {
	if err := e.validate(); err != nil {
		l.lost(e, err)
		return Record{}, err
	}
	if err := l.acquire(ctx); err != nil {
		l.lost(e, err)
		return Record{}, err
	}
	defer l.release()
	switch {
	case l.isClosed():
		return Record{}, ErrClosed
	case l.active == nil || l.Integrity().State == StateBroken:
		return Record{}, ErrBroken
	}
	r := Record{Seq: l.head.Last.Seq + 1, Time: l.clock.Now().UTC(), Actor: e.Actor, Action: e.Action,
		Target: e.Target, Result: e.Result, CorrelationID: e.CorrelationID, Detail: e.Detail,
		PrevHash: l.head.Last.Hash}
	line, hash, err := encode(r)
	if err != nil {
		l.lost(e, err) // nothing written: the chain itself is intact
		return Record{}, err
	}
	r.Hash = hash
	// An outcome lost meanwhile breaks the log without the files (it could
	// not get them): check again before changing anything.
	if l.Integrity().State == StateBroken {
		return Record{}, ErrBroken
	}
	if err := l.append(line, point{r.Seq, hash}); err != nil {
		l.fail(err)
		return Record{}, fmt.Errorf("audit: record not written: %w", err)
	}
	l.mu.Lock()
	l.integrity.Seq = r.Seq
	broken := l.integrity.State == StateBroken
	l.mu.Unlock()
	l.mirror(r)
	if broken && e.Result == ResultIntent {
		// Another outcome was lost while this intent was being written: the
		// intent stays in the log, but its action must not run.
		return r, ErrBroken
	}
	return r, nil
}

// append writes one line, rotating first if it would not fit, then syncs
// it and saves the new head.
func (l *Log) append(line []byte, last point) error {
	if l.size > 0 && l.size+int64(len(line)) > l.maxFile {
		if err := l.rotate(); err != nil {
			return fmt.Errorf("rotate: %w", err)
		}
		if err := l.intact(); err != nil {
			return err
		}
	}
	// One write per line: with O_APPEND a line is never interleaved.
	n, err := l.active.Write(line)
	l.size += int64(n)
	if err != nil {
		return err
	}
	if err := l.active.Sync(); err != nil {
		return err
	}
	h := l.head
	h.Last = last
	if err := writeHead(l.root, h); err != nil {
		return fmt.Errorf("save %s: %w", headFile, err)
	}
	l.head = h
	return nil
}

// intact returns ErrBroken once the log is broken. An outcome lost while
// the files are busy breaks the log without them, so every step that
// changes the files checks it first: nothing starts after the break, only
// a step already in the kernel finishes.
func (l *Log) intact() error {
	if l.Integrity().State == StateBroken {
		return ErrBroken
	}
	return nil
}

// rotate makes the active file the newest rotated one. The oldest file
// expires first: the head's anchor moves to its last record before it is
// removed, so a crash in between leaves a chain that still checks. Each
// step is made durable (a directory sync) before the next, so a later
// rename never reaches the disk without the one before it, and none
// starts once the log is broken.
func (l *Log) rotate() error {
	var steps []func() error
	oldest := fileName(retainedFiles - 1)
	found, err := exists(l.root, oldest)
	if err != nil {
		return err
	}
	if found {
		var last Record
		reason, _ := l.eachLine(oldest, func(_ int, line []byte) (string, string) {
			r, err := parse(line)
			if err != nil {
				return ReasonBadRecord, ""
			}
			last = r
			return "", ""
		})
		if reason != "" || last.Seq == 0 {
			return fmt.Errorf("%s cannot be read to expire it (%s)", oldest, reason)
		}
		steps = append(steps,
			func() error {
				h := l.head
				h.Anchor = point{last.Seq, last.Hash}
				if err := writeHead(l.root, h); err != nil {
					return err
				}
				l.head = h
				return nil
			},
			func() error { return l.durably(l.root.Remove(oldest)) })
	}
	for i := retainedFiles - 2; i >= 1; i-- {
		steps = append(steps, func() error {
			err := l.root.Rename(fileName(i), fileName(i+1))
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return l.durably(err)
		})
	}
	steps = append(steps,
		func() error {
			err := l.active.Close()
			l.active = nil
			if err != nil {
				return err
			}
			return l.durably(l.root.Rename(activeFile, fileName(1)))
		},
		l.openActive)
	for _, step := range steps {
		if err := l.intact(); err != nil {
			return err
		}
		if err := step(); err != nil {
			return err
		}
	}
	return nil
}

// durably syncs the directory after a change that succeeded.
func (l *Log) durably(err error) error {
	if err != nil {
		return err
	}
	return l.syncDir(l.root)
}

// fail marks the log broken after a failed write. The caller holds sem.
func (l *Log) fail(err error) {
	if l.active != nil {
		_ = l.active.Close() // already failing; no more writes
		l.active = nil
	}
	l.markBroken(ReasonWriteFailed, activeFile)
	l.log.Error("audit record not written: operate and admin requests are refused until sentineld restarts", "error", err)
}

// lost marks the log broken when e is an outcome that was not recorded.
func (l *Log) lost(e Entry, err error) {
	if e.Result != ResultOK && e.Result != ResultFailed {
		return
	}
	l.markBroken(ReasonWriteFailed, "outcome of "+e.Action+" not recorded")
	l.log.Error("audit outcome not recorded: operate and admin requests are refused until sentineld restarts",
		"action", e.Action, "error", err)
}

// markBroken records why the log refuses new records; the first reason
// stays.
func (l *Log) markBroken(reason, where string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.integrity.State != StateBroken {
		l.integrity = Integrity{State: StateBroken, Reason: reason, Where: where, Seq: l.integrity.Seq}
	}
}

// mirror logs a copy of r, which leaves the host through the journal.
func (l *Log) mirror(r Record) {
	attrs := []any{"seq", r.Seq, "action", r.Action, "result", r.Result, "actor", r.Actor.Kind}
	if r.Actor.UID != nil {
		attrs = append(attrs, "uid", *r.Actor.UID)
	}
	if r.Actor.GID != nil {
		attrs = append(attrs, "gid", *r.Actor.GID)
	}
	if r.Actor.PID != 0 {
		attrs = append(attrs, "pid", r.Actor.PID)
	}
	if r.Target != "" {
		attrs = append(attrs, "target", r.Target)
	}
	if r.CorrelationID != "" {
		attrs = append(attrs, "correlation_id", r.CorrelationID)
	}
	if len(r.Detail) > 0 {
		attrs = append(attrs, "detail", r.Detail)
	}
	l.log.Info("audit", append(attrs, "hash", r.Hash)...)
}

// Tail returns up to limit of the last records, oldest first, and how many
// lines it skipped because they do not parse. It reads the retained files
// as they are, broken or not: Integrity says whether they can be trusted.
// It stops when ctx ends, since it holds the files that records wait for.
func (l *Log) Tail(ctx context.Context, limit int) ([]Record, int, error) {
	if limit <= 0 {
		return nil, 0, nil
	}
	if err := l.acquire(ctx); err != nil {
		return nil, 0, err
	}
	defer l.release()
	if l.isClosed() {
		return nil, 0, ErrClosed
	}
	var out []Record
	skipped := 0
	for i := 0; i < retainedFiles && len(out) < limit; i++ {
		want := limit - len(out)
		var ring []Record
		reason, _ := l.lines(fileName(i), true, func(_ int, line []byte) (string, string) {
			if ctx.Err() != nil {
				return reasonCancelled, ""
			}
			if r, err := parse(line); err == nil {
				ring = append(ring, r)
				if len(ring) > want {
					ring = ring[1:]
				}
			} else {
				skipped++
			}
			return "", ""
		})
		switch reason {
		case reasonCancelled:
			return nil, 0, fmt.Errorf("audit: read: %w", ctx.Err())
		case ReasonUnreadable:
			return nil, 0, fmt.Errorf("audit: %s cannot be read", fileName(i))
		}
		out = append(ring, out...)
	}
	return out, skipped, nil
}

func (l *Log) isClosed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closed
}

// Close refuses every later record at once, then closes the active file
// and the state directory handle, waiting for a record being written
// until ctx ends. If it gave up, a later Close tries again; once the files
// are closed, Close does nothing.
func (l *Log) Close(ctx context.Context) error {
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
	// Free files are taken even when ctx has ended: only a write in
	// progress makes Close give up.
	select {
	case l.sem <- struct{}{}:
	default:
		if err := l.acquire(ctx); err != nil {
			return fmt.Errorf("audit: a record was still being written: %w", err)
		}
	}
	defer l.release()
	if l.root == nil {
		return nil // already closed
	}
	var errs []error
	if l.active != nil {
		errs = append(errs, l.active.Close())
		l.active = nil
	}
	errs = append(errs, l.root.Close())
	l.root = nil
	return errors.Join(errs...)
}
