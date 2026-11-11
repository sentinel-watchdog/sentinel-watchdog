package supervisor

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"
	"unicode/utf8"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/clock"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/module"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/platform/executor"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/platform/journald"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/api"
)

// fakeJournal records the entries a sink sends, decoded, and answers each
// send with the next of errs, then with err (nil: accepted).
type fakeJournal struct {
	mu      sync.Mutex
	entries []map[string]string
	errs    []error
	err     error
	calls   int
	times   []time.Time // of each send
}

func (j *fakeJournal) send(entry []byte) error {
	fields, err := decodeEntry(entry)
	if err != nil {
		panic(err) // a test bug or an encoding bug: either way, loud
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.calls++
	j.times = append(j.times, time.Now())
	answer := j.err
	if len(j.errs) > 0 {
		answer, j.errs = j.errs[0], j.errs[1:]
	}
	if answer != nil {
		return answer
	}
	j.entries = append(j.entries, fields)
	return nil
}

// span is the time from the first send to the last.
func (j *fakeJournal) span() time.Duration {
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(j.times) == 0 {
		return 0
	}
	return j.times[len(j.times)-1].Sub(j.times[0])
}

func (j *fakeJournal) sends() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.calls
}

// errFull is what journald.Send returns for a full socket.
var errFull = fmt.Errorf("%w: %w", journald.ErrFull, syscall.EAGAIN)

// fullTimes returns n errFull answers.
func fullTimes(n int) []error {
	errs := make([]error, n)
	for i := range errs {
		errs[i] = errFull
	}
	return errs
}

// runSink starts a service with one launch whose output is lines, sent
// through j, and waits until the sinks closed.
func runSink(t *testing.T, j *fakeJournal, lines ...string) *rig {
	t.Helper()
	l := &fakeLauncher{output: lines}
	r := start(t, configureWithJournal(t, map[string]string{"supervisor/a.yaml": loggedService}, l, j.send), "")
	// The fake launcher writes the output under its lock, and a sink retrying
	// a full journal waits on a timer: sleeping lets the bubble's time pass.
	time.Sleep(time.Second)
	l.proc(0).end(executor.Exit{}, nil)
	synctest.Wait()
	return r
}

// Review 3c-5b R1: a full socket is retried every millisecond for at most
// 20 ms, so a burst journald is still draining loses nothing (D-100, refined).
func TestOutputRetriesAFullJournal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		j := &fakeJournal{errs: fullTimes(5)}
		r := runSink(t, j, "one")
		defer r.stop(t)
		if sent := j.sent(); len(sent) != 1 || sent[0]["MESSAGE"] != "one" {
			t.Fatalf("entries %q", sent)
		}
		if waited := j.span(); waited < 5*journalRetryEvery {
			t.Errorf("waited %v, want at least the 5 retries", waited)
		}
		if st := statusResult(t, r.in); st.OutputDropped != 0 {
			t.Errorf("output_dropped = %d", st.OutputDropped)
		}
	})
}

// After 20 ms the line is dropped and counted, and the stream stops waiting:
// later lines try once, so a stuck journald slows no program.
func TestOutputStopsWaitingOnAStuckJournal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		j := &fakeJournal{err: errFull}
		r := runSink(t, j, "one", "two", "three")
		defer r.stop(t)
		if waited := j.span(); waited < journalRetryFor || waited >= 2*journalRetryFor {
			t.Errorf("waited %v, want one retry bound of %v", waited, journalRetryFor)
		}
		if n, want := j.sends(), 1+int(journalRetryFor/journalRetryEvery)+2; n != want {
			t.Errorf("%d sends, want %d", n, want)
		}
		if st := statusResult(t, r.in); st.OutputDropped != 3 {
			t.Errorf("output_dropped = %d, want 3", st.OutputDropped)
		}
	})
}

