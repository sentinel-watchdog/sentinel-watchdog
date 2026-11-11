package supervisor

import (
	"errors"
	"time"
	"unicode/utf8"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/platform/journald"
)

// The fields of an entry of program output (D-100). SENTINEL_SOURCE names
// the service or job, which share one name space; a query matches it together
// with _UID, which journald sets and nobody else can.
const (
	fieldMessage    = "MESSAGE"
	fieldPriority   = "PRIORITY"
	fieldIdentifier = "SYSLOG_IDENTIFIER"
	fieldSource     = "SENTINEL_SOURCE"
	fieldStream     = "SENTINEL_STREAM"
	fieldRunID      = "SENTINEL_RUN_ID"
)

// priorityInfo is syslog's info, systemd's default for a service's stdout
// and stderr alike.
const priorityInfo = "6"

// A full journal socket is retried every journalRetryEvery for at most
// journalRetryFor: journald's queue holds 512 entries, and a burst of 1000
// lines it is still draining lost a third of them without a retry, none with
// it (measured on systemd 239 and 259, D-100).
const (
	journalRetryEvery = time.Millisecond
	journalRetryFor   = 20 * time.Millisecond
)

// maxMessage is the longest MESSAGE sent: journalctl's JSON prints a field of
// 4096 bytes or more as null, counting "MESSAGE=" on systemd 259 and the value
// alone on 239 (measured), and a line read back as null shows nothing.
const maxMessage = 4096 - len(fieldMessage+"=") - 1

// lineCut marks a line cut short, as executor.LineWriter marks its own cuts.
const lineCut = " [line cut]"

const fallbackWarning = "supervisor cannot send program output to the journal; it goes to sentineld's log, where supervisor logs does not find it"

// outputSink receives the lines of one stream of one launch. The LineWriter
// calls line from a single goroutine, the stream's copy, so congested needs no
// lock; what sinks share, the drop count and the warning, is atomic on the
// instance.
type outputSink struct {
	in     *instance
	kind   string // "service" or "job"
	name   string
	runID  string // a job's run; "" for a service
	stream string // "stdout" or "stderr"
	redact func(string) string
	// congested: the stream's last line was dropped on a full socket, so the
	// next one is not waited for until journald takes a line again.
	congested bool
}

// line sends one line of output: to the journal as an entry of its own when
// sentineld's stderr is journald's (Options.JournalSend), else to the logger.
// A full journal is retried for at most journalRetryFor, then the line is
// dropped and counted, never sent to the logger: the logger writes to the same
// journald. Any other send error, such as a missing socket while journald
// restarts, falls back to the logger, which keeps the logger's own risk of
// blocking (B-004).
func (s *outputSink) line(text string) {
	text = s.redact(text)
	if !s.in.nativeJournal() {
		s.toLogger(text)
		return
	}
	err := s.send(journald.Encode(s.fields(fitMessage(text))))
	switch {
	case err == nil:
		s.congested = false
		s.in.journalWarned.CompareAndSwap(true, false) // a later outage is warned about again
	case errors.Is(err, journald.ErrFull):
		s.congested = true
		s.in.outputDropped.Add(1)
	default:
		if s.in.journalWarned.CompareAndSwap(false, true) {
			s.in.log.Warn(fallbackWarning, "error", err)
		}
		s.toLogger(text)
	}
}

// send sends entry, waiting for a full socket unless the stream is congested.
func (s *outputSink) send(entry []byte) error {
	err := s.in.journalSend(entry)
	for waited := time.Duration(0); errors.Is(err, journald.ErrFull) && !s.congested && waited < journalRetryFor; waited += journalRetryEvery {
		t := s.in.rt.Clock.NewTimer(journalRetryEvery)
		<-t.C()
		err = s.in.journalSend(entry)
	}
	return err
}

func (s *outputSink) fields(text string) []journald.Field {
	f := []journald.Field{
		{Name: fieldMessage, Value: text},
		{Name: fieldPriority, Value: priorityInfo},
		{Name: fieldIdentifier, Value: "sentineld/" + s.name},
		{Name: fieldSource, Value: s.name},
		{Name: fieldStream, Value: s.stream},
	}
	if s.runID != "" {
		f = append(f, journald.Field{Name: fieldRunID, Value: s.runID})
	}
	return f
}

// toLogger writes the line to sentineld's log, the only place for it without
// the journal, as before Phase 3c-5b.
func (s *outputSink) toLogger(text string) {
	attrs := []any{s.kind, s.name}
	if s.runID != "" {
		attrs = append(attrs, "run_id", s.runID)
	}
	s.in.log.Info(s.kind+" output", append(attrs, "stream", s.stream, "line", text)...)
}

// fitMessage cuts text to maxMessage bytes on a rune boundary, marked once:
// the LineWriter's own cut is 4 KiB, and redaction can lengthen a line.
func fitMessage(text string) string {
	if len(text) <= maxMessage {
		return text
	}
	if len(text) >= len(lineCut) && text[len(text)-len(lineCut):] == lineCut {
		text = text[:len(text)-len(lineCut)]
	}
	text = text[:maxMessage-len(lineCut)]
	for range utf8.UTFMax - 1 { // only a rune the cut split: invalid bytes of the program's stay
		r, size := utf8.DecodeLastRuneInString(text)
		if r != utf8.RuneError || size != 1 {
			break
		}
		text = text[:len(text)-1]
	}
	return text + lineCut
}
