package audit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/clock"
)

var now = time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)

func newDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// openLog opens the log in dir with files of at most maxFile bytes (0:
// the production size).
func openLog(t *testing.T, dir string, maxFile int64) *Log {
	t.Helper()
	l, err := tryOpen(dir, maxFile, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close(context.Background()) })
	return l
}

func tryOpen(dir string, maxFile int64, log *slog.Logger) (*Log, error) {
	if maxFile == 0 {
		maxFile = maxFileBytes
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	l, err := open(root, Options{Clock: clock.NewFake(now), Logger: log}, maxFile)
	if err != nil {
		_ = root.Close()
	}
	return l, err
}

// integrityAfterReopen closes l and returns what a new Open finds.
func integrityAfterReopen(t *testing.T, l *Log, dir string, maxFile int64) Integrity {
	t.Helper()
	if l != nil {
		if err := l.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	return openLog(t, dir, maxFile).Integrity()
}

func uid(v uint32) *uint32 { return &v }

func entry(action string, result Result) Entry {
	return Entry{Actor: Actor{Kind: ActorSocket, UID: uid(1000), GID: uid(1000), PID: 42},
		Action: action, Result: result, CorrelationID: "c1", Detail: map[string]string{"code": "ok"}}
}

func write(t *testing.T, l *Log, n int) []Record {
	t.Helper()
	var out []Record
	for i := range n {
		r, err := l.Record(context.Background(), entry(fmt.Sprintf("core.op_%d", i%7), ResultOK))
		if err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
		out = append(out, r)
	}
	return out
}

func checkChain(t *testing.T, recs []Record) {
	t.Helper()
	for i := 1; i < len(recs); i++ {
		if recs[i].Seq != recs[i-1].Seq+1 || recs[i].PrevHash != recs[i-1].Hash {
			t.Fatalf("records %d and %d are not chained: %+v / %+v", i-1, i, recs[i-1], recs[i])
		}
	}
}

func TestChainSurvivesRestart(t *testing.T) {
	dir := newDir(t)
	l := openLog(t, dir, 0)
	first := write(t, l, 3)
	if first[0].Seq != 1 || first[0].PrevHash != genesisHash {
		t.Errorf("first record %+v", first[0])
	}
	if got := l.Integrity(); got.Seq != 3 {
		t.Errorf("Integrity().Seq = %d after 3 records", got.Seq)
	}
	if got := integrityAfterReopen(t, l, dir, 0); got.State != StateOK || got.Seq != 3 {
		t.Fatalf("integrity after restart: %+v", got)
	}
	l = openLog(t, dir, 0)
	next := write(t, l, 1)
	if next[0].Seq != 4 || next[0].PrevHash != first[2].Hash {
		t.Errorf("record after restart %+v does not follow %+v", next[0], first[2])
	}
	tail, skipped, err := l.Tail(context.Background(), 10)
	if err != nil || skipped != 0 || len(tail) != 4 {
		t.Fatalf("Tail = %d records, %d skipped, %v", len(tail), skipped, err)
	}
	checkChain(t, tail)
	for _, name := range []string{activeFile, headFile} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.Mode().Perm() != fileMode {
			t.Errorf("%s: %v %v", name, info, err)
		}
	}
}

// The line format is part of the external contract (ADR-0011): this golden
// line must keep verifying in every later release.
func TestGoldenLine(t *testing.T) {
	const golden = `{"seq":1,"time":"2026-10-12T10:00:00Z","actor":{"kind":"socket","uid":0,"gid":0,"pid":42},` +
		`"action":"core.reload","result":"intent","correlation_id":"c1","detail":{"a":"1","b":"2"},` +
		`"prev_hash":"sha256:0000000000000000000000000000000000000000000000000000000000000000",` +
		`"hash":"sha256:6649cf0e5f32164560c05b1adae717010008a5e04327fe388e3485643c5ac58c"}`
	dir := newDir(t)
	l := openLog(t, dir, 0)
	e := Entry{Actor: Actor{Kind: ActorSocket, UID: uid(0), GID: uid(0), PID: 42}, Action: "core.reload",
		Result: ResultIntent, CorrelationID: "c1", Detail: map[string]string{"b": "2", "a": "1"}}
	if _, err := l.Record(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, activeFile))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSuffix(string(data), "\n"); got != golden {
		t.Errorf("line\n got %s\nwant %s", got, golden)
	}
	if _, err := parse([]byte(golden)); err != nil {
		t.Errorf("golden line does not parse: %v", err)
	}
}

