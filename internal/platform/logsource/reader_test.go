package logsource

import (
	"bytes"
	"context"
	"encoding/json/v2"
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
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/api"
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

// sourceRecords hold a source query's answer as measured on systemd 239 and
// 259 (2026-11-10): a byte array, a run id, a multi-line message.
var sourceRecords = map[string]string{
	"systemd 239": `{ "__CURSOR" : "s=c;i=229", "__REALTIME_TIMESTAMP" : "1794330763025142", "__MONOTONIC_TIMESTAMP" : "1725025885", "_BOOT_ID" : "b", "MESSAGE" : [ 99, 116, 108, 1, 32, 98, 97, 100, 255, 32, 101, 110, 100 ] }
{ "__CURSOR" : "s=c;i=22a", "__REALTIME_TIMESTAMP" : "1794330763025644", "__MONOTONIC_TIMESTAMP" : "1725026387", "_BOOT_ID" : "b", "MESSAGE" : "job line", "SENTINEL_RUN_ID" : "0123456789abcdef0123456789abcdef" }
{ "__CURSOR" : "s=c;i=22b", "__REALTIME_TIMESTAMP" : "1794330763026229", "__MONOTONIC_TIMESTAMP" : "1725026972", "_BOOT_ID" : "b", "MESSAGE" : "multi\nline" }
`,
	"systemd 259": `{"__REALTIME_TIMESTAMP":"1794330766351683","__MONOTONIC_TIMESTAMP":"1693140830","__CURSOR":"s=c;i=657","__SEQNUM":"1623","MESSAGE":[99,116,108,1,32,98,97,100,255,32,101,110,100],"__SEQNUM_ID":"6","_BOOT_ID":"a"}
{"__SEQNUM":"1624","__CURSOR":"s=c;i=658","SENTINEL_RUN_ID":"0123456789abcdef0123456789abcdef","_BOOT_ID":"a","__REALTIME_TIMESTAMP":"1794330766352912","__MONOTONIC_TIMESTAMP":"1693142059","__SEQNUM_ID":"6","MESSAGE":"job line"}
{"__SEQNUM_ID":"6","__SEQNUM":"1625","__CURSOR":"s=c;i=659","_BOOT_ID":"a","__MONOTONIC_TIMESTAMP":"1693143199","__REALTIME_TIMESTAMP":"1794330766354052","MESSAGE":"multi\nline"}
`,
}

func TestSourceJournalEncodings(t *testing.T) {
	for version, data := range sourceRecords {
		t.Run(version, func(t *testing.T) {
			run := &fakeRunner{answers: []answer{{out: executor.JournalOutput{Stdout: []byte(data)}}}}
			got, err := newReader(run).Source(t.Context(), "web", 10, plain)
			if err != nil {
				t.Fatal(err)
			}
			wantText := []string{"ctl\x01 bad\uFFFD end", "job line", "multi", "line"}
			wantRun := []string{"", runA, "", ""}
			for i, l := range got.Lines {
				if i >= len(wantText) || l.Text != wantText[i] || l.RunID != wantRun[i] {
					t.Errorf("line %d %+v", i, l)
				}
			}
			if len(got.Lines) != len(wantText) || got.Truncated {
				t.Errorf("%d lines, truncated %v", len(got.Lines), got.Truncated)
			}
		})
	}
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

// sourceEntry is a record of a program's output, as a source query returns it.
func sourceEntry(t int, message, runID string) string {
	return fmt.Sprintf(`{"__REALTIME_TIMESTAMP":"%d000000","MESSAGE":%q,"SENTINEL_RUN_ID":%q}`, t, message, runID)
}

const runA, runB = "0123456789abcdef0123456789abcdef", "fedcba9876543210fedcba9876543210"

func TestSource(t *testing.T) {
	run := &fakeRunner{answers: []answer{stdout(
		sourceEntry(1, "one", runA),
		sourceEntry(2, "two", runB),
		entry(3, "a service's line"),
	)}}
	got, err := newReader(run).Source(t.Context(), "web", 5, plain)
	if err != nil {
		t.Fatal(err)
	}
	want := []Line{
		{Time: time.Unix(1, 0).UTC(), Text: "one", RunID: runA},
		{Time: time.Unix(2, 0).UTC(), Text: "two", RunID: runB},
		{Time: time.Unix(3, 0).UTC(), Text: "a service's line"},
	}
	if !slices.Equal(got.Lines, want) || got.Truncated {
		t.Errorf("%+v", got)
	}
	if !slices.Equal(run.queries, []executor.JournalQuery{{Source: "web", Lines: 5}}) {
		t.Errorf("queries %+v", run.queries)
	}
}

// A source read is a journal read: an empty answer still needs the probe, and
// every failure is ErrUnavailable.
func TestSourceEmptyAndFailures(t *testing.T) {
	t.Run("empty and the probe finds an entry", func(t *testing.T) {
		run := &fakeRunner{answers: []answer{{}, stdout(entry(1, "x"))}}
		got, err := newReader(run).Source(t.Context(), "web", 5, plain)
		if err != nil || len(got.Lines) != 0 {
			t.Fatalf("%+v, %v", got, err)
		}
	})
	t.Run("empty and the probe finds nothing", func(t *testing.T) {
		run := &fakeRunner{answers: []answer{{}, {}}}
		if _, err := newReader(run).Source(t.Context(), "web", 5, plain); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("error %v, want ErrUnavailable", err)
		}
	})
	t.Run("an exit status", func(t *testing.T) {
		run := &fakeRunner{answers: []answer{{out: executor.JournalOutput{Exit: executor.Exit{Code: 1}}}}}
		if _, err := newReader(run).Source(t.Context(), "web", 5, plain); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("error %v, want ErrUnavailable", err)
		}
	})
	t.Run("no runner", func(t *testing.T) {
		if _, err := newReader(nil).Source(t.Context(), "web", 5, plain); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("error %v, want ErrUnavailable", err)
		}
	})
	t.Run("no redactor", func(t *testing.T) {
		if _, err := newReader(&fakeRunner{}).Source(t.Context(), "web", 5, nil); err == nil {
			t.Fatal("no error")
		}
	})
}

