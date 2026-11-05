package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/platform/privilege"
)

// The whole argv is fixed but for the two values of the query, and neither
// can become an option of its own.
func TestJournalArgs(t *testing.T) {
	fixed := []string{"--no-pager", "--output=json", "--output-fields=MESSAGE"}
	tests := []struct {
		name string
		q    JournalQuery
		want []string // nil: refused
	}{
		{"a unit", JournalQuery{Unit: "nginx.service", Lines: 50}, append(slices.Clone(fixed), "--lines=50", "--unit=nginx.service")},
		{"an instance", JournalQuery{Unit: `getty@tty1\x2d.service`, Lines: 1000}, append(slices.Clone(fixed), "--lines=1000", `--unit=getty@tty1\x2d.service`)},
		{"the probe", JournalQuery{Lines: 1}, append(slices.Clone(fixed), "--lines=1")},
		{"no unit and more than one line", JournalQuery{Lines: 2}, nil},
		{"no lines", JournalQuery{Unit: "a.service"}, nil},
		{"negative lines", JournalQuery{Unit: "a.service", Lines: -1}, nil},
		{"too many lines", JournalQuery{Unit: "a.service", Lines: MaxJournalLines + 1}, nil},
		{"an option as unit", JournalQuery{Unit: "--grep=x", Lines: 1}, nil},
		{"a dash first", JournalQuery{Unit: "-a.service", Lines: 1}, nil},
		{"a glob, which would widen the match", JournalQuery{Unit: "*.service", Lines: 1}, nil},
		{"a question mark glob", JournalQuery{Unit: "a?.service", Lines: 1}, nil},
		{"a bracket glob", JournalQuery{Unit: "[a].service", Lines: 1}, nil},
		{"a space", JournalQuery{Unit: "a b.service", Lines: 1}, nil},
		{"a newline", JournalQuery{Unit: "a\n.service", Lines: 1}, nil},
		{"a path", JournalQuery{Unit: "../a.service", Lines: 1}, nil},
		{"too long", JournalQuery{Unit: strings.Repeat("a", 256), Lines: 1}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := journalArgs(tt.q)
			if tt.want == nil {
				if err == nil {
					t.Fatalf("args %q, want a refusal", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("args %q, want %q", got, tt.want)
			}
		})
	}
}

// --reverse with --lines skips back and goes on older on Rocky 8 (D-099),
// losing exactly the lines an operator asks for.
func TestJournalArgsNeverReverse(t *testing.T) {
	got, err := journalArgs(JournalQuery{Unit: "a.service", Lines: 5})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(got, "--reverse") || slices.Contains(got, "-r") {
		t.Errorf("args %q", got)
	}
}

func FuzzJournalArgs(f *testing.F) {
	f.Add("nginx.service", 50)
	f.Add("--grep=x", 1)
	f.Add("", 1)
	f.Add("a\x00b", 3)
	f.Fuzz(func(t *testing.T, unit string, lines int) {
		got, err := journalArgs(JournalQuery{Unit: unit, Lines: lines})
		if err != nil {
			return
		}
		want := []string{"--no-pager", "--output=json", "--output-fields=MESSAGE", fmt.Sprintf("--lines=%d", lines)}
		if unit != "" {
			want = append(want, "--unit="+unit)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("args %q, want %q", got, want)
		}
		if strings.ContainsAny(unit, "*?[]\n\x00 ") || strings.HasPrefix(unit, "-") {
			t.Fatalf("accepted unit %q", unit)
		}
	})
}

func journalLookup(user, group bool) privilege.Lookup {
	return privilege.Lookup{
		User: func(string) (uint32, uint32, error) {
			if !user {
				return 0, 0, privilege.ErrUnknownUser
			}
			return 65534, 65534, nil
		},
		Group: func(string) (uint32, error) {
			if !group {
				return 0, privilege.ErrUnknownGroup
			}
			return 190, nil
		},
	}
}

func TestJournalSpec(t *testing.T) {
	bin := helperPath(t)
	q := JournalQuery{Unit: "a.service", Lines: 3}

	// By number: Leap 15 has no nobody in /etc/passwd, so the lookup of a user
	// must not be needed.
	t.Run("as root: nobody by number and the journal's group, no supplementary group", func(t *testing.T) {
		spec, err := journalSpec(q, []string{bin}, journalLookup(false, true), 0)
		if err != nil {
			t.Fatal(err)
		}
		if spec.Cred == nil || spec.Cred.UID != 65534 || spec.Cred.GID != 190 || len(spec.Cred.Groups) != 0 {
			t.Errorf("cred %+v", spec.Cred)
		}
		if !slices.Equal(spec.Env, []string{"LC_ALL=C"}) || spec.Dir != "" {
			t.Errorf("env %q dir %q", spec.Env, spec.Dir)
		}
		if !slices.Contains(spec.Args, "--unit=a.service") {
			t.Errorf("args %q", spec.Args)
		}
	})
	t.Run("as another user: its own identity", func(t *testing.T) {
		spec, err := journalSpec(q, []string{bin}, journalLookup(false, false), 1000)
		if err != nil {
			t.Fatal(err)
		}
		if spec.Cred != nil {
			t.Errorf("cred %+v", spec.Cred)
		}
	})
	t.Run("the first candidate that exists", func(t *testing.T) {
		spec, err := journalSpec(q, []string{filepath.Join(t.TempDir(), "absent"), bin}, journalLookup(true, true), 1000)
		if err != nil {
			t.Fatal(err)
		}
		if spec.Path == "" || filepath.Base(spec.Path) != filepath.Base(bin) {
			t.Errorf("path %q", spec.Path)
		}
	})

	unavailable := []struct {
		name     string
		binaries func(t *testing.T) []string
		lookup   privilege.Lookup
		euid     int
	}{
		{"no binary", func(t *testing.T) []string { return []string{filepath.Join(t.TempDir(), "absent")} }, journalLookup(true, true), 0},
		{"an untrusted binary is not passed over", func(t *testing.T) []string {
			dir := t.TempDir()
			bad := filepath.Join(dir, "journalctl")
			if err := os.WriteFile(bad, nil, 0o777); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(bad, 0o777); err != nil {
				t.Fatal(err)
			}
			return []string{bad, bin}
		}, journalLookup(true, true), 0},
		{"no such group", func(*testing.T) []string { return []string{bin} }, journalLookup(true, false), 0},
	}
	for _, tt := range unavailable {
		t.Run(tt.name, func(t *testing.T) {
			_, err := journalSpec(q, tt.binaries(t), tt.lookup, tt.euid)
			if !errors.Is(err, ErrJournalUnavailable) {
				t.Errorf("err = %v", err)
			}
		})
	}

	t.Run("a refused query is not a matter of availability", func(t *testing.T) {
		_, err := journalSpec(JournalQuery{Unit: "--grep=x", Lines: 1}, []string{bin}, journalLookup(true, true), 0)
		if err == nil || errors.Is(err, ErrJournalUnavailable) {
			t.Errorf("err = %v", err)
		}
	})
}

func TestRunJournal(t *testing.T) {
	e := newExecutor(t)

	t.Run("output and exit", func(t *testing.T) {
		out, err := e.runJournal(t.Context(), helper(t, "lines", nil))
		if err != nil {
			t.Fatal(err)
		}
		if string(out.Stdout) != "one\ntwo\nthree" || out.Truncated || out.Exit.Code != 0 {
			t.Errorf("%+v", out)
		}
	})
	t.Run("a failure is reported, not hidden", func(t *testing.T) {
		out, err := e.runJournal(t.Context(), helper(t, "exit:1", nil))
		if err != nil {
			t.Fatal(err)
		}
		if out.Exit.Code != 1 {
			t.Errorf("%+v", out)
		}
	})
	t.Run("a flood keeps the newest bytes", func(t *testing.T) {
		out, err := e.runJournal(t.Context(), helper(t, "flood", nil))
		if err != nil {
			t.Fatal(err)
		}
		if !out.Truncated || len(out.Stdout) != journalStdoutMax {
			t.Fatalf("truncated %v, %d bytes", out.Truncated, len(out.Stdout))
		}
		if !bytes.HasSuffix(out.Stdout, fmt.Appendf(nil, "line %d\n", floodLines)) {
			t.Errorf("the newest line is missing: %q", out.Stdout[len(out.Stdout)-40:])
		}
		if len(out.Stderr) != journalStderrMax {
			t.Errorf("stderr %d bytes", len(out.Stderr))
		}
	})
	t.Run("the deadline kills the group and answers at once", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		started := time.Now()
		_, err := e.runJournal(ctx, helper(t, "ignoreterm", nil))
		if !errors.Is(err, ErrJournalUnavailable) || !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("err = %v", err)
		}
		// Well under the 2 s of each stream's drain (C2), which the reaper
		// finishes alone.
		if d := time.Since(started); d > time.Second {
			t.Errorf("answered after %v", d)
		}
	})
	t.Run("a program that does not start", func(t *testing.T) {
		_, err := e.runJournal(t.Context(), Spec{Path: filepath.Join(t.TempDir(), "absent")})
		if !errors.Is(err, ErrJournalUnavailable) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("a closed executor", func(t *testing.T) {
		closed := newExecutor(t)
		closed.Close()
		_, err := closed.runJournal(t.Context(), helper(t, "lines", nil))
		if !errors.Is(err, ErrJournalUnavailable) {
			t.Errorf("err = %v", err)
		}
	})
}

// Where an identity cannot be changed, the runner says unavailable rather
// than unsupported (criterion 5).
func TestJournalUnsupportedIdentity(t *testing.T) {
	spec := helper(t, "lines", nil)
	spec.Cred = &privilege.Cred{UID: uint32(os.Geteuid()), GID: uint32(os.Getegid())}
	_, attrErr := sysProcAttr(spec.Cred)
	if attrErr == nil {
		t.Skip("this system changes identities")
	}
	_, err := newExecutor(t).runJournal(t.Context(), spec)
	if !errors.Is(err, ErrJournalUnavailable) || !errors.Is(err, ErrUnsupported) {
		t.Errorf("err = %v", err)
	}
}

func TestTailWriter(t *testing.T) {
	tests := []struct {
		name   string
		writes []string
		want   string
		cut    bool
	}{
		{"under the bound", []string{"ab", "cd"}, "abcd", false},
		{"exactly the bound", []string{"abcdefgh"}, "abcdefgh", false},
		{"one big write", []string{"0123456789"}, "23456789", true},
		{"many small writes", []string{"012", "345", "678", "9"}, "23456789", true},
		{"far over the bound, in pieces", []string{"0123456789", "0123456789", "0123456789ab"}, "456789ab", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := tailWriter{max: 8}
			for _, s := range tt.writes {
				if n, err := w.Write([]byte(s)); n != len(s) || err != nil {
					t.Fatalf("write = %d, %v", n, err)
				}
			}
			if got := string(w.bytes()); got != tt.want || w.cut() != tt.cut {
				t.Errorf("%q cut %v, want %q cut %v", got, w.cut(), tt.want, tt.cut)
			}
		})
	}
}

func TestCapWriter(t *testing.T) {
	w := capWriter{max: 4}
	for _, s := range []string{"ab", "cdef", "gh"} {
		if n, err := w.Write([]byte(s)); n != len(s) || err != nil {
			t.Fatalf("write = %d, %v", n, err)
		}
	}
	if string(w.buf) != "abcd" {
		t.Errorf("%q", w.buf)
	}
}