func TestRotationKeepsTheChainAndTheBound(t *testing.T) {
	dir := newDir(t)
	const maxFile = 1000 // about four records per file
	l := openLog(t, dir, maxFile)
	recs := write(t, l, 60)
	if _, err := os.Stat(filepath.Join(dir, fileName(retainedFiles))); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("more than %d files kept: %v", retainedFiles, err)
	}
	total := int64(0)
	for i := range retainedFiles {
		info, err := os.Stat(filepath.Join(dir, fileName(i)))
		if err != nil {
			t.Fatalf("%s: %v", fileName(i), err)
		}
		if info.Size() > maxFile {
			t.Errorf("%s has %d bytes", fileName(i), info.Size())
		}
		total += info.Size()
	}
	tail, _, err := l.Tail(context.Background(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	checkChain(t, tail)
	if last := tail[len(tail)-1]; last.Seq != recs[len(recs)-1].Seq || len(tail) >= len(recs) {
		t.Errorf("tail ends at %d with %d records; wrote %d", last.Seq, len(tail), len(recs))
	}
	if got := integrityAfterReopen(t, l, dir, maxFile); got.State != StateOK || got.Seq != 60 {
		t.Errorf("integrity after rotations: %+v", got)
	}
}

// files returns the content of every audit file in dir.
func files(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "audit") && e.Type().IsRegular() { // a FIFO would block
			data, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err == nil {
				out[e.Name()] = data
			}
		}
	}
	return out
}

func restore(t *testing.T, dir string, snap map[string][]byte) {
	t.Helper()
	for name := range files(t, dir) {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	for name, data := range snap {
		if err := os.WriteFile(filepath.Join(dir, name), data, fileMode); err != nil {
			t.Fatal(err)
		}
	}
}

func lines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.SplitAfter(strings.TrimSuffix(string(data), "\n"), "\n")
}

func setLines(t *testing.T, path string, ls []string) {
	t.Helper()
	data := strings.Join(ls, "")
	if !strings.HasSuffix(data, "\n") && data != "" {
		data += "\n"
	}
	if err := os.WriteFile(path, []byte(data), fileMode); err != nil {
		t.Fatal(err)
	}
}

