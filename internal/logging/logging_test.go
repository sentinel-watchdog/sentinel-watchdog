package logging

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
)

func TestParseLevel(t *testing.T) {
	tests := []struct {
		in      string
		want    slog.Level
		wantErr bool
	}{
		{"debug", slog.LevelDebug, false},
		{"INFO", slog.LevelInfo, false},
		{"warn", slog.LevelWarn, false},
		{"warning", slog.LevelWarn, false},
		{"error", slog.LevelError, false},
		{"trace", 0, true},
	}
	for _, tt := range tests {
		got, err := ParseLevel(tt.in)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("ParseLevel(%q) = %v, %v", tt.in, got, err)
		}
	}
}

func TestTextFormatRedactsSecrets(t *testing.T) {
	var buf bytes.Buffer
	log, err := New(Options{Format: FormatText, Output: &buf})
	if err != nil {
		t.Fatal(err)
	}
	log.Info("delivering", "monitor", "api", "token", "s3cret",
		slog.Group("request", slog.String("Authorization", "Bearer s3cret"), slog.Int("status", 200)))
	out := buf.String()
	if strings.Contains(out, "s3cret") {
		t.Fatalf("secret leaked: %s", out)
	}
	for _, want := range []string{"time=", "level=INFO", "monitor=api", "token=[REDACTED]", "request.status=200"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %q", want, out)
		}
	}
}

func TestJSONFormat(t *testing.T) {
	var buf bytes.Buffer
	log, err := New(Options{Format: FormatJSON, Output: &buf, Level: slog.LevelDebug})
	if err != nil {
		t.Fatal(err)
	}
	log.Debug("check", "monitor", "worker", "password", "pw")
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("not JSON: %v: %s", err, buf.String())
	}
	if rec["monitor"] != "worker" || rec["password"] != "[REDACTED]" || rec["level"] != "DEBUG" {
		t.Errorf("unexpected record: %v", rec)
	}
}

func TestJournalFormat(t *testing.T) {
	var buf bytes.Buffer
	level := new(slog.LevelVar)
	level.Set(slog.LevelDebug)
	log, err := New(Options{Format: FormatJournal, Output: &buf, Level: level})
	if err != nil {
		t.Fatal(err)
	}
	log = log.With("component", "test")
	log.Error("boom", "api_key", "k")
	log.Warn("careful")
	log.Info("hello")
	log.Debug("details")
	level.Set(slog.LevelInfo)
	log.Debug("hidden after level change")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("expected 4 lines, got %d: %q", len(lines), lines)
	}
	for i, prefix := range []string{"<3>", "<4>", "<6>", "<7>"} {
		if !strings.HasPrefix(lines[i], prefix) {
			t.Errorf("line %d = %q, want prefix %s", i, lines[i], prefix)
		}
		if strings.Contains(lines[i], "time=") {
			t.Errorf("journal lines must not carry timestamps: %q", lines[i])
		}
		if !strings.Contains(lines[i], "component=test") {
			t.Errorf("WithAttrs lost: %q", lines[i])
		}
	}
	if !strings.Contains(lines[0], "api_key=[REDACTED]") {
		t.Errorf("not redacted: %q", lines[0])
	}
}

func TestJournalFormatConcurrent(t *testing.T) {
	var buf safeBuffer
	log, err := New(Options{Format: FormatJournal, Output: &buf, Level: slog.LevelDebug})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Go(func() {
			if i%2 == 0 {
				log.Error("e", "i", i)
			} else {
				log.Debug("d", "i", i)
			}
		})
	}
	wg.Wait()
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		switch {
		case strings.HasPrefix(line, "<3>level=ERROR"), strings.HasPrefix(line, "<7>level=DEBUG"):
		default:
			t.Errorf("priority does not match level: %q", line)
		}
	}
}

func TestUnknownFormat(t *testing.T) {
	if _, err := New(Options{Format: "xml", Output: &bytes.Buffer{}}); err == nil {
		t.Error("expected error")
	}
}

func TestAutoFormat(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	st := info.Sys().(*syscall.Stat_t)
	stream := fmt.Sprintf("%d:%d", st.Dev, st.Ino)

	tests := []struct {
		name, env, wantPrefix string
	}{
		{"journal stream matches", stream, "<6>"},
		{"journal stream differs", "1:2", "time="},
		{"no journal", "", "time="},
		{"garbage", "abc", "time="},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := f.Truncate(0); err != nil {
				t.Fatal(err)
			}
			if _, err := f.Seek(0, 0); err != nil {
				t.Fatal(err)
			}
			log, err := New(Options{Format: FormatAuto, Output: f, Getenv: func(string) string { return tt.env }})
			if err != nil {
				t.Fatal(err)
			}
			log.Info("x")
			data, err := os.ReadFile(f.Name())
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(string(data), tt.wantPrefix) {
				t.Errorf("output %q, want prefix %q", data, tt.wantPrefix)
			}
		})
	}
}

type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
