package executor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"syscall"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/platform/privilege"
)

// MaxJournalLines is the most lines one journal query may ask for.
const MaxJournalLines = 1000

const (
	journalTimeout   = 10 * time.Second
	journalStdoutMax = 1 << 20
	journalStderrMax = 8 << 10
	maxUnitName      = 255 // systemd's UNIT_NAME_MAX less its NUL
)

// journalUID is nobody, the kernel's overflow id, by number: it needs no account,
// and openSUSE Leap 15 has no such entry in /etc/passwd (measured, D-099).
const journalUID = "65534"

// journalBinaries are the only programs the runner executes.
var journalBinaries = []string{"/usr/bin/journalctl", "/bin/journalctl"}

// ErrJournalUnavailable is matched by every error of Journal that means the
// query could not run or finish, as opposed to a refused query. It wraps the
// cause: a deadline or a cancellation still matches its own error.
var ErrJournalUnavailable = errors.New("executor: journal not available")

// JournalQuery is all a caller of the runner chooses: the last Lines entries
// of one Unit, or of one Source, the name the supervisor gives the output of a
// program it runs (D-100). With neither it is the probe, one line of anything,
// which tells an empty answer from a journal that cannot be read (D-099).
type JournalQuery struct {
	Unit   string
	Source string
	Lines  int
}

// JournalOutput is the result of a finished query.
type JournalOutput struct {
	Stdout    []byte // the newest bytes, oldest entry first
	Truncated bool   // older bytes of Stdout were dropped
	Stderr    []byte // the first bytes
	Exit      Exit
}

// Journal runs journalctl for q. It is the argv-allowlist runner (D-084,
// D-093): sentineld runs this program on an operator's request rather than
// from root's configuration, so the caller chooses no program, option,
// environment, directory or identity. Those are fixed here, and as root the
// program runs as uid 65534 (nobody) with the journal's group, because any
// local user can write what it parses (D-099).
func (e *Executor) Journal(ctx context.Context, q JournalQuery) (JournalOutput, error) {
	spec, err := journalSpec(q, journalBinaries, privilege.System, os.Geteuid())
	if err != nil {
		return JournalOutput{}, err
	}
	return e.runJournal(ctx, spec)
}

// journalArgs builds the argv of q, refusing anything but the agreed shape.
//
// Never --reverse: on Rocky 8 (systemd 239) it skips back the asked number
// of lines and goes on older, so the newest lines are lost (D-099). JSON
// implies --quiet in journalctl, so no option here hides or adds a warning.
//
// A Source query matches the entries sentineld sent itself: SENTINEL_SOURCE
// is a field any writer may set, but journald sets _UID from the sender's
// credentials, and _TRANSPORT=journal leaves out the output streams, which
// cannot carry fields of their own. Only those entries carry a run id worth
// reading: in a unit's, any writer could put one (D-100). The field names are
// the supervisor's (output.go), repeated here as validSource repeats a name's
// grammar, so that the runner depends on nothing it constrains.
func journalArgs(q JournalQuery, euid int) ([]string, error) {
	if q.Lines < 1 || q.Lines > MaxJournalLines {
		return nil, fmt.Errorf("journal query: %d lines is out of range", q.Lines)
	}
	fields := "--output-fields=MESSAGE"
	if q.Source != "" {
		fields += ",SENTINEL_RUN_ID"
	}
	args := []string{"--no-pager", "--output=json", fields, "--lines=" + strconv.Itoa(q.Lines)}
	switch {
	case q.Unit != "" && q.Source != "":
		return nil, errors.New("journal query: a unit or a source, not both")
	case q.Source != "":
		if !validSource(q.Source) {
			return nil, errors.New("journal query: not a source name")
		}
		return append(args, "SENTINEL_SOURCE="+q.Source, "_UID="+strconv.Itoa(euid), "_TRANSPORT=journal"), nil
	case q.Unit == "" && q.Lines != 1:
		return nil, errors.New("journal query: only one line can be asked for without a unit")
	case q.Unit == "":
		return args, nil
	case !validJournalUnit(q.Unit):
		return nil, errors.New("journal query: not a unit name")
	}
	return append(args, "--unit="+q.Unit), nil
}

// validSource reports whether s is a service or job name: the grammar of
// api.ValidName, repeated so that the runner is safe on its own, as
// validJournalUnit is. It cannot start with '-' or '/' (an option, or a path
// journalctl matches as a program) and holds no '+' (journalctl's OR) or '='.
func validSource(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case i > 0 && (c == '.' || c == '_' || c == '-'):
		default:
			return false
		}
	}
	return true
}