// Once journald takes a line of the stream again, a full socket is waited
// for again.
func TestOutputWaitsAgainAfterTheJournalRecovers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// one: dropped after the bound; two: taken; three: taken at the second try.
		errs := append(fullTimes(1+int(journalRetryFor/journalRetryEvery)), nil, errFull)
		j := &fakeJournal{errs: errs}
		r := runSink(t, j, "one", "two", "three")
		defer r.stop(t)
		var got []string
		for _, e := range j.sent() {
			got = append(got, e["MESSAGE"])
		}
		if !slices.Equal(got, []string{"two", "three"}) {
			t.Errorf("messages %q", got)
		}
		if st := statusResult(t, r.in); st.OutputDropped != 1 {
			t.Errorf("output_dropped = %d, want 1", st.OutputDropped)
		}
	})
}

// Review 3c-5b R3: the fallback warning is given again once a send has worked
// in between, so a second outage is not silent.
func TestOutputFallbackWarnsAgainAfterARecovery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		missing := fmt.Errorf("journald: sendto: %w", syscall.ENOENT)
		j := &fakeJournal{errs: []error{missing, missing, nil, missing}}
		r := runSink(t, j, "one", "two", "three", "four")
		defer r.stop(t)
		if c := strings.Count(r.logs.String(), fallbackWarning); c != 2 {
			t.Errorf("%d warnings, want 2:\n%s", c, r.logs.String())
		}
	})
}

// Review 3c-5b R2: journalctl's JSON prints a MESSAGE of 4088 bytes or more as
// null (systemd 259; 4096 on 239), so a line sent is never longer than 4087
// bytes, also when redaction lengthens it, and is cut on a rune boundary.
func TestOutputMessageFitsJournalctl(t *testing.T) {
	tests := []struct {
		name, line string
		cut        bool
	}{
		{"short", "hello", false},
		{"at the bound", strings.Repeat("x", maxMessage), false},
		{"over the bound", strings.Repeat("x", maxMessage+1), true},
		{"cut by the line writer", strings.Repeat("x", 5000), true},
		{"lengthened by redaction", strings.Repeat("x", 4000) + strings.Repeat(" abcd", 20), true},
		{"multi-byte, cut inside a rune", strings.Repeat("€", 1500), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				j := &fakeJournal{}
				l := &fakeLauncher{output: []string{tc.line}}
				body := "version: 1\nservices:\n  - name: web\n    command: /bin/sh\n    environment: {API_KEY: \"abcd\"}\n"
				r := start(t, configureWithJournal(t, map[string]string{"supervisor/a.yaml": body}, l, j.send), "")
				defer r.stop(t)
				synctest.Wait()
				sent := j.sent()
				if len(sent) != 1 {
					t.Fatalf("%d entries", len(sent))
				}
				msg := sent[0]["MESSAGE"]
				if len(msg) > maxMessage || !utf8.ValidString(msg) || strings.Contains(msg, "abcd") {
					t.Errorf("%d bytes, valid %v", len(msg), utf8.ValidString(msg))
				}
				if cut := strings.HasSuffix(msg, lineCut); cut != tc.cut || strings.Count(msg, lineCut) > 1 {
					t.Errorf("cut %v, want %v: ...%q", cut, tc.cut, msg[max(0, len(msg)-30):])
				}
			})
		})
	}
}

// Review 3c-5b R7: a service's stdout and stderr sinks run at once and share
// the counter and the warning; under -race they warn once and count every line.
func TestOutputSinksShareTheirStateSafely(t *testing.T) {
	var logs syncBuffer
	in := &instance{log: slog.New(slog.NewTextHandler(&logs, nil)), rt: module.Runtime{Clock: clock.Real()}}
	in.journalSend = func([]byte) error { return fmt.Errorf("journald: sendto: %w", syscall.ECONNREFUSED) }
	var wg sync.WaitGroup
	for _, stream := range []string{"stdout", "stderr"} {
		sink := &outputSink{in: in, kind: "service", name: "web", stream: stream, redact: func(s string) string { return s }}
		wg.Go(func() {
			for i := range 200 {
				sink.line(fmt.Sprintf("line %d", i))
			}
		})
	}
	wg.Wait()
	if c := strings.Count(logs.String(), fallbackWarning); c != 1 {
		t.Errorf("%d warnings, want 1", c)
	}
	if c := strings.Count(logs.String(), "service output"); c != 400 {
		t.Errorf("%d lines in the logger, want 400", c)
	}
}

