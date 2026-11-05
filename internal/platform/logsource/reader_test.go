package logsource

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/platform/executor"
)

// fakeRunner answers queries in turn and records them.
type fakeRunner struct {
	mu      sync.Mutex
	queries []executor.JournalQuery
	answers []answer
	block   chan struct{} // closed to release blocked queries
}

type answer struct {
	out executor.JournalOutput
	err error
}

func (f *fakeRunner) Journal(ctx context.Context, q executor.JournalQuery) (executor.JournalOutput, error) {
	f.mu.Lock()
	f.queries = append(f.queries, q)
	var a answer
	if len(f.answers) > 0 {
		a, f.answers = f.answers[0], f.answers[1:]
	}
	f.mu.Unlock()
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return executor.JournalOutput{}, ctx.Err()
		}
	}
	return a.out, a.err
}

func stdout(lines ...string) answer {
	return answer{out: executor.JournalOutput{Stdout: []byte(strings.Join(lines, "\n") + "\n")}}
}

// entry is a record as journalctl writes it, at the second t of the epoch.
func entry(t int, message string) string {
	return fmt.Sprintf(`{"__REALTIME_TIMESTAMP":"%d000000","MESSAGE":%q,"_BOOT_ID":"b"}`, t, message)
}

func plain(s string) string { return s }

func texts(r Result) []string {
	out := make([]string, len(r.Lines))
	for i, l := range r.Lines {
		out[i] = l.Text
	}
	return out
}

func newReader(r Runner) *Reader { return NewReader(r, slog.New(slog.DiscardHandler)) }

func TestUnit(t *testing.T) {
	run := &fakeRunner{answers: []answer{stdout(entry(1, "one"), entry(2, "two"))}}
	got, err := newReader(run).Unit(t.Context(), Named("a.service"), 5, plain)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(texts(got), []string{"one", "two"}) || got.Truncated {
		t.Errorf("%+v", got)
	}
	if want := time.Unix(2, 0).UTC(); !got.Lines[1].Time.Equal(want) {
		t.Errorf("time %v, want %v", got.Lines[1].Time, want)
	}
	if !slices.Equal(run.queries, []executor.JournalQuery{{Unit: "a.service", Lines: 5}}) {
		t.Errorf("queries %+v", run.queries)
	}
}

// journalRecords holds a record per encoding of MESSAGE, as journalctl writes
// them on the oldest and newest systemd of the matrix: strings, byte arrays for
// what is not UTF-8, null over 4 KiB, a message that is itself JSON. The two
// differ in spacing and field order. TestRealEncodings, under tests/integration,
// checks them against a real journal.
var journalRecords = map[string]string{
	"systemd 239": `{ "__REALTIME_TIMESTAMP" : "1793867682000000", "MESSAGE" : "first line" }
{ "__REALTIME_TIMESTAMP" : "1793867682000001", "MESSAGE" : [ 98, 97, 100, 255, 254, 98, 121, 116, 101, 115 ] }
{ "__REALTIME_TIMESTAMP" : "1793867682000002", "MESSAGE" : [ 101, 115, 99, 27, 91, 50, 74, 97, 112, 101 ] }
{ "__REALTIME_TIMESTAMP" : "1793867682000003", "MESSAGE" : "{\"MESSAGE\":\"nested\"}" }
{ "__REALTIME_TIMESTAMP" : "1793867682000004", "MESSAGE" : null }
{ "__REALTIME_TIMESTAMP" : "1793867682000005", "MESSAGE" : "last line" }
`,
	"systemd 259": `{"MESSAGE":"first line","__REALTIME_TIMESTAMP":"1793867682000000"}
{"__REALTIME_TIMESTAMP":"1793867682000001","MESSAGE":[98,97,100,255,254,98,121,116,101,115]}
{"MESSAGE":[101,115,99,27,91,50,74,97,112,101],"__REALTIME_TIMESTAMP":"1793867682000002"}
{"__REALTIME_TIMESTAMP":"1793867682000003","MESSAGE":"{\"MESSAGE\":\"nested\"}"}
{"MESSAGE":null,"__REALTIME_TIMESTAMP":"1793867682000004"}
{"__REALTIME_TIMESTAMP":"1793867682000005","MESSAGE":"last line"}
`,
}

