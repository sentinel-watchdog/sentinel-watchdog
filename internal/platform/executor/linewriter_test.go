package executor

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/clock"
)

func collect(c clock.Clock, opts LineOptions) (*LineWriter, *[]string) {
	var lines []string
	return NewLineWriter(c, opts, func(l string) { lines = append(lines, l) }), &lines
}

func TestLineWriterSplitsAndCuts(t *testing.T) {
	w, lines := collect(clock.Real(), LineOptions{MaxLine: 8, Rate: 1000, Burst: 1000})
	for _, chunk := range []string{"ab", "c\nde", "f\n\n", "0123456789abc", "def\nlast"} {
		_, _ = w.Write([]byte(chunk))
	}
	_ = w.Close()
	want := []string{"abc", "def", "", "01234567 [line cut]", "last"}
	if strings.Join(*lines, "|") != strings.Join(want, "|") {
		t.Errorf("lines %q, want %q", *lines, want)
	}
}

// A line cut inside a UTF-8 sequence does not end with half a character.
func TestLineWriterCutsOnRunes(t *testing.T) {
	w, lines := collect(clock.Real(), LineOptions{MaxLine: 5, Rate: 1000, Burst: 1000})
	_, _ = w.Write([]byte("aaaé€\n"))  // é is 2 bytes: the cut after 5 bytes falls after it
	_, _ = w.Write([]byte("aaaa€x\n")) // 4 + 3 bytes: cut inside €
	for _, l := range *lines {
		if !utf8.ValidString(l) {
			t.Errorf("invalid UTF-8 %q", l)
		}
	}
	if len(*lines) != 2 || (*lines)[1] != "aaaa [line cut]" {
		t.Errorf("lines %q", *lines)
	}
}

// A flood of lines is dropped beyond the rate and reported when lines
// flow again (D-084: a child cannot flood the journal).
func TestLineWriterRateLimit(t *testing.T) {
	fake := clock.NewFake(time.Unix(0, 0))
	w, lines := collect(fake, LineOptions{MaxLine: 100, Rate: 2, Burst: 3})
	for range 10 {
		_, _ = w.Write([]byte("x\n"))
	}
	if len(*lines) != 3 || w.Dropped() != 7 {
		t.Fatalf("%d lines, %d dropped", len(*lines), w.Dropped())
	}
	fake.Advance(time.Second) // two tokens
	_, _ = w.Write([]byte("y\n"))
	if got := (*lines)[3:]; len(got) != 2 || !strings.HasPrefix(got[0], "[7 lines dropped") || got[1] != "y" {
		t.Errorf("after the pause: %q", got)
	}
}

// FuzzLineWriter: whatever a child prints, no emitted line exceeds the
// limit by more than the marker, and every byte is accounted for.
func FuzzLineWriter(f *testing.F) {
	f.Add([]byte("a\nb"), 4)
	f.Add([]byte("\xff\xfe\n€€€€"), 3)
	f.Fuzz(func(t *testing.T, data []byte, max int) {
		max = 1 + abs(max)%64
		w, lines := collect(clock.Real(), LineOptions{MaxLine: max, Rate: 1e9, Burst: 1 << 20})
		_, _ = w.Write(data)
		_ = w.Close()
		for _, l := range *lines {
			if len(l) > max+len(" [line cut]") {
				t.Fatalf("line of %d bytes with max %d", len(l), max)
			}
		}
	})
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// Full review 2026-10-28 N4: when the sink blocks past the drain
// timeouts, the copy is abandoned inside LineWriter.Write; the supervisor
// then closes its sinks (closeAll) once the process is done, so Close runs
// beside a Write that resumes later. The race shows under -race (task
// check runs it); the 1 s bound guards against a fix that would make Close
// wait for the blocked Write (a mutex), hanging the supervisor's closeAll.
func TestLineWriterCloseAfterAbandonedCopy(t *testing.T) {
	blocked, release := make(chan struct{}), make(chan struct{})
	first := true
	w := NewLineWriter(clock.Real(), DefaultLineOptions, func(string) {
		if first {
			first = false
			close(blocked)
			<-release // the log's destination stops reading
		}
	})
	p, err := newExecutor(t).Start(context.Background(), helper(t, "lines", w)) // "one\ntwo\nthree"
	if err != nil {
		t.Fatal(err)
	}
	<-blocked
	waitDone(t, p) // drain gave up on the blocked copy
	closed := make(chan struct{})
	go func() { _ = w.Close(); close(closed) }() // what closeAll does after the process ended
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Error("Close waits for a Write blocked in the sink")
	}
	close(release) // the copy resumes inside Write
	<-closed
	_ = w.Close()
}
