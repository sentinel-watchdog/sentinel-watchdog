package supervisor

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/clock"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/platform/executor"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/platform/systemd"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/api"
)

// logsRig is a started supervisor whose journal answers through journal.
type logsRig struct {
	in  *instance
	l   *fakeLauncher
	fu  *fakeUnits
	dir string
	r   *rig
}

func logsService(t *testing.T, services string, fu *fakeUnits, l *fakeLauncher) *logsRig {
	t.Helper()
	return logsServiceWith(t, services, fu, l, nil)
}

// logsServiceWith is logsService with journal as the journal sender, as
// under systemd: log output is then read from the journal (D-100).
func logsServiceWith(t *testing.T, services string, fu *fakeUnits, l *fakeLauncher, journal func([]byte) error) *logsRig {
	t.Helper()
	dir := t.TempDir()
	body := "version: 1\nservices:\n" + strings.ReplaceAll(services, "DIR", dir)
	files := map[string]string{"sentinel.yaml": armedCentral, "supervisor/a.yaml": body}
	mc, err := moduleConfig(t, files, nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(Options{NewChecker: newFakes().build, Lookup: fakeLookup,
		NewLauncher:   func(clock.Clock) (launcher, error) { return l, nil },
		NewUnitClient: func() unitClient { return fu },
		BootID:        func() (string, error) { return testBoot, nil },
		JournalSend:   journal}).Configure(mc)
	if err != nil {
		t.Fatal(err)
	}
	in := c.(*instance)
	return &logsRig{in: in, l: l, fu: fu, dir: dir, r: start(t, in, "")}
}

func (r *logsRig) logs(t *testing.T, args string) (api.SupervisorLogsResult, *api.Error) {
	t.Helper()
	res, apiErr := r.in.Handle(context.Background(), api.SupervisorLogs.Name, jsontext.Value(args))
	if apiErr != nil {
		return api.SupervisorLogsResult{}, apiErr
	}
	out, ok := res.(api.SupervisorLogsResult)
	if !ok {
		t.Fatalf("result %T", res)
	}
	return out, nil
}

func journalLine(sec int, message string) string {
	return fmt.Sprintf(`{"__REALTIME_TIMESTAMP":"%d000000","MESSAGE":%q}`, sec, message)
}

func journalAnswers(lines ...string) func(executor.JournalQuery) (executor.JournalOutput, error) {
	return func(executor.JournalQuery) (executor.JournalOutput, error) {
		return executor.JournalOutput{Stdout: []byte(strings.Join(lines, "\n") + "\n")}, nil
	}
}

func lineTexts(r api.SupervisorLogsResult) []string {
	out := make([]string, len(r.Lines))
	for i, l := range r.Lines {
		out[i] = l.Text
	}
	return out
}

const logsServices = `  - {name: web, unit: nginx.service, check_interval: 10s}
  - {name: filed, command: /bin/sh, environment: {API_KEY: literal-secret-42}, output: {file: DIR/filed.log}}
  - {name: logged, command: /bin/sh}
  - {name: quiet, command: /bin/sh, output: discard}
  - {name: site, url: "https://site.example.org/"}
`

func TestLogsOfAUnit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fu := &fakeUnits{}
		l := &fakeLauncher{journal: journalAnswers(journalLine(1, "one"), journalLine(2, "token literal-secret-42 here"))}
		r := logsService(t, logsServices, fu, l)
		defer r.r.stop(t)
		synctest.Wait()
		got, apiErr := r.logs(t, `{"name":"web","limit":7}`)
		if apiErr != nil {
			t.Fatal(apiErr)
		}
		if got.Name != "web" || got.Source != api.LogSourceJournal || got.Truncated {
			t.Errorf("%+v", got)
		}
		// A systemd unit's journal may quote what a process service was
		// given: the whole redaction applies.
		if !slices.Equal(lineTexts(got), []string{"one", "token [REDACTED] here"}) {
			t.Errorf("%q", lineTexts(got))
		}
		if got.Lines[1].Time.Unix() != 2 {
			t.Errorf("time %v", got.Lines[1].Time)
		}
		if q := l.queries(); !slices.Equal(q, []executor.JournalQuery{{Unit: "nginx.service", Lines: 7}}) {
			t.Errorf("queries %+v", q)
		}
	})
}

func TestLogsDefaultLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{journal: journalAnswers(journalLine(1, "one"))}
		r := logsService(t, logsServices, &fakeUnits{}, l)
		defer r.r.stop(t)
		synctest.Wait()
		if _, apiErr := r.logs(t, `{"name":"web"}`); apiErr != nil {
			t.Fatal(apiErr)
		}
		if q := l.queries(); len(q) != 1 || q[0].Lines != api.DefaultLogLines {
			t.Errorf("queries %+v", q)
		}
	})
}