// Every way of tampering with the retained files or the head is detected,
// the files are left as they are, and nothing more is recorded.
func TestTamperingIsDetectedAndNeverRepaired(t *testing.T) {
	active := func(dir string) string { return filepath.Join(dir, activeFile) }
	tests := []struct {
		name   string
		rotate bool // write enough to fill every retained file
		tamper func(t *testing.T, dir string)
		reason string
	}{
		{"edited record", false, func(t *testing.T, dir string) {
			ls := lines(t, active(dir))
			ls[1] = strings.Replace(ls[1], `"uid":1000`, `"uid":1001`, 1)
			setLines(t, active(dir), ls)
		}, ReasonBadRecord},
		{"deleted record", false, func(t *testing.T, dir string) {
			ls := lines(t, active(dir))
			setLines(t, active(dir), append(ls[:1:1], ls[2:]...))
		}, ReasonGap},
		{"reordered records", false, func(t *testing.T, dir string) {
			ls := lines(t, active(dir))
			ls[0], ls[1] = ls[1], ls[0]
			setLines(t, active(dir), ls)
		}, ReasonGap},
		{"truncated tail", false, func(t *testing.T, dir string) {
			ls := lines(t, active(dir))
			setLines(t, active(dir), ls[:len(ls)-1])
		}, ReasonHeadAhead},
		{"torn tail", false, func(t *testing.T, dir string) {
			f, err := os.OpenFile(active(dir), os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if _, err := f.WriteString(`{"seq":`); err != nil {
				t.Fatal(err)
			}
		}, ReasonTornTail},
		{"oversized line", false, func(t *testing.T, dir string) {
			ls := lines(t, active(dir))
			setLines(t, active(dir), append(ls, strings.Repeat("a", MaxLineBytes+10)+"\n"))
		}, ReasonBadRecord},
		{"every file removed", false, func(t *testing.T, dir string) {
			if err := os.Remove(active(dir)); err != nil {
				t.Fatal(err)
			}
		}, ReasonHeadAhead},
		{"replaced by another valid chain", false, func(t *testing.T, dir string) {
			other := newDir(t)
			l := openLog(t, other, 0)
			if _, err := l.Record(context.Background(), entry("core.other", ResultOK)); err != nil {
				t.Fatal(err)
			}
			write(t, l, 3) // as long as the original
			restore(t, dir, map[string][]byte{activeFile: files(t, other)[activeFile], headFile: files(t, dir)[headFile]})
		}, ReasonHeadMismatch},
		{"head removed", false, func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, headFile)); err != nil {
				t.Fatal(err)
			}
		}, ReasonHeadMissing},
		{"head corrupt", false, func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, headFile), []byte(`{"version":1}`), fileMode); err != nil {
				t.Fatal(err)
			}
		}, ReasonHeadCorrupt},
		{"active file is a symbolic link", false, func(t *testing.T, dir string) {
			target := filepath.Join(t.TempDir(), "elsewhere")
			if err := os.Rename(active(dir), target); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, active(dir)); err != nil {
				t.Fatal(err)
			}
		}, ReasonUnreadable},
		{"active file is a FIFO", false, func(t *testing.T, dir string) {
			if err := os.Remove(active(dir)); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(active(dir), 0o600); err != nil {
				t.Fatal(err)
			}
		}, ReasonUnreadable},
		{"active file writable by others", false, func(t *testing.T, dir string) {
			if err := os.Chmod(active(dir), 0o622); err != nil {
				t.Fatal(err)
			}
		}, ReasonUnreadable},
		{"rotated file removed", true, func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, fileName(2))); err != nil {
				t.Fatal(err)
			}
		}, ReasonGap},
		{"oldest retained file removed", true, func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, fileName(retainedFiles-1))); err != nil {
				t.Fatal(err)
			}
		}, ReasonAnchor},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := newDir(t)
			n, maxFile := 4, int64(0) // one file
			if tt.rotate {
				n, maxFile = 40, 1000 // every retained file
			}
			l := openLog(t, dir, maxFile)
			write(t, l, n)
			if err := l.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			tt.tamper(t, dir)
			before := files(t, dir)

			l2, err := tryOpen(dir, maxFile, slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatal(err)
			}
			defer l2.Close(context.Background())
			got := l2.Integrity()
			if got.State != StateBroken || got.Reason != tt.reason {
				t.Fatalf("integrity = %+v, want broken (%s)", got, tt.reason)
			}
			if _, err := l2.Record(context.Background(), entry("core.reload", ResultIntent)); !errors.Is(err, ErrBroken) {
				t.Errorf("Record on a broken chain: %v", err)
			}
			if after := files(t, dir); fmt.Sprint(after) != fmt.Sprint(before) {
				t.Error("the files changed after the break was found")
			}
		})
	}
}

// A crash between writing a record and saving the head leaves the head one
// record behind: that record is durable and chained, so it is accepted.
// Two records behind is not a crash window.
func TestHeadBehind(t *testing.T) {
	for _, behind := range []int{1, 2} {
		t.Run(fmt.Sprint(behind), func(t *testing.T) {
			dir := newDir(t)
			l := openLog(t, dir, 0)
			write(t, l, 3-behind)
			old := files(t, dir)[headFile]
			write(t, l, behind)
			if err := l.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, headFile), old, fileMode); err != nil {
				t.Fatal(err)
			}
			got := openLog(t, dir, 0).Integrity()
			switch {
			case behind == 1 && (got.State != StateOK || got.Seq != 3):
				t.Errorf("one record behind: %+v", got)
			case behind == 2 && (got.State != StateBroken || got.Reason != ReasonHeadMismatch):
				t.Errorf("two records behind: %+v", got)
			}
		})
	}
}

