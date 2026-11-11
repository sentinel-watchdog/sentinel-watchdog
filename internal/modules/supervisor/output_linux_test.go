package supervisor

import (
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/platform/executor"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/platform/journald"
)

// fullJournal returns the path of a datagram socket, at a filesystem path
// (R-024), whose queue is full: it is never read.
func fullJournal(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "j.sock")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	entry := journald.Encode([]journald.Field{{Name: "MESSAGE", Value: "x"}})
	for range 5000 {
		err := journald.Send(path, entry)
		if errors.Is(err, journald.ErrFull) {
			return path
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Fatal("the queue never filled")
	return ""
}

// Criterion 15 with the real adapter: against a journald that stopped
// reading, the program's output is consumed at once, a final line without
// newline included, and every line is counted, none logged.
func TestOutputAgainstAFullJournal(t *testing.T) {
	path := fullJournal(t)
	send := func(entry []byte) error { return journald.Send(path, entry) }
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{output: []string{"one", "two"}, partial: "three"}
		r := start(t, configureWithJournal(t, map[string]string{"supervisor/a.yaml": loggedService}, l, send), "")
		defer r.stop(t)
		time.Sleep(time.Second) // the first line is retried for 20 ms (runSink)
		l.proc(0).end(executor.Exit{}, nil)
		synctest.Wait()
		if st := statusResult(t, r.in); st.OutputDropped != 3 {
			t.Errorf("output_dropped = %d, want 3", st.OutputDropped)
		}
		if logs := r.logs.String(); strings.Contains(logs, "service output") {
			t.Errorf("logs:\n%s", logs)
		}
	})
}