func TestUnitJournalEncodings(t *testing.T) {
	for version, data := range journalRecords {
		t.Run(version, func(t *testing.T) {
			run := &fakeRunner{answers: []answer{{out: executor.JournalOutput{Stdout: []byte(data)}}}}
			got, err := newReader(run).Unit(t.Context(), Named("a.service"), 10, plain)
			if err != nil {
				t.Fatal(err)
			}
			want := []string{"first line", "bad\uFFFDbytes", "esc\x1b[2Jape", `{"MESSAGE":"nested"}`, tooLong, "last line"}
			if !slices.Equal(texts(got), want) {
				t.Errorf("%q", texts(got))
			}
			if got.Truncated || got.Lines[0].Time.Year() != 2026 {
				t.Errorf("truncated %v, time %v", got.Truncated, got.Lines[0].Time)
			}
		})
	}
}

func TestUnitFailures(t *testing.T) {
	noEntries := executor.JournalOutput{}
	denied := executor.JournalOutput{Exit: executor.Exit{Code: 1}, Stderr: []byte("No journal files were opened due to insufficient permissions.\n")}
	tests := []struct {
		name    string
		answers []answer
		want    []string
		wantErr error
		queries int
	}{
		{"cannot run", []answer{{err: fmt.Errorf("%w: no binary", executor.ErrJournalUnavailable)}}, nil, ErrUnavailable, 1},
		// Measured on systemd 239 and 259: total denial exits 1.
		{"permission denied is an error, never an empty log", []answer{{out: denied}}, nil, ErrUnavailable, 1},
		{"killed", []answer{{out: executor.JournalOutput{Exit: executor.Exit{Code: -1, Signal: "SIGSEGV"}}}}, nil, ErrUnavailable, 1},
		// Measured: no journal files exits 0 with empty output, so empty
		// proves nothing until the probe finds something readable.
		{"empty, and the probe finds nothing either", []answer{{out: noEntries}, {out: noEntries}}, nil, ErrUnavailable, 2},
		{"empty, and the probe fails", []answer{{out: noEntries}, {out: denied}}, nil, ErrUnavailable, 2},
		{"empty, and the probe cannot run", []answer{{out: noEntries}, {err: executor.ErrJournalUnavailable}}, nil, ErrUnavailable, 2},
		{"empty, and the journal is readable: no lines", []answer{{out: noEntries}, stdout(entry(1, "other"))}, []string{}, nil, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run := &fakeRunner{answers: tt.answers}
			got, err := newReader(run).Unit(t.Context(), Named("a.service"), 5, plain)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if len(run.queries) != tt.queries {
				t.Errorf("%d queries, want %d", len(run.queries), tt.queries)
			}
			if tt.wantErr == nil && (!slices.Equal(texts(got), tt.want) || got.Lines == nil) {
				t.Errorf("%+v", got)
			}
			if tt.queries == 2 && run.queries[1] != (executor.JournalQuery{Lines: 1}) {
				t.Errorf("probe %+v", run.queries[1])
			}
		})
	}
	t.Run("a refused query stays a refused query", func(t *testing.T) {
		refused := errors.New("journal query: not a unit name")
		run := &fakeRunner{answers: []answer{{err: refused}}}
		_, err := newReader(run).Unit(t.Context(), Named("--grep=x"), 5, plain)
		if !errors.Is(err, refused) || errors.Is(err, ErrUnavailable) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("no runner", func(t *testing.T) {
		_, err := NewReader(nil, slog.New(slog.DiscardHandler)).Unit(t.Context(), Named("a.service"), 5, plain)
		if !errors.Is(err, ErrUnavailable) {
			t.Errorf("err = %v", err)
		}
	})
}

// D-099: journalctl warns about one truncated file and reads the others
// (measured on 239 and 259). The lines are what an operator needs then.
func TestUnitWarningWithRecords(t *testing.T) {
	var logged bytes.Buffer
	a := stdout(entry(1, "one"))
	a.out.Stderr = []byte("Journal file /run/log/journal/x/bad.journal is truncated, ignoring file.\n")
	r := NewReader(&fakeRunner{answers: []answer{a}}, slog.New(slog.NewTextHandler(&logged, nil)))
	got, err := r.Unit(t.Context(), Named("a.service"), 5, func(s string) string { return strings.ReplaceAll(s, "bad.journal", "[MASKED]") })
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(texts(got), []string{"one"}) || !got.Truncated {
		t.Errorf("%+v", got)
	}
	if !strings.Contains(logged.String(), "truncated, ignoring") || strings.Contains(logged.String(), "bad.journal") {
		t.Errorf("log %q", logged.String())
	}
}

func TestNilRedactorIsRefused(t *testing.T) {
	r := newReader(&fakeRunner{answers: []answer{stdout(entry(1, "secret"))}})
	if _, err := r.Unit(t.Context(), Named("a.service"), 5, nil); err == nil {
		t.Error("Unit without a redactor")
	}
	if _, err := r.File(t.Context(), "/nonexistent", 5, nil); err == nil {
		t.Error("File without a redactor")
	}
}

// The redactor sees a whole entry before it is split, or a secret holding a
// newline would never match (Q3 masks its lines too, for what is written line
// by line).
func TestRedactionPrecedesSplitAndCut(t *testing.T) {
	secret := "first\nsecond"
	redact := func(s string) string { return strings.ReplaceAll(s, secret, "[MASKED]") }
	run := &fakeRunner{answers: []answer{stdout(entry(1, "a first\nsecond b"))}}
	got, err := newReader(run).Unit(t.Context(), Named("a.service"), 5, redact)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(texts(got), []string{"a [MASKED] b"}) {
		t.Errorf("%q", texts(got))
	}

	// A secret across the line cut is masked, not half shown.
	long := strings.Repeat("x", maxLine-3) + "SECRET"
	got = shape([]record{{text: long}}, 5, func(s string) string { return strings.ReplaceAll(s, "SECRET", "[M]") })
	if strings.Contains(got.Lines[0].Text, "SEC") {
		t.Errorf("%q", got.Lines[0].Text[maxLine-10:])
	}
}

func TestShapeBounds(t *testing.T) {
	var many []record
	for i := range 100 {
		many = append(many, record{text: fmt.Sprintf("line %d", i)})
	}
	t.Run("the newest n, in order, nothing missing", func(t *testing.T) {
		got := shape(many, 3, plain)
		if !slices.Equal(texts(got), []string{"line 97", "line 98", "line 99"}) || got.Truncated {
			t.Errorf("%+v", got)
		}
	})
	t.Run("a multi-line entry counts its lines", func(t *testing.T) {
		got := shape([]record{{text: "a\nb\nc\n"}}, 2, plain)
		if !slices.Equal(texts(got), []string{"b", "c"}) {
			t.Errorf("%q", texts(got))
		}
	})
	t.Run("a long line is cut, on a rune boundary, and said", func(t *testing.T) {
		got := shape([]record{{text: "é" + strings.Repeat("é", maxLine)}}, 5, plain)
		if l := got.Lines[0].Text; len(l) > maxLine || !utf8.ValidString(l) || !got.Truncated {
			t.Errorf("%d bytes, valid %v, truncated %v", len(l), utf8.ValidString(l), got.Truncated)
		}
	})
	t.Run("the response bound keeps the newest", func(t *testing.T) {
		var big []record
		for i := range 300 {
			big = append(big, record{text: fmt.Sprintf("%04d", i) + strings.Repeat("y", 2000)})
		}
		got := shape(big, 300, plain)
		if !got.Truncated || len(got.Lines) == 0 || len(got.Lines) == 300 {
			t.Fatalf("%d lines, truncated %v", len(got.Lines), got.Truncated)
		}
		if !strings.HasPrefix(got.Lines[len(got.Lines)-1].Text, "0299") {
			t.Errorf("the newest is %q", got.Lines[len(got.Lines)-1].Text[:4])
		}
		if n := encodedSize(got); n > maxResponse {
			t.Errorf("%d encoded bytes", n)
		}
	})
	// C5: control characters take six bytes each once encoded, so bytes of
	// text say little about the response: 1000 lines of them would be 24 MiB.
	t.Run("the bound counts encoded bytes", func(t *testing.T) {
		var ctl []record
		for range 1000 {
			ctl = append(ctl, record{text: strings.Repeat("\x01", maxLine)})
		}
		got := shape(ctl, 1000, plain)
		if n := encodedSize(got); n > maxResponse || !got.Truncated {
			t.Errorf("%d encoded bytes, truncated %v", n, got.Truncated)
		}
	})
	t.Run("invalid UTF-8 is replaced before the cut", func(t *testing.T) {
		// A run of invalid bytes grows from one byte to three: the bound holds
		// on the final text.
		got := shape([]record{{text: strings.Repeat("\xffa", maxLine/2)}}, 5, plain)
		if l := got.Lines[0].Text; len(l) > maxLine || !utf8.ValidString(l) {
			t.Errorf("%d bytes, valid %v", len(l), utf8.ValidString(l))
		}
	})
	t.Run("no lines is not nil", func(t *testing.T) {
		if got := shape(nil, 5, plain); got.Lines == nil || got.Truncated {
			t.Errorf("%+v", got)
		}
	})
}

// encodedSize is what shape's bound is about: the size of the lines' JSON.
func encodedSize(r Result) int {
	n := 0
	for _, l := range r.Lines {
		q, _ := jsontext.AppendQuote(nil, l.Text)
		n += len(q) + lineOverhead
	}
	return n
}

func TestParseJournal(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		cut     bool
		want    []string
		skipped int
	}{
		{"strings", entry(1, "a") + "\n" + entry(2, "b") + "\n", false, []string{"a", "b"}, 0},
		{"over 4 KiB", `{"MESSAGE":null}` + "\n", false, []string{tooLong}, 0},
		{"bytes", `{"MESSAGE":[104,105]}` + "\n", false, []string{"hi"}, 0},
		{"a repeated field: the first", `{"MESSAGE":["one","two"]}` + "\n", false, []string{"one"}, 0},
		{"a repeated field of bytes", `{"MESSAGE":[[104,105],"two"]}` + "\n", false, []string{"hi"}, 0},
		{"a repeated field with null first", `{"MESSAGE":[null,"two"]}` + "\n", false, []string{tooLong}, 0},
		{"no message: nothing to show", `{"_BOOT_ID":"b"}` + "\n", false, nil, 0},
		{"nested JSON is content", `{"MESSAGE":"{\"MESSAGE\":\"x\"}"}` + "\n", false, []string{`{"MESSAGE":"x"}`}, 0},
		{"not JSON", "-- No entries --\n", false, nil, 1},
		{"a JSON value that is no object", "[1,2]\n", false, nil, 1},
		{"an impossible byte", `{"MESSAGE":[300]}` + "\n", false, nil, 1},
		{"a last line without its end", entry(1, "a") + "\n" + `{"MESSAGE":"b`, false, []string{"a"}, 1},
		{"after a cut the first line is a fragment", `SAGE":"partial"}` + "\n" + entry(1, "a") + "\n", true, []string{"a"}, 0},
		{"over the record bound", `{"MESSAGE":"` + strings.Repeat("z", maxRecord) + `"}` + "\n" + entry(1, "a") + "\n", false, []string{"a"}, 1},
		{"blank lines", "\n\n", false, nil, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _, skipped := parseJournal([]byte(tt.in), tt.cut)
			var msgs []string
			for _, r := range got {
				msgs = append(msgs, r.text)
			}
			if !slices.Equal(msgs, tt.want) || skipped != tt.skipped {
				t.Errorf("%q skipped %d, want %q skipped %d", msgs, skipped, tt.want, tt.skipped)
			}
		})
	}
}