// The journal records a unit's canonical name, and --unit does not resolve
// an alias (measured): the query uses the Id the last observation read.
func TestLogsOfAnAlias(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fu := &fakeUnits{}
		fu.set(func(context.Context) (systemd.UnitState, error) {
			st := runningUnit('a', 0)
			st.ID = "canonical.service"
			return st, nil
		})
		l := &fakeLauncher{journal: journalAnswers(journalLine(1, "one"))}
		r := logsService(t, "  - {name: web, unit: alias.service, check_interval: 10s}\n", fu, l)
		defer r.r.stop(t)
		synctest.Wait()
		if _, apiErr := r.logs(t, `{"name":"web"}`); apiErr != nil {
			t.Fatal(apiErr)
		}
		if q := l.queries(); len(q) != 1 || q[0].Unit != "canonical.service" {
			t.Errorf("queries %+v", q)
		}
	})
}

// A disabled service is never observed, and may be an alias (Q7).
func TestLogsOfADisabledUnit(t *testing.T) {
	const disabled = "  - {name: web, unit: alias.service, enabled: false}\n"
	t.Run("the Id is read for the request", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			fu := &fakeUnits{}
			fu.set(func(context.Context) (systemd.UnitState, error) {
				st := runningUnit('a', 0)
				st.ID = "canonical.service"
				return st, nil
			})
			l := &fakeLauncher{journal: journalAnswers(journalLine(1, "one"))}
			r := logsService(t, disabled, fu, l)
			defer r.r.stop(t)
			synctest.Wait()
			if fu.count() != 0 {
				t.Fatalf("a disabled service was observed %d times", fu.count())
			}
			got, apiErr := r.logs(t, `{"name":"web"}`)
			if apiErr != nil || !slices.Equal(lineTexts(got), []string{"one"}) {
				t.Fatalf("%+v, %v", got, apiErr)
			}
			if q := l.queries(); len(q) != 1 || q[0].Unit != "canonical.service" {
				t.Errorf("queries %+v", q)
			}
		})
	})
	t.Run("without the Id there is no answer, never an empty one", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			fu := &fakeUnits{}
			fu.set(func(context.Context) (systemd.UnitState, error) { return systemd.UnitState{}, systemd.ErrUnavailable })
			l := &fakeLauncher{journal: journalAnswers(journalLine(1, "one"))}
			r := logsService(t, disabled, fu, l)
			defer r.r.stop(t)
			synctest.Wait()
			if _, apiErr := r.logs(t, `{"name":"web"}`); apiErr == nil || apiErr.Code != api.CodeUnavailable {
				t.Fatalf("error %v", apiErr)
			}
			if q := l.queries(); len(q) != 0 {
				t.Errorf("queries %+v", q)
			}
		})
	})
	t.Run("a read that does not answer is bounded", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			fu := &fakeUnits{}
			fu.set(func(ctx context.Context) (systemd.UnitState, error) {
				<-ctx.Done()
				return systemd.UnitState{}, ctx.Err()
			})
			r := logsService(t, disabled, fu, &fakeLauncher{})
			defer r.r.stop(t)
			synctest.Wait()
			if _, apiErr := r.logs(t, `{"name":"web"}`); apiErr == nil || apiErr.Code != api.CodeUnavailable {
				t.Fatalf("error %v", apiErr)
			}
		})
	})
}

func TestLogsOfAFile(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		r := logsService(t, logsServices, &fakeUnits{}, l)
		defer r.r.stop(t)
		synctest.Wait()
		// Written raw (process.go): redaction happens here.
		path := filepath.Join(r.dir, "filed.log")
		if err := os.WriteFile(path, []byte("one\nkey literal-secret-42\nthree\n"), 0o640); err != nil {
			t.Fatal(err)
		}
		got, apiErr := r.logs(t, `{"name":"filed","limit":2}`)
		if apiErr != nil {
			t.Fatal(apiErr)
		}
		if got.Source != api.LogSourceFile || !slices.Equal(lineTexts(got), []string{"key [REDACTED]", "three"}) {
			t.Errorf("%+v", got)
		}
		if !got.Lines[0].Time.IsZero() {
			t.Errorf("a file has no times: %v", got.Lines[0].Time)
		}
		if q := l.queries(); len(q) != 0 {
			t.Errorf("a file needs no journal: %+v", q)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("/etc/passwd", path); err != nil {
			t.Fatal(err)
		}
		if _, apiErr := r.logs(t, `{"name":"filed"}`); apiErr == nil || apiErr.Code != api.CodeUnavailable ||
			strings.Contains(apiErr.Message, r.dir) {
			t.Errorf("a link: %v", apiErr)
		}
	})
}

