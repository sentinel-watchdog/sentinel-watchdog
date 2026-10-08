// Package logging builds sentineld's structured logger on top of log/slog.
//
// Formats:
//   - text:    logfmt-style lines with timestamps, for foreground use;
//   - json:    one JSON object per line;
//   - journal: logfmt without timestamps, prefixed with a "<N>" syslog
//     priority that journald turns into the entry's PRIORITY field;
//   - auto:    journal when stderr is connected to journald (JOURNAL_STREAM),
//     text otherwise.
//
// Every handler redacts attributes whose key, or enclosing group, looks
// secret (see internal/core/redact).
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/redact"
)

// Format is a log output encoding.
type Format string

// Supported formats.
const (
	FormatAuto    Format = "auto"
	FormatText    Format = "text"
	FormatJSON    Format = "json"
	FormatJournal Format = "journal"
)

// Options configure New.
type Options struct {
	// Level is usually a *slog.LevelVar so it can change on reload.
	Level  slog.Leveler
	Format Format
	// Output defaults to os.Stderr.
	Output io.Writer
	// Getenv defaults to os.Getenv; injected by tests.
	Getenv func(string) string
}

// ParseLevel parses debug, info, warn or error.
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("unknown log level %q (debug, info, warn, error)", s)
}

// New returns a logger configured by opts.
func New(opts Options) (*slog.Logger, error) {
	out := opts.Output
	if out == nil {
		out = os.Stderr
	}
	getenv := opts.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	level := opts.Level
	if level == nil {
		level = slog.LevelInfo
	}

	format := opts.Format
	if format == "" || format == FormatAuto {
		format = FormatText
		if f, ok := out.(*os.File); ok && IsJournalStream(f, getenv("JOURNAL_STREAM")) {
			format = FormatJournal
		}
	}

	hopts := &slog.HandlerOptions{Level: level, ReplaceAttr: redactAttr}
	switch format {
	case FormatText:
		return slog.New(slog.NewTextHandler(out, hopts)), nil
	case FormatJSON:
		return slog.New(slog.NewJSONHandler(out, hopts)), nil
	case FormatJournal:
		return slog.New(newJournalHandler(out, level)), nil
	}
	return nil, fmt.Errorf("unknown log format %q (auto, text, json, journal)", format)
}

// redactAttr masks values of sensitive keys at any nesting level, and
// every value inside a group whose name is sensitive ("credentials").
func redactAttr(groups []string, a slog.Attr) slog.Attr {
	if a.Value.Kind() == slog.KindGroup {
		return a
	}
	if redact.IsSensitiveKey(a.Key) || slices.ContainsFunc(groups, redact.IsSensitiveKey) {
		return slog.String(a.Key, redact.Placeholder)
	}
	return a
}

// IsJournalStream reports whether f is the stream systemd connected to
// journald, as advertised by JOURNAL_STREAM="<device>:<inode>".
func IsJournalStream(f *os.File, journalStream string) bool {
	devStr, inoStr, ok := strings.Cut(journalStream, ":")
	if !ok || !isDigits(devStr) || !isDigits(inoStr) {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	// Compare in decimal form: Stat_t field types differ across platforms.
	return fmt.Sprint(st.Dev) == devStr && fmt.Sprint(st.Ino) == inoStr
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// journalHandler writes logfmt records without timestamps, each prefixed by
// "<priority>" (sd-daemon(3) convention, honoured by journald's
// SyslogLevelPrefix=yes default).
type journalHandler struct {
	inner slog.Handler
	pw    *prefixWriter
}

// prefixWriter injects the priority prefix of the record being written.
// slog.TextHandler performs exactly one Write per record, and the shared
// mutex serialises prefix selection with that write.
type prefixWriter struct {
	mu       sync.Mutex
	w        io.Writer
	priority int
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	line := make([]byte, 0, len(b)+3)
	line = append(line, '<')
	line = strconv.AppendInt(line, int64(p.priority), 10)
	line = append(line, '>')
	line = append(line, b...)
	if _, err := p.w.Write(line); err != nil {
		return 0, err
	}
	return len(b), nil
}

func newJournalHandler(w io.Writer, level slog.Leveler) *journalHandler {
	pw := &prefixWriter{w: w}
	inner := slog.NewTextHandler(pw, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Attr{} // journald timestamps entries itself
			}
			return redactAttr(groups, a)
		},
	})
	return &journalHandler{inner: inner, pw: pw}
}

func (h *journalHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h *journalHandler) Handle(ctx context.Context, r slog.Record) error {
	h.pw.mu.Lock()
	defer h.pw.mu.Unlock()
	h.pw.priority = syslogPriority(r.Level)
	return h.inner.Handle(ctx, r)
}

func (h *journalHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &journalHandler{inner: h.inner.WithAttrs(attrs), pw: h.pw}
}

func (h *journalHandler) WithGroup(name string) slog.Handler {
	return &journalHandler{inner: h.inner.WithGroup(name), pw: h.pw}
}

// syslogPriority maps slog levels to syslog severities.
func syslogPriority(l slog.Level) int {
	switch {
	case l >= slog.LevelError:
		return 3 // err
	case l >= slog.LevelWarn:
		return 4 // warning
	case l >= slog.LevelInfo:
		return 6 // info
	default:
		return 7 // debug
	}
}