func FuzzParseJournal(f *testing.F) {
	for _, data := range journalRecords {
		f.Add([]byte(data), false, 5)
	}
	f.Add([]byte(`{"MESSAGE":[[1,2],null,"x"]}`+"\n"), true, 1)
	f.Add([]byte("\xff\xfe\n{"), false, 1000)
	f.Fuzz(func(t *testing.T, in []byte, cut bool, n int) {
		if n < 1 || n > 1000 || len(in) > 2<<20 {
			return
		}
		records, _, _ := parseJournal(in, cut)
		checkShaped(t, shape(records, n, plain), n)
	})
}

func FuzzTail(f *testing.F) {
	f.Add([]byte("one\ntwo\n"), false, 5)
	f.Add([]byte("frag\nment\nline"), true, 1)
	f.Add([]byte("\xff\x00\x1b]0;x\x07\r\n\n"), true, 1000)
	f.Fuzz(func(t *testing.T, in []byte, cut bool, n int) {
		if n < 1 || n > 1000 || len(in) > 2<<20 {
			return
		}
		checkShaped(t, shapeFile(in, cut, n, plain), n)
	})
}

func checkShaped(t *testing.T, r Result, n int) {
	t.Helper()
	if r.Lines == nil || len(r.Lines) > n {
		t.Fatalf("%d lines, asked %d", len(r.Lines), n)
	}
	if size := encodedSize(r); size > maxResponse {
		t.Fatalf("%d encoded bytes", size)
	}
	for _, l := range r.Lines {
		if len(l.Text) > maxLine || !utf8.ValidString(l.Text) || strings.Contains(l.Text, "\n") {
			t.Fatalf("line %q", l.Text)
		}
	}
}