func TestLogsOfAJob(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		body := "version: 1\njobs:\n  - {name: nightly, schedule: \"0 0 * * *\", command: /bin/sh, output: {file: " + dir + "/job.log}}\n" +
			"  - {name: chatty, schedule: \"0 0 * * *\", command: /bin/sh}\n  - {name: mute, schedule: \"0 0 * * *\", command: /bin/sh, output: discard}\n"
		l := &fakeLauncher{}
		in := mustConfigureProcs(t, map[string]string{"supervisor/a.yaml": body}, l)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		if err := os.WriteFile(filepath.Join(dir, "job.log"), []byte("done\n"), 0o640); err != nil {
			t.Fatal(err)
		}
		rig := &logsRig{in: in}
		got, apiErr := rig.logs(t, `{"name":"nightly"}`)
		if apiErr != nil || got.Source != api.LogSourceFile || !slices.Equal(lineTexts(got), []string{"done"}) {
			t.Errorf("%+v, %v", got, apiErr)
		}
		if _, apiErr := rig.logs(t, `{"name":"chatty"}`); apiErr == nil || apiErr.Code != api.CodeUnavailable {
			t.Errorf("chatty, log output without a journal: %v", apiErr)
		}
		if _, apiErr := rig.logs(t, `{"name":"mute"}`); apiErr == nil || apiErr.Code != api.CodeUnsupported {
			t.Errorf("mute: %v", apiErr)
		}
	})
}

// Nothing a request holds reaches journalctl but a configured unit: a name
// that is refused, or unknown, or has no journal, runs nothing (criterion 4).
func TestLogsRefused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{journal: journalAnswers(journalLine(1, "one"))}
		r := logsService(t, logsServices, &fakeUnits{}, l)
		defer r.r.stop(t)
		synctest.Wait()
		tests := []struct {
			name string
			args string
			code api.Code
		}{
			{"an option as name", `{"name":"--grep=x"}`, api.CodeBadRequest},
			{"a path as name", `{"name":"../shadow"}`, api.CodeBadRequest},
			{"no name", `{}`, api.CodeBadRequest},
			{"no arguments", ``, api.CodeBadRequest},
			{"another argument", `{"name":"web","unit":"sshd.service"}`, api.CodeBadRequest},
			{"not JSON", `{`, api.CodeBadRequest},
			{"unknown", `{"name":"nosuch"}`, api.CodeBadRequest},
			{"limit zero", `{"name":"web","limit":0}`, api.CodeBadRequest},
			{"limit negative", `{"name":"web","limit":-1}`, api.CodeBadRequest},
			{"limit too big", `{"name":"web","limit":1001}`, api.CodeBadRequest},
			{"an http service", `{"name":"site"}`, api.CodeUnsupported},
			{"discarded output", `{"name":"quiet"}`, api.CodeUnsupported},
			{"log output without a journal", `{"name":"logged"}`, api.CodeUnavailable},
		}
		for _, tt := range tests {
			if _, apiErr := r.logs(t, tt.args); apiErr == nil || apiErr.Code != tt.code {
				t.Errorf("%s: error %+v, want %s", tt.name, apiErr, tt.code)
			}
		}
		if q := l.queries(); len(q) != 0 {
			t.Errorf("queries %+v", q)
		}
	})
}

func sourceLine(sec int, message, runID string) string {
	return fmt.Sprintf(`{"__REALTIME_TIMESTAMP":"%d000000","MESSAGE":%q,"SENTINEL_RUN_ID":%q}`, sec, message, runID)
}