func (j *fakeJournal) sent() []map[string]string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return slices.Clone(j.entries)
}

// decodeEntry reads the binary form of journald's native protocol (NAME\n,
// little-endian length, value, \n), the only form the sink may send.
func decodeEntry(entry []byte) (map[string]string, error) {
	fields := map[string]string{}
	for len(entry) > 0 {
		nl := bytes.IndexByte(entry, '\n')
		if nl < 0 || len(entry) < nl+9 {
			return nil, fmt.Errorf("a field without its length: %q", entry)
		}
		name := string(entry[:nl])
		n := binary.LittleEndian.Uint64(entry[nl+1:])
		entry = entry[nl+9:]
		if uint64(len(entry)) < n+1 || entry[n] != '\n' {
			return nil, fmt.Errorf("field %s shorter than its length", name)
		}
		if _, dup := fields[name]; dup {
			return nil, fmt.Errorf("field %s twice", name)
		}
		fields[name] = string(entry[:n])
		entry = entry[n+1:]
	}
	return fields, nil
}

// configureWithJournal configures files with l as launcher and journal as
// the journal sender, as cmd/sentineld does under systemd.
func configureWithJournal(t *testing.T, files map[string]string, l *fakeLauncher, journal func([]byte) error) *instance {
	t.Helper()
	if _, ok := files["sentinel.yaml"]; !ok {
		files["sentinel.yaml"] = armedCentral
	}
	mc, err := moduleConfig(t, files, map[string]string{"TOKEN": "tok-9f8e7d"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(Options{NewChecker: newFakes().build, Lookup: fakeLookup,
		NewLauncher: func(clock.Clock) (launcher, error) { return l, nil },
		JournalSend: journal}).Configure(mc)
	if err != nil {
		t.Fatal(err)
	}
	return c.(*instance)
}

const loggedService = "version: 1\nservices:\n  - name: web\n    command: /bin/sh\n" +
	"    environment: {API_KEY: \"literal-secret-42\"}\n"

// Criterion 13: each line of log output is an entry of its own, with the
// fields that name its source, redacted; nothing of it goes to the logger.
func TestLogOutputToTheJournal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		j := &fakeJournal{}
		l := &fakeLauncher{output: []string{"hello", "key literal-secret-42"}, partial: "last words"}
		r := start(t, configureWithJournal(t, map[string]string{"supervisor/a.yaml": loggedService}, l, j.send), "")
		defer r.stop(t)
		synctest.Wait()
		l.proc(0).end(executor.Exit{}, nil) // the sinks close: the partial line is sent
		synctest.Wait()
		sent := j.sent()
		var messages []string
		for _, e := range sent {
			messages = append(messages, e["MESSAGE"])
			want := map[string]string{"MESSAGE": e["MESSAGE"], "PRIORITY": "6", "SYSLOG_IDENTIFIER": "sentineld/web",
				"SENTINEL_SOURCE": "web", "SENTINEL_STREAM": "stdout"}
			if !maps.Equal(e, want) {
				t.Errorf("entry %q, want %q", e, want)
			}
		}
		if want := []string{"hello", "key [REDACTED]", "last words"}; !slices.Equal(messages, want) {
			t.Errorf("messages %q, want %q first", messages, want)
		}
		if logs := r.logs.String(); strings.Contains(logs, "service output") || strings.Contains(logs, "literal-secret-42") {
			t.Errorf("logs:\n%s", logs)
		}
	})
}

// Every field name the sink sends is one journald stores from a client.
func TestOutputFieldNamesAreValid(t *testing.T) {
	for _, name := range []string{fieldMessage, fieldPriority, fieldIdentifier, fieldSource, fieldStream, fieldRunID} {
		if !journald.ValidName(name) {
			t.Errorf("%q is not a valid journal field name", name)
		}
	}
}

var runIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Criterion 13: a job's lines carry the id of the run that wrote them.
func TestJobOutputToTheJournal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		j := &fakeJournal{}
		l := &fakeLauncher{output: []string{"backup done"}}
		files := map[string]string{"sentinel.yaml": jobsCentral, "supervisor/a.yaml": minuteJob}
		r := start(t, configureWithJournal(t, files, l, j.send), "")
		defer r.stop(t)
		tick(time.Minute)
		l.proc(0).end(executor.Exit{}, nil)
		synctest.Wait()
		last := readFileState(t, r.stateDir).Jobs["tick"].Last
		if last == nil || !runIDPattern.MatchString(last.RunID) {
			t.Fatalf("last run %+v", last)
		}
		sent := j.sent()
		if len(sent) != 1 {
			t.Fatalf("entries %q", sent)
		}
		want := map[string]string{"MESSAGE": "backup done", "PRIORITY": "6", "SYSLOG_IDENTIFIER": "sentineld/tick",
			"SENTINEL_SOURCE": "tick", "SENTINEL_STREAM": "stdout", "SENTINEL_RUN_ID": last.RunID}
		if !maps.Equal(sent[0], want) {
			t.Errorf("entry %q, want %q", sent[0], want)
		}
	})
}

// Criterion 15: a full socket drops and counts the line; the program is not
// held and the logger gets nothing, since it writes to the same journald.
func TestOutputDroppedWhenTheJournalIsFull(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		j := &fakeJournal{err: errFull}
		l := &fakeLauncher{output: []string{"one", "two"}, partial: "three"}
		r := start(t, configureWithJournal(t, map[string]string{"supervisor/a.yaml": loggedService}, l, j.send), "")
		defer r.stop(t)
		time.Sleep(time.Second) // the first line is retried for 20 ms (runSink)
		l.proc(0).end(executor.Exit{}, nil)
		synctest.Wait()
		if logs := r.logs.String(); strings.Contains(logs, "service output") || strings.Contains(logs, fallbackWarning) {
			t.Errorf("logs:\n%s", logs)
		}
		st := statusResult(t, r.in)
		if st.OutputDropped != 3 {
			t.Errorf("output_dropped = %d, want 3", st.OutputDropped)
		}
	})
}

// Criterion 15: any other send error falls back to the logger, line by
// line, and is warned about once, not per line or per launch.
func TestOutputFallsBackToTheLogger(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		j := &fakeJournal{err: fmt.Errorf("journald: sendto: %w", syscall.ENOENT)}
		l := &fakeLauncher{output: []string{"one", "two"}}
		r := start(t, configureWithJournal(t, map[string]string{"supervisor/a.yaml": loggedService}, l, j.send), "")
		defer r.stop(t)
		synctest.Wait()
		l.proc(0).end(executor.Exit{}, nil) // a second launch after the restart
		tick(5 * time.Second)
		logs := r.logs.String()
		if c := strings.Count(logs, "service output"); c < 3 {
			t.Errorf("%d lines in the logger, want the lines of both launches:\n%s", c, logs)
		}
		if c := strings.Count(logs, fallbackWarning); c != 1 {
			t.Errorf("%d warnings, want 1:\n%s", c, logs)
		}
		if st := statusResult(t, r.in); st.OutputDropped != 0 {
			t.Errorf("output_dropped = %d", st.OutputDropped)
		}
	})
}

// Criterion 16: with no journal, lines go to the logger as before, and the
// journal is never written.
func TestLogOutputWithoutAJournal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{output: []string{"hello"}}
		r := start(t, configureWithJournal(t, map[string]string{"supervisor/a.yaml": loggedService}, l, nil), "")
		defer r.stop(t)
		synctest.Wait()
		if logs := r.logs.String(); !strings.Contains(logs, "service output") || !strings.Contains(logs, "hello") {
			t.Errorf("logs:\n%s", logs)
		}
	})
}

// statusResult reads the whole supervisor.status of in.
func statusResult(t *testing.T, in *instance) api.SupervisorStatusResult {
	t.Helper()
	res, apiErr := in.Handle(t.Context(), api.SupervisorStatus.Name, nil)
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	st, ok := res.(api.SupervisorStatusResult)
	if !ok {
		t.Fatalf("result %T", res)
	}
	return st
}