// A million short lines must not cost a million Lines when one was asked for
// (review 3c-5a R1): a file's lines are the program's to choose.
func TestShapeCostFollowsTheAnswer(t *testing.T) {
	data := bytes.Repeat([]byte("\n"), maxFileTail)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	got := shapeFile(data, false, 1, plain)
	runtime.ReadMemStats(&after)
	if len(got.Lines) != 1 {
		t.Fatalf("%d lines", len(got.Lines))
	}
	if used := after.TotalAlloc - before.TotalAlloc; used > 8<<20 {
		t.Errorf("allocated %d bytes for one line", used)
	}
}

// Redaction and shaping are the costly part of a read: the slot covers them
// (review 3c-5a R2).
func TestFileSlotCoversShaping(t *testing.T) {
	r := newReader(nil)
	path := writeFile(t, "line\n")
	entered := make(chan struct{}, maxSlots)
	unblock := make(chan struct{})
	var wg sync.WaitGroup
	for range maxSlots {
		wg.Go(func() {
			_, _ = r.File(t.Context(), path, 1, func(s string) string {
				entered <- struct{}{}
				<-unblock
				return s
			})
		})
	}
	for range maxSlots {
		<-entered
	}
	_, err := r.File(t.Context(), path, 1, plain)
	close(unblock)
	wg.Wait()
	if !errors.Is(err, ErrBusy) {
		t.Errorf("a fifth read: err = %v", err)
	}
}