// Criterion 14: log output sent to the journal is read back by the source's
// name, with each job line's run id; the redaction applies again.
func TestLogsOfLogOutput(t *testing.T) {
	const runA = "0123456789abcdef0123456789abcdef"
	services := logsServices + "jobs:\n  - {name: nightly, schedule: \"0 0 * * *\", command: /bin/sh}\n"
	tests := []struct {
		name   string
		answer []string
		want   []api.LogLine
		wantQ  executor.JournalQuery
	}{
		{"a service", []string{journalLine(1, "one"), journalLine(2, "token literal-secret-42 here")},
			[]api.LogLine{{Time: time.Unix(1, 0).UTC(), Text: "one"}, {Time: time.Unix(2, 0).UTC(), Text: "token [REDACTED] here"}},
			executor.JournalQuery{Source: "logged", Lines: 9}},
		{"a job", []string{sourceLine(3, "done", runA)},
			[]api.LogLine{{Time: time.Unix(3, 0).UTC(), Text: "done", RunID: runA}},
			executor.JournalQuery{Source: "nightly", Lines: 9}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				l := &fakeLauncher{journal: journalAnswers(tc.answer...)}
				r := logsServiceWith(t, services, &fakeUnits{}, l, (&fakeJournal{}).send)
				defer r.r.stop(t)
				synctest.Wait()
				got, apiErr := r.logs(t, `{"name":"`+tc.wantQ.Source+`","limit":9}`)
				if apiErr != nil {
					t.Fatal(apiErr)
				}
				if got.Source != api.LogSourceJournal || !slices.Equal(got.Lines, tc.want) {
					t.Errorf("%+v", got)
				}
				if q := l.queries(); !slices.Equal(q, []executor.JournalQuery{tc.wantQ}) {
					t.Errorf("queries %+v", q)
				}
			})
		})
	}
}

func TestLogsUnavailable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{journal: func(executor.JournalQuery) (executor.JournalOutput, error) {
			return executor.JournalOutput{}, fmt.Errorf("%w: /secret/path", executor.ErrJournalUnavailable)
		}}
		r := logsService(t, logsServices, &fakeUnits{}, l)
		defer r.r.stop(t)
		synctest.Wait()
		_, apiErr := r.logs(t, `{"name":"web"}`)
		if apiErr == nil || apiErr.Code != api.CodeUnavailable || strings.Contains(apiErr.Message, "/secret/path") {
			t.Errorf("error %+v", apiErr)
		}
	})
}

// A launcher that cannot run the journal leaves files readable.
func TestLogsWithoutARunner(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := logsService(t, logsServices, &fakeUnits{}, &fakeLauncher{noJournal: true})
		defer r.r.stop(t)
		synctest.Wait()
		if _, apiErr := r.logs(t, `{"name":"web"}`); apiErr == nil || apiErr.Code != api.CodeUnavailable {
			t.Errorf("error %+v", apiErr)
		}
		if got, apiErr := r.logs(t, `{"name":"filed"}`); apiErr != nil || got.Lines == nil {
			t.Errorf("%+v, %v", got, apiErr)
		}
	})
}

// Q3: a multi-line secret is masked line by line, because what is written
// line by line never holds the whole value.
func TestProcessRedactorMasksLinesOfASecret(t *testing.T) {
	const key = "-----BEGIN KEY-----\r\nbase64body0123\nno\n-----END KEY-----"
	r := processRedactor([]string{"cfg-first\ncfg-second"}, []*service{{def: Service{Process: Process{Env: []string{"KEY=" + key}}}}}, nil)
	tests := []struct{ in, want string }{
		{key, "[REDACTED]"},
		{"a -----BEGIN KEY----- b", "a [REDACTED] b"},
		{"a base64body0123 b", "a [REDACTED] b"},
		{"-----END KEY-----", "[REDACTED]"},
		{"cfg-second", "[REDACTED]"},
		{"under the floor: no", "under the floor: no"},
	}
	for _, tt := range tests {
		if got := r(tt.in); got != tt.want {
			t.Errorf("r(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// Review of 3c-5a's plan, C4 and R2, on the real path: the read starts inside
// a multi-line secret, and what is left of it must not show.
func TestLogsOfAFileCutInsideASecret(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		service := "  - name: filed\n    command: /bin/sh\n    environment: {KEY: \"head-of-secret\\nbody-of-secret\\nfoot-of-secret\"}\n    output: {file: DIR/filed.log}\n"
		r := logsService(t, service, &fakeUnits{}, &fakeLauncher{})
		defer r.r.stop(t)
		synctest.Wait()
		// One long line, so that the secret's lines stay within the bounds
		// of the answer. The read of the last 1 MiB starts on "head".
		pad := strings.Repeat("p", 1<<20-len("-of-secret\nbody-of-secret\nfoot-of-secret\n")-1) + "\n"
		content := "older\nhead-of-secret\nbody-of-secret\nfoot-of-secret\n" + pad
		if err := os.WriteFile(filepath.Join(r.dir, "filed.log"), []byte(content), 0o640); err != nil {
			t.Fatal(err)
		}
		got, apiErr := r.logs(t, `{"name":"filed"}`)
		if apiErr != nil {
			t.Fatal(apiErr)
		}
		if len(got.Lines) != 3 {
			t.Fatalf("%d lines: the test does not cut where it means to", len(got.Lines))
		}
		for _, line := range got.Lines {
			if strings.Contains(line.Text, "of-secret") {
				t.Errorf("secret shown: %q", line.Text[:min(40, len(line.Text))])
			}
		}
	})
}

// Regression: log output is redacted line by line, so a multi-line secret
// was written whole to sentineld's log (found planning 3c-5a).
func TestProcessOutputRedactsLinesOfASecret(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{output: []string{"head-of-secret", "body-of-secret"}}
		service := "version: 1\nservices:\n  - name: w\n    command: /bin/sh\n    environment: {KEY: \"head-of-secret\\nbody-of-secret\"}\n"
		in := mustConfigureProcs(t, map[string]string{"supervisor/a.yaml": service}, l)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		if logs := r.logs.String(); strings.Contains(logs, "of-secret") {
			t.Errorf("logs:\n%s", logs)
		}
	})
}