// Only a run id sentineld could have written is kept: 32 lower-case hex
// digits, not masked by redaction. Anything else is dropped, never the line.
func TestSourceRunIDs(t *testing.T) {
	const secret = "00000000000000000000000000000000"
	mask := func(s string) string { return strings.ReplaceAll(s, secret, "[redacted]") }
	tests := []struct {
		name, field string // field: the raw JSON of SENTINEL_RUN_ID
		want        string
	}{
		{"valid", `"` + runA + `"`, runA},
		{"upper case", `"` + strings.ToUpper(runA) + `"`, ""},
		{"too short", `"` + runA[1:] + `"`, ""},
		{"too long", `"` + strings.Repeat("a", 4096) + `"`, ""},
		{"not hex", `"0123456789abcdef0123456789abcdeg"`, ""},
		{"a number", `12`, ""},
		{"bytes", `[48,49]`, ""},
		{"repeated", `["` + runA + `","` + runB + `"]`, ""},
		{"null", `null`, ""},
		{"a configured secret", `"` + secret + `"`, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := `{"__REALTIME_TIMESTAMP":"1000000","MESSAGE":"line","SENTINEL_RUN_ID":` + tc.field + `}`
			run := &fakeRunner{answers: []answer{stdout(rec)}}
			got, err := newReader(run).Source(t.Context(), "job", 5, mask)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Lines) != 1 || got.Lines[0].Text != "line" || got.Lines[0].RunID != tc.want {
				t.Errorf("%+v, want run id %q", got.Lines, tc.want)
			}
		})
	}
}

// A unit's entries are written by whatever the unit runs: a run id there is
// anyone's to forge, so a unit read never returns one (D-100).
func TestUnitIgnoresRunIDs(t *testing.T) {
	run := &fakeRunner{answers: []answer{stdout(sourceEntry(1, "one", runA))}}
	got, err := newReader(run).Unit(t.Context(), Named("a.service"), 5, plain)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Lines) != 1 || got.Lines[0].RunID != "" {
		t.Errorf("%+v", got.Lines)
	}
}

// Run ids count in the response bound: 1000 lines each with one, near the
// bound in text, stay within it once encoded.
func TestShapeCountsRunIDs(t *testing.T) {
	var recs []record
	for range 1000 {
		recs = append(recs, record{text: strings.Repeat("y", 250), runID: runA})
	}
	got := shape(recs, 1000, plain)
	if n := encodedSize(got); n > maxResponse || !got.Truncated {
		t.Errorf("%d encoded bytes, truncated %v", n, got.Truncated)
	}
	if got.Lines[len(got.Lines)-1].RunID != runA {
		t.Errorf("the run id was lost: %+v", got.Lines[len(got.Lines)-1])
	}
}

// Review 3c-5b R1 (code-review): the bound holds for the whole answer, the
// array's brackets and the result's other fields included, at its edge.
func TestShapeBoundsTheWholeAnswer(t *testing.T) {
	at := time.Unix(1, 0).UTC()
	var recs []record
	for range 779 {
		recs = append(recs, record{time: at, text: strings.Repeat("y", 250), runID: runA})
	}
	recs = append(recs, record{time: at, text: strings.Repeat("y", 314), runID: runA})
	got := shape(recs, 1000, plain)
	lines := make([]api.LogLine, len(got.Lines))
	for i, l := range got.Lines {
		lines[i] = api.LogLine{Time: l.Time, Text: l.Text, RunID: l.RunID}
	}
	answer := api.SupervisorLogsResult{Name: strings.Repeat("n", 63), Source: api.LogSourceJournal, Lines: lines, Truncated: true}
	b, err := json.Marshal(answer)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > maxResponse {
		t.Errorf("the answer takes %d bytes, over %d", len(b), maxResponse)
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

// encodedSize is what shape's bound is about: the size of the lines as the
// answer encodes them, measured by encoding them, not by shape's own sum.
func encodedSize(r Result) int {
	lines := make([]api.LogLine, len(r.Lines))
	for i, l := range r.Lines {
		lines[i] = api.LogLine{Time: l.Time, Text: l.Text, RunID: l.RunID}
	}
	b, err := json.Marshal(lines)
	if err != nil {
		panic(err)
	}
	return len(b)
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
			got, _, skipped := parseJournal([]byte(tt.in), tt.cut, false)
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
	for _, data := range sourceRecords {
		f.Add([]byte(data), false, 5)
	}
	f.Fuzz(func(t *testing.T, in []byte, cut bool, n int) {
		if n < 1 || n > 1000 || len(in) > 2<<20 {
			return
		}
		records, _, _ := parseJournal(in, cut, true)
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