// Finding a unit's name over D-Bus is part of a read: the bound covers it, and
// a fifth request asks systemd nothing (review 3c-5a, risk R2).
func TestUnitResolveHoldsASlot(t *testing.T) {
	r := newReader(&fakeRunner{answers: []answer{stdout(entry(1, "a"))}})
	entered := make(chan struct{}, maxSlots+1)
	unblock := make(chan struct{})
	resolve := func(context.Context) (string, error) {
		entered <- struct{}{}
		<-unblock
		return "a.service", nil
	}
	var wg sync.WaitGroup
	for range maxSlots {
		wg.Go(func() { _, _ = r.Unit(t.Context(), resolve, 1, plain) })
	}
	for range maxSlots {
		<-entered
	}
	_, err := r.Unit(t.Context(), func(context.Context) (string, error) {
		t.Error("a fifth request resolved its unit")
		return "", nil
	}, 1, plain)
	close(unblock)
	wg.Wait()
	if !errors.Is(err, ErrBusy) {
		t.Errorf("a fifth read: err = %v", err)
	}
}

func TestUnitResolveFails(t *testing.T) {
	run := &fakeRunner{}
	_, err := newReader(run).Unit(t.Context(), func(context.Context) (string, error) {
		return "", errors.New("no bus")
	}, 5, plain)
	if !errors.Is(err, ErrUnavailable) || len(run.queries) != 0 {
		t.Errorf("err = %v, queries %+v", err, run.queries)
	}
}