// A crash at any step of a rotation leaves a chain that still checks.
func TestInterruptedRotation(t *testing.T) {
	const maxFile = 1000
	steps := []struct {
		name  string
		crash func(t *testing.T, dir string)
	}{
		// The anchor moved to the oldest file's last record, the file is
		// still there.
		{"before the oldest file is removed", func(t *testing.T, dir string) {}},
		{"after the oldest file is removed", func(t *testing.T, dir string) {
			mustRemove(t, filepath.Join(dir, fileName(4)))
		}},
		{"halfway through the renames", func(t *testing.T, dir string) {
			mustRemove(t, filepath.Join(dir, fileName(4)))
			mustRename(t, filepath.Join(dir, fileName(3)), filepath.Join(dir, fileName(4)))
		}},
		{"before the new active file exists", func(t *testing.T, dir string) {
			mustRemove(t, filepath.Join(dir, fileName(4)))
			for i := 3; i >= 0; i-- {
				mustRename(t, filepath.Join(dir, fileName(i)), filepath.Join(dir, fileName(i+1)))
			}
		}},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			dir := newDir(t)
			l := openLog(t, dir, maxFile)
			// Fill every retained file, up to the record that rotates.
			for {
				write(t, l, 1)
				if _, err := os.Stat(filepath.Join(dir, fileName(4))); err == nil && l.size+400 > maxFile {
					break
				}
			}
			before := files(t, dir)
			beforeHead := l.head
			write(t, l, 1) // rotates
			anchor := l.head.Anchor
			if anchor == beforeHead.Anchor {
				t.Fatal("the last record did not rotate")
			}
			if err := l.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			// Back to the moment the anchor was saved, then the crash.
			restore(t, dir, before)
			if err := writeHead(mustRoot(t, dir), head{Last: beforeHead.Last, Anchor: anchor}); err != nil {
				t.Fatal(err)
			}
			step.crash(t, dir)
			l2 := openLog(t, dir, maxFile)
			if got := l2.Integrity(); got.State != StateOK || got.Seq != beforeHead.Last.Seq {
				t.Fatalf("integrity = %+v", got)
			}
			write(t, l2, 1)
			if got := integrityAfterReopen(t, l2, dir, maxFile); got.State != StateOK {
				t.Errorf("after one more record: %+v", got)
			}
		})
	}
}

func mustRoot(t *testing.T, dir string) *os.Root {
	t.Helper()
	r, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func mustRemove(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}

func mustRename(t *testing.T, from, to string) {
	t.Helper()
	if err := os.Rename(from, to); err != nil {
		t.Fatal(err)
	}
}

// A failed write leaves the end of the chain uncertain: the log refuses
// every later record (fail closed), and status says why.
func TestWriteFailureBreaksTheLog(t *testing.T) {
	dir := newDir(t)
	l := openLog(t, dir, 0)
	write(t, l, 1)
	_ = l.active.Close() // the next write fails
	if _, err := l.Record(context.Background(), entry("core.reload", ResultIntent)); err == nil {
		t.Fatal("Record succeeded on a closed file")
	}
	if got := l.Integrity(); got.State != StateBroken || got.Reason != ReasonWriteFailed {
		t.Errorf("integrity = %+v", got)
	}
	if _, err := l.Record(context.Background(), entry("core.reload", ResultIntent)); !errors.Is(err, ErrBroken) {
		t.Errorf("next Record: %v", err)
	}
}

// The record is durable but the head could not be saved: the log stops,
// and the next start accepts the record (one ahead of the head).
func TestHeadSaveFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := newDir(t)
	l := openLog(t, dir, 0)
	write(t, l, 1)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	_, err := l.Record(context.Background(), entry("core.reload", ResultIntent))
	if chmodErr := os.Chmod(dir, 0o700); chmodErr != nil {
		t.Fatal(chmodErr)
	}
	if err == nil || l.Integrity().Reason != ReasonWriteFailed {
		t.Fatalf("Record = %v, integrity %+v", err, l.Integrity())
	}
	if got := integrityAfterReopen(t, l, dir, 0); got.State != StateOK || got.Seq != 2 {
		t.Errorf("after restart: %+v", got)
	}
}