// D-093 (5), Q2: redaction leaves values under 4 bytes alone, so the ones
// likely to be secrets are reported; the value is never printed.
func TestShortSecretWarning(t *testing.T) {
	tests := []struct {
		name string
		env  string
		warn string
	}{
		{"a sensitive key", `{API_TOKEN: "x9z"}`, "environment.API_TOKEN"},
		{"another", `{DB_PASSWORD: "x9z"}`, "environment.DB_PASSWORD"},
		{"a harmless key", `{DEBUG: "x9z"}`, ""},
		{"long enough", `{API_TOKEN: "x9zk"}`, ""},
		{"empty", `{API_TOKEN: ""}`, ""},
	}
	for _, tt := range tests {
		for _, shape := range []string{"services", "jobs"} {
			t.Run(tt.name+" in "+shape, func(t *testing.T) {
				body := "version: 1\nservices:\n  - {name: p, command: /bin/sh, environment: " + tt.env + "}\n"
				if shape == "jobs" {
					body = "version: 1\njobs:\n  - {name: p, schedule: \"0 0 * * *\", command: /bin/sh, environment: " + tt.env + "}\n"
				}
				in := mustConfigureProcs(t, map[string]string{"supervisor/a.yaml": body}, &fakeLauncher{})
				var hits []string
				for _, w := range in.Warnings() {
					if strings.Contains(w.Message, "x9z") || strings.Contains(w.Path, "x9z") {
						t.Errorf("the value is printed: %+v", w)
					}
					if strings.Contains(w.Message, "shorter than") {
						hits = append(hits, w.Path)
					}
				}
				if (tt.warn == "") != (len(hits) == 0) || (tt.warn != "" && !strings.Contains(strings.Join(hits, " "), tt.warn)) {
					t.Errorf("warnings %+v, want %q", in.Warnings(), tt.warn)
				}
			})
		}
	}
}

// A read that lasts to the end of the exchange must still get its answer out:
// the connection's deadline is the same instant (review 3c-5a, code-review-2 R1).
func TestLogsAnswerBeforeTheExchangeEnds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fu := &fakeUnits{}
		l := &fakeLauncher{journalBlocks: true}
		r := logsService(t, logsServices, fu, l)
		defer r.r.stop(t)
		synctest.Wait()
		ctx, cancel := context.WithTimeout(context.Background(), api.SupervisorLogs.Timeout)
		defer cancel()
		deadline, _ := ctx.Deadline()
		_, apiErr := r.in.Handle(ctx, api.SupervisorLogs.Name, jsontext.Value(`{"name":"web"}`))
		if apiErr == nil || apiErr.Code != api.CodeUnavailable {
			t.Fatalf("error %+v", apiErr)
		}
		if left := time.Until(deadline); left < 500*time.Millisecond {
			t.Errorf("answered %v before the exchange ends", left)
		}
	})
}

// Criterion 8 at the module: a fifth read is refused at once, and says why.
func TestLogsFifthReadIsRefused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{journalBlocks: true}
		r := logsService(t, logsServices, &fakeUnits{}, l)
		defer r.r.stop(t)
		synctest.Wait()
		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		for range 4 {
			wg.Go(func() { _, _ = r.in.Handle(ctx, api.SupervisorLogs.Name, jsontext.Value(`{"name":"web"}`)) })
		}
		synctest.Wait()
		_, apiErr := r.logs(t, `{"name":"web"}`)
		cancel()
		wg.Wait()
		if apiErr == nil || apiErr.Code != api.CodeUnavailable || !strings.Contains(apiErr.Message, "too many") {
			t.Errorf("error %+v", apiErr)
		}
	})
}