// The newest entry of a healthy journal may have no MESSAGE: it still proves
// the journal readable (review 3c-5a R3, R4).
func TestProbeAcceptsAnEntryWithoutAMessage(t *testing.T) {
	run := &fakeRunner{answers: []answer{{}, stdout(`{"__REALTIME_TIMESTAMP":"1000000","_BOOT_ID":"b"}`)}}
	got, err := newReader(run).Unit(t.Context(), Named("a.service"), 5, plain)
	if err != nil || got.Lines == nil || len(got.Lines) != 0 {
		t.Errorf("%+v, %v", got, err)
	}
}

func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "worker.log")
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestFile(t *testing.T) {
	r := newReader(nil) // a file needs no journal: Alpine (criterion 5)

	t.Run("the tail", func(t *testing.T) {
		got, err := r.File(t.Context(), writeFile(t, "one\ntwo\nthree\n"), 2, plain)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(texts(got), []string{"two", "three"}) || got.Truncated || !got.Lines[0].Time.IsZero() {
			t.Errorf("%+v", got)
		}
	})
	t.Run("a last line without its end", func(t *testing.T) {
		got, err := r.File(t.Context(), writeFile(t, "one\ntwo"), 5, plain)
		if err != nil || !slices.Equal(texts(got), []string{"one", "two"}) {
			t.Errorf("%q, %v", texts(got), err)
		}
	})
	t.Run("redacted", func(t *testing.T) {
		got, err := r.File(t.Context(), writeFile(t, "a TOKEN b\n"), 5, func(s string) string { return strings.ReplaceAll(s, "TOKEN", "[M]") })
		if err != nil || !slices.Equal(texts(got), []string{"a [M] b"}) {
			t.Errorf("%q, %v", texts(got), err)
		}
	})
	t.Run("a missing file has no lines", func(t *testing.T) {
		got, err := r.File(t.Context(), filepath.Join(t.TempDir(), "absent"), 5, plain)
		if err != nil || got.Lines == nil || len(got.Lines) != 0 || got.Truncated {
			t.Errorf("%+v, %v", got, err)
		}
	})
	t.Run("a missing directory has no lines", func(t *testing.T) {
		got, err := r.File(t.Context(), filepath.Join(t.TempDir(), "absent", "log"), 5, plain)
		if err != nil || len(got.Lines) != 0 {
			t.Errorf("%+v, %v", got, err)
		}
	})
	t.Run("an unsafe file is refused", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "target")
		if err := os.WriteFile(target, []byte("private\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "link")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		got, err := r.File(t.Context(), link, 5, plain)
		if !errors.Is(err, ErrUnsafeFile) || len(got.Lines) != 0 {
			t.Errorf("%+v, %v", got, err)
		}
	})
	t.Run("over the read bound: the newest, and a fragment is dropped", func(t *testing.T) {
		var b strings.Builder
		for i := 0; b.Len() < 2*maxFileTail; i++ {
			fmt.Fprintf(&b, "line %07d\n", i)
		}
		content := b.String()
		got, err := r.File(t.Context(), writeFile(t, content), 1000, plain)
		if err != nil {
			t.Fatal(err)
		}
		lastLine := content[strings.LastIndex(strings.TrimSuffix(content, "\n"), "\n")+1:]
		if got.Lines[len(got.Lines)-1].Text != strings.TrimSuffix(lastLine, "\n") || len(got.Lines) != 1000 || got.Truncated {
			t.Errorf("%d lines, truncated %v", len(got.Lines), got.Truncated)
		}
		// The read holds fewer lines than asked for: some may be missing.
		longLines := strings.Repeat(strings.Repeat("z", 3000)+"\n", maxFileTail/3000*2)
		got, err = r.File(t.Context(), writeFile(t, longLines), 1000, plain)
		if err != nil || !got.Truncated || got.Lines[0].Text != strings.Repeat("z", 3000) {
			t.Errorf("%d lines, truncated %v, %v", len(got.Lines), got.Truncated, err)
		}
	})
	t.Run("no newline in the whole read: nothing whole to show", func(t *testing.T) {
		got, err := r.File(t.Context(), writeFile(t, strings.Repeat("x", 2*maxFileTail)), 5, plain)
		if err != nil || len(got.Lines) != 0 || !got.Truncated {
			t.Errorf("%d lines, truncated %v, %v", len(got.Lines), got.Truncated, err)
		}
	})
}