func TestFreshDirectory(t *testing.T) {
	dir := newDir(t)
	// An empty active file without head: a crash before the first record.
	if err := os.WriteFile(filepath.Join(dir, activeFile), nil, fileMode); err != nil {
		t.Fatal(err)
	}
	if got := openLog(t, dir, 0).Integrity(); got.State != StateOK || got.Seq != 0 {
		t.Errorf("integrity = %+v", got)
	}
}

func TestInvalidEntriesAreNotWritten(t *testing.T) {
	long := strings.Repeat("x", maxDetailValue+1)
	many := map[string]string{}
	for i := range maxDetailKeys + 1 {
		many[fmt.Sprintf("k%d", i)] = "v"
	}
	tests := map[string]func(*Entry){
		"actor kind":     func(e *Entry) { e.Actor.Kind = "cli" },
		"action":         func(e *Entry) { e.Action = "reload; rm -rf /" },
		"result":         func(e *Entry) { e.Result = "maybe" },
		"target length":  func(e *Entry) { e.Target = strings.Repeat("t", maxTargetBytes+1) },
		"target UTF-8":   func(e *Entry) { e.Target = "\xff" },
		"correlation":    func(e *Entry) { e.CorrelationID = "a b" },
		"detail key":     func(e *Entry) { e.Detail = map[string]string{"Bad-Key": "v"} },
		"detail value":   func(e *Entry) { e.Detail = map[string]string{"k": long} },
		"too many keys":  func(e *Entry) { e.Detail = many },
		"detail UTF-8":   func(e *Entry) { e.Detail = map[string]string{"k": "\xc3"} },
		"no action name": func(e *Entry) { e.Action = "" },
	}
	dir := newDir(t)
	l := openLog(t, dir, 0)
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			e := entry("core.reload", ResultIntent)
			mutate(&e)
			if _, err := l.Record(context.Background(), e); err == nil {
				t.Error("invalid entry recorded")
			}
		})
	}
	if l.Integrity().State != StateOK || l.size != 0 {
		t.Errorf("an invalid entry changed the log: %+v, %d bytes", l.Integrity(), l.size)
	}
}

// A caller waits for the log at most until its context ends.
func TestRecordHonoursTheContext(t *testing.T) {
	l := openLog(t, newDir(t), 0)
	l.sem <- struct{}{} // a write in progress
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := l.Record(ctx, entry("core.reload", ResultIntent)); !errors.Is(err, context.Canceled) {
		t.Errorf("Record = %v", err)
	}
	l.release()
}