// validJournalUnit reports whether s holds only what a unit name may hold.
// It leaves out glob characters, which would widen --unit to other units,
// and a leading dash. The supervisor passes parsed names; this keeps the
// runner safe on its own, which is why it repeats the character set and length
// of systemd.ParseUnitName rather than import a D-Bus client here.
func validJournalUnit(s string) bool {
	if len(s) > maxUnitName || s[0] == '-' {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == ':', r == '-', r == '_', r == '.', r == '\\', r == '@':
		default:
			return false
		}
	}
	return true
}

// journalSpec builds the whole Spec of q from the first of binaries that
// exists. A binary that exists and is not trusted is not passed over: that
// is a fault to report, and the next candidate may be a link to it.
func journalSpec(q JournalQuery, binaries []string, lookup privilege.Lookup, euid int) (Spec, error) {
	args, err := journalArgs(q, euid)
	if err != nil {
		return Spec{}, err
	}
	path, err := firstTrusted(binaries)
	if err != nil {
		return Spec{}, err
	}
	spec := Spec{Path: path, Args: args, Env: []string{"LC_ALL=C"}}
	if euid == 0 {
		cred, err := lookup.Resolve(journalUID, "systemd-journal", 0)
		if err != nil {
			return Spec{}, fmt.Errorf("%w: identity: %w", ErrJournalUnavailable, err)
		}
		spec.Cred = &cred
	}
	return spec, nil
}

func firstTrusted(binaries []string) (string, error) {
	for _, b := range binaries {
		path, err := TrustedExecutable(b)
		switch {
		case err == nil:
			return path, nil
		case !errors.Is(err, fs.ErrNotExist):
			return "", fmt.Errorf("%w: %w", ErrJournalUnavailable, err)
		}
	}
	return "", fmt.Errorf("%w: journalctl: %w", ErrJournalUnavailable, fs.ErrNotExist)
}

// runJournal runs spec within journalTimeout. At the deadline it kills the
// group and answers at once: the reaper finishes the drain alone, about 4 s
// at most with two held pipes, and the buffers it may still write are not
// read here. Every failure to run is ErrJournalUnavailable, and an exit
// status is for the caller to judge.
func (e *Executor) runJournal(ctx context.Context, spec Spec) (JournalOutput, error) {
	ctx, cancel := context.WithTimeout(ctx, journalTimeout)
	defer cancel()
	stdout, stderr := &tailWriter{max: journalStdoutMax}, &capWriter{max: journalStderrMax}
	spec.Stdout, spec.Stderr = stdout, stderr
	p, err := e.Start(ctx, spec)
	if err != nil {
		return JournalOutput{}, fmt.Errorf("%w: %w", ErrJournalUnavailable, err)
	}
	select {
	case <-p.Done():
		return JournalOutput{Stdout: stdout.bytes(), Truncated: stdout.cut(), Stderr: stderr.buf, Exit: p.Exit()}, nil
	case <-ctx.Done():
		_, _ = p.Stop(ctx, syscall.SIGKILL, 0) // ctx has ended: it signals and returns
		return JournalOutput{}, fmt.Errorf("%w: %w", ErrJournalUnavailable, ctx.Err())
	}
}

// tailWriter keeps the newest max bytes written to it. Its Write never fails
// or waits, so a program is never held by a full pipe.
type tailWriter struct {
	max     int
	buf     []byte
	dropped bool
}

// Write implements io.Writer. It compacts only when twice over the bound, so
// a long stream of small writes costs no copy per write.
func (w *tailWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	if len(w.buf) > 2*w.max {
		n := copy(w.buf, w.buf[len(w.buf)-w.max:])
		w.buf = w.buf[:n]
		w.dropped = true
	}
	return len(p), nil
}

func (w *tailWriter) bytes() []byte {
	if len(w.buf) > w.max {
		return w.buf[len(w.buf)-w.max:]
	}
	return w.buf
}

func (w *tailWriter) cut() bool { return w.dropped || len(w.buf) > w.max }

// capWriter keeps the first max bytes written to it. Like tailWriter, it
// never fails or waits.
type capWriter struct {
	max int
	buf []byte
}

// Write implements io.Writer.
func (w *capWriter) Write(p []byte) (int, error) {
	if room := w.max - len(w.buf); room > 0 {
		w.buf = append(w.buf, p[:min(room, len(p))]...)
	}
	return len(p), nil
}