// A read Go cannot interrupt must not hold the command, nor shutdown, which
// waits for commands (Daemon.stop); its slot stays taken until it returns, so
// at most slots such reads exist (D-099).
func TestFileStuck(t *testing.T) {
	release := make(chan struct{})
	r := newReader(nil)
	entered := make(chan struct{}, maxSlots)
	r.open = func(string) (*os.File, error) {
		entered <- struct{}{}
		<-release
		return nil, fs.ErrNotExist
	}
	for range maxSlots {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
		_, err := r.File(ctx, "/stuck", 5, plain)
		cancel()
		if !errors.Is(err, ErrUnavailable) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v", err)
		}
	}
	if _, err := r.File(t.Context(), "/stuck", 5, plain); !errors.Is(err, ErrBusy) {
		t.Errorf("with every slot stuck: err = %v", err)
	}
	close(release)
	for range maxSlots {
		<-entered
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := r.File(t.Context(), "/stuck", 5, plain)
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("slots were not released: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestSlots(t *testing.T) {
	run := &fakeRunner{block: make(chan struct{}), answers: []answer{stdout(entry(1, "a")), stdout(entry(1, "a")), stdout(entry(1, "a")), stdout(entry(1, "a")), stdout(entry(1, "a"))}}
	r := newReader(run)
	var wg sync.WaitGroup
	errs := make(chan error, maxSlots)
	for range maxSlots {
		wg.Go(func() {
			_, err := r.Unit(t.Context(), Named("a.service"), 5, plain)
			errs <- err
		})
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		run.mu.Lock()
		n := len(run.queries)
		run.mu.Unlock()
		if n == maxSlots {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d queries started", n)
		}
	}
	if _, err := r.Unit(t.Context(), Named("a.service"), 5, plain); !errors.Is(err, ErrBusy) {
		t.Errorf("a fifth read: err = %v", err)
	}
	if _, err := r.File(t.Context(), "/x", 5, plain); !errors.Is(err, ErrBusy) {
		t.Errorf("a fifth read of a file: err = %v", err)
	}
	close(run.block)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	if _, err := r.Unit(t.Context(), Named("a.service"), 5, plain); err != nil {
		t.Errorf("a slot was not released: %v", err)
	}
}