func TestConcurrentRecordsFormOneChain(t *testing.T) {
	dir := newDir(t)
	l := openLog(t, dir, 2000)
	var wg sync.WaitGroup
	for range 40 {
		wg.Go(func() {
			if _, err := l.Record(context.Background(), entry("core.reload", ResultOK)); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	tail, _, err := l.Tail(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	checkChain(t, tail)
	if got := integrityAfterReopen(t, l, dir, 2000); got.State != StateOK || got.Seq != 40 {
		t.Errorf("integrity = %+v", got)
	}
}

// Every record is mirrored to the log, which leaves the host through the
// journal (ADR-0011).
func TestRecordsAreMirroredToTheLog(t *testing.T) {
	var buf bytes.Buffer
	l, err := tryOpen(newDir(t), 0, slog.New(slog.NewTextHandler(&buf, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close(context.Background())
	if _, err := l.Record(context.Background(), entry("core.reload", ResultDenied)); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"msg=audit", "seq=1", "action=core.reload", "result=denied", "uid=1000", "pid=42", "hash=sha256:"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("mirror %q lacks %q", buf.String(), want)
		}
	}
}

func TestTailSkipsUnparsableLines(t *testing.T) {
	dir := newDir(t)
	l := openLog(t, dir, 0)
	write(t, l, 3)
	if err := l.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	ls := lines(t, filepath.Join(dir, activeFile))
	ls[1] = "not a record\n"
	setLines(t, filepath.Join(dir, activeFile), ls)
	l = openLog(t, dir, 0)
	tail, skipped, err := l.Tail(context.Background(), 10)
	if err != nil || len(tail) != 2 || skipped != 1 {
		t.Errorf("Tail = %d records, %d skipped, %v", len(tail), skipped, err)
	}
	if l.Integrity().State != StateBroken {
		t.Error("tampered log reported intact")
	}
}

// Review 2c-3a R1: the outcome of an action that ran must be recorded. If
// it cannot even wait for the files, the log stops, as after a failed
// write, so no later privileged request runs as if the log were complete.
func TestLostOutcomeBreaksTheLog(t *testing.T) {
	l := openLog(t, newDir(t), 0)
	if _, err := l.Record(context.Background(), entry("core.reload", ResultIntent)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := l.Record(ctx, entry("core.reload", ResultOK)); !errors.Is(err, context.Canceled) {
		t.Fatalf("outcome: %v", err)
	}
	if got := l.Integrity(); got.State != StateBroken || got.Reason != ReasonWriteFailed {
		t.Fatalf("integrity after a lost outcome: %+v", got)
	}
	if _, err := l.Record(context.Background(), entry("core.reload", ResultIntent)); !errors.Is(err, ErrBroken) {
		t.Errorf("next intent: %v", err)
	}
	// An intent or a denial that cannot wait changes nothing: no action ran.
	l2 := openLog(t, newDir(t), 0)
	for _, r := range []Result{ResultIntent, ResultDenied} {
		if _, err := l2.Record(ctx, entry("core.reload", r)); err == nil {
			t.Fatal("recorded with an ended context")
		}
	}
	if l2.Integrity().State != StateOK {
		t.Errorf("a lost intent broke the log: %+v", l2.Integrity())
	}
}

// Review 2c-3a R2: rotation makes every removal, rename and creation
// durable before the next one, and stops at the first failure; whatever
// step fails, the files left check.
func TestRotationSyncsEveryStep(t *testing.T) {
	const maxFile = 1000
	fill := func(t *testing.T) (*Log, string) {
		dir := newDir(t)
		l := openLog(t, dir, maxFile)
		for {
			write(t, l, 1)
			if _, err := os.Stat(filepath.Join(dir, fileName(4))); err == nil && l.size+400 > maxFile {
				return l, dir
			}
		}
	}
	l, _ := fill(t)
	var listings []string
	l.syncDir = func(r *os.Root) error {
		entries, _ := fs.ReadDir(r.FS(), ".")
		names := ""
		for _, e := range entries {
			names += e.Name() + " "
		}
		listings = append(listings, names)
		return syncDir(r)
	}
	write(t, l, 1) // rotates
	// Remove the oldest, three renames, the active file renamed, the new
	// one created: six barriers, each after exactly one change.
	if len(listings) != 6 {
		t.Fatalf("%d directory syncs during rotation, want 6:\n%s", len(listings), strings.Join(listings, "\n"))
	}
	for k := range 6 {
		t.Run(fmt.Sprint("sync ", k+1, " fails"), func(t *testing.T) {
			l, dir := fill(t)
			calls := 0
			l.syncDir = func(r *os.Root) error {
				if calls++; calls == k+1 {
					return errors.New("injected")
				}
				return syncDir(r)
			}
			if _, err := l.Record(context.Background(), entry("core.reload", ResultIntent)); err == nil {
				t.Fatal("record succeeded although the rotation failed")
			}
			if calls != k+1 {
				t.Errorf("rotation went on after the failed sync: %d syncs", calls)
			}
			if got := integrityAfterReopen(t, l, dir, maxFile); got.State != StateOK {
				t.Errorf("files left after the failure do not check: %+v", got)
			}
		})
	}
}

// ctxAfter is a context that ends after n calls to Err, to end a call
// while it runs.
type ctxAfter struct {
	context.Context
	n int
}

func (c *ctxAfter) Err() error {
	if c.n--; c.n < 0 {
		return context.Canceled
	}
	return nil
}

// Review 2c-3a R3: Tail stops when its caller gives up, since it holds the
// files that records wait for.
func TestTailStopsWhenTheContextEnds(t *testing.T) {
	l := openLog(t, newDir(t), 0)
	write(t, l, 50)
	ctx := &ctxAfter{Context: context.Background(), n: 10}
	if _, _, err := l.Tail(ctx, 5); !errors.Is(err, context.Canceled) {
		t.Fatalf("Tail = %v", err)
	}
	if ctx.n > -1 || ctx.n < -2 {
		t.Errorf("Tail went on reading after the context ended (%d checks left)", ctx.n)
	}
	if _, err := l.Record(context.Background(), entry("core.reload", ResultIntent)); err != nil {
		t.Errorf("files not released: %v", err)
	}
}

// Review 2c-3a R5: an outcome refused as invalid or too large is lost like
// one that could not be written: the log stops.
func TestRejectedOutcomeBreaksTheLog(t *testing.T) {
	for _, result := range []Result{ResultOK, ResultFailed} {
		for _, oversized := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/oversized=%t", result, oversized), func(t *testing.T) {
				l := openLog(t, newDir(t), 0)
				if _, err := l.Record(context.Background(), entry("core.reload", ResultIntent)); err != nil {
					t.Fatal(err)
				}
				e := entry("core.reload", result)
				if oversized { // valid fields, too large a line
					e.Detail = map[string]string{}
					for i := range maxDetailKeys {
						e.Detail[fmt.Sprintf("k%d", i)] = strings.Repeat("x", maxDetailValue)
					}
					if err := e.validate(); err != nil {
						t.Fatal(err)
					}
				} else {
					e.Detail = map[string]string{"code": strings.Repeat("x", maxDetailValue+1)}
				}
				if _, err := l.Record(context.Background(), e); err == nil {
					t.Fatal("outcome recorded")
				}
				if got := l.Integrity(); got.State != StateBroken || got.Reason != ReasonWriteFailed {
					t.Fatalf("lost outcome left integrity %+v", got)
				}
				if _, err := l.Record(context.Background(), entry("core.reload", ResultIntent)); !errors.Is(err, ErrBroken) {
					t.Fatalf("later intent: %v", err)
				}
			})
		}
	}
}

// Review 2c-3a R8: an oversized line does not hide the records after it
// from Tail; it is counted as skipped. The start-up check stays strict.
func TestTailContinuesAfterAnOversizedLine(t *testing.T) {
	dir := newDir(t)
	l := openLog(t, dir, 0)
	write(t, l, 3)
	if err := l.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, activeFile)
	ls := lines(t, path)
	ls[1] = strings.Repeat("x", 3*MaxLineBytes) + "\n"
	setLines(t, path, ls)
	l = openLog(t, dir, 0)
	recs, skipped, err := l.Tail(context.Background(), 10)
	if err != nil || skipped != 1 || len(recs) != 2 || recs[1].Seq != 3 {
		t.Fatalf("Tail: %d records (last %+v), %d skipped, %v", len(recs), recs, skipped, err)
	}
	if got := l.Integrity(); got.Reason != ReasonBadRecord {
		t.Errorf("integrity %+v", got)
	}
}

// A Close that gave up on a write in progress refuses records at once and
// does not count as done: the next Close closes the files.
func TestCloseRetriesAfterAWriteInProgress(t *testing.T) {
	l := openLog(t, newDir(t), 0)
	l.sem <- struct{}{} // a write in progress
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := l.Close(ctx); err == nil {
		t.Fatal("Close succeeded with a write in progress")
	}
	l.release() // the write ends
	if l.root == nil {
		t.Fatal("files closed under a write")
	}
	if err := l.Close(context.Background()); err != nil || l.root != nil {
		t.Fatalf("second Close: %v, root %v", err, l.root)
	}
	if _, err := l.Record(context.Background(), entry("core.reload", ResultIntent)); !errors.Is(err, ErrClosed) {
		t.Errorf("Record after Close: %v", err)
	}
	if err := l.Close(context.Background()); err != nil {
		t.Errorf("third Close: %v", err)
	}
}

// pauseClock pauses the first call of Now: Record calls it after its first
// checks, before writing.
type pauseClock struct {
	clock.Clock
	once    sync.Once
	entered chan struct{}
	resume  chan struct{}
}

func (c *pauseClock) Now() time.Time {
	c.once.Do(func() {
		close(c.entered)
		<-c.resume
	})
	return c.Clock.Now()
}

// Review 2c-3a R9 (seventh review): an intent being written when an outcome
// is lost neither writes nor rotates evidence away.
func TestInFlightIntentPreservesEvidenceAfterALostOutcome(t *testing.T) {
	for _, maxFile := range []int64{0, 1} { // no rotation; one record per file
		t.Run(fmt.Sprint(maxFile), func(t *testing.T) { inFlightIntent(t, maxFile) })
	}
}

func inFlightIntent(t *testing.T, maxFile int64) {
	dir := newDir(t)
	l := openLog(t, dir, maxFile)
	write(t, l, retainedFiles)
	before := files(t, dir)
	c := &pauseClock{Clock: l.clock, entered: make(chan struct{}), resume: make(chan struct{})}
	l.clock = c
	bg := context.Background()
	done := make(chan error, 1)
	go func() {
		_, err := l.Record(bg, entry("core.reload", ResultIntent))
		done <- err
	}()
	<-c.entered
	ctx, cancel := context.WithCancel(bg)
	cancel()
	if _, err := l.Record(ctx, entry("core.reload", ResultOK)); !errors.Is(err, context.Canceled) {
		t.Fatalf("outcome: %v", err)
	}
	close(c.resume)
	if err := <-done; !errors.Is(err, ErrBroken) {
		t.Fatalf("intent: %v", err)
	}
	if after := files(t, dir); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Error("files changed after the log broke")
	}
}

// Review 2c-3a R9 (eighth review): an outcome lost during a rotation stops
// it at the next step; nothing more changes, not even the intent's line.
func TestLostOutcomeDuringRotationStopsFurtherChanges(t *testing.T) {
	for pause := range 6 { // at each of the rotation's directory syncs
		t.Run(fmt.Sprint("sync ", pause+1), func(t *testing.T) {
			dir := newDir(t)
			l := openLog(t, dir, 1) // one record per file
			write(t, l, retainedFiles)
			entered, resume := make(chan struct{}), make(chan struct{})
			calls := 0
			l.syncDir = func(r *os.Root) error {
				err := syncDir(r)
				if calls++; calls == pause+1 {
					close(entered)
					<-resume
				}
				return err
			}
			done := make(chan error, 1)
			go func() {
				_, err := l.Record(context.Background(), entry("core.reload", ResultIntent))
				done <- err
			}()
			<-entered
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if _, err := l.Record(ctx, entry("core.reload", ResultOK)); !errors.Is(err, context.Canceled) {
				t.Fatalf("outcome: %v", err)
			}
			before := files(t, dir) // the evidence when the log broke
			close(resume)
			if err := <-done; !errors.Is(err, ErrBroken) {
				t.Fatalf("intent: %v", err)
			}
			if after := files(t, dir); fmt.Sprint(after) != fmt.Sprint(before) {
				t.Errorf("files changed after the log broke (sync %d)", pause+1)
			}
		})
	}
}
