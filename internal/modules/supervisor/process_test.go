package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/clock"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/state"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/platform/executor"
)

const workerService = "version: 1\nservices:\n  - name: worker\n    command: /bin/sh\n    args: [\"--token\", \"ARGSECRET\"]\n" +
	"    environment: {API_KEY: \"literal-secret-42\", SHORT: ab}\n    startup_grace: 2s\n    stop_signal: SIGINT\n    stop_timeout: 3s\n" +
	"    notifications: [ops]\n"

// readFileState reads the supervisor's state file under dir.
func readFileState(t *testing.T, dir string) fileState {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, moduleName, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		SchemaVersion int       `json:"schema_version"`
		Data          fileState `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || env.SchemaVersion != stateVersion {
		t.Fatalf("state file %s: %v", raw, err)
	}
	return env.Data
}

// AC1, AC3: the process is launched once with the configured spec,
// becomes healthy after startup_grace, and a crash is one service_failed
// with no relaunch, however long one waits.
func TestProcessLifecycle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := mustConfigureProcs(t, map[string]string{"supervisor/a.yaml": workerService}, l)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		if l.launches() != 1 {
			t.Fatalf("%d launches", l.launches())
		}
		spec := l.spec(0)
		if spec.Arg0 != "/bin/sh" || !filepath.IsAbs(spec.Path) || spec.Dir != "/" ||
			strings.Join(spec.Args, " ") != "--token ARGSECRET" ||
			strings.Join(spec.Env, " ") != "API_KEY=literal-secret-42 PATH="+defaultPath+" SHORT=ab" {
			t.Errorf("spec = %+v", spec)
		}
		s := statusOf(t, in, "worker")
		if s.Phase != phaseStarting || s.Pid != 1001 || s.State != "unknown" || s.Starts != 1 || s.StartedAt.IsZero() {
			t.Errorf("starting: %+v", s)
		}
		if g := readFileState(t, r.stateDir).Groups["worker"]; g.Pgid != 1001 || g.Boot != testBoot {
			t.Errorf("group record not saved at launch: %+v", g)
		}
		tick(2 * time.Second)
		if s := statusOf(t, in, "worker"); s.Phase != phaseRunning || s.State != "healthy" {
			t.Errorf("after the grace: %+v", s)
		}
		l.proc(0).end(executor.Exit{Code: 0}, nil) // status 0 is a failure too
		synctest.Wait()
		evs := r.drain()
		if len(evs) != 1 || evs[0].Type != EventServiceFailed || evs[0].Attributes["failure"] != string(FailureExited) ||
			evs[0].Attributes["exit_code"] != 0 || evs[0].Attributes["startup"] != false || evs[0].SourceType != "process" ||
			evs[0].PreviousState != "healthy" {
			t.Fatalf("events = %+v", evs)
		}
		for _, secret := range []string{"ARGSECRET", "literal-secret-42", "/bin/sh"} {
			if strings.Contains(evs[0].Message, secret) || strings.Contains(r.logs.String(), secret) {
				t.Errorf("%q in an event or the log", secret)
			}
		}
		s = statusOf(t, in, "worker")
		if s.Phase != phaseExited || s.State != "failed" || s.Pid != 0 || s.LastExit == nil || s.LastExit.Code != 0 ||
			s.LastFailure != string(FailureExited) || s.IncidentID != evs[0].ID {
			t.Errorf("after the exit: %+v", s)
		}
		if _, ok := readFileState(t, r.stateDir).Groups["worker"]; ok {
			t.Error("the group record outlived the process")
		}
		tick(time.Hour)
		if l.launches() != 1 || len(r.drain()) != 0 {
			t.Errorf("relaunched (%d) or more events: there is no restart in 3b-2", l.launches())
		}
	})
}

// An exit before startup_grace, and an exit by a signal.
func TestProcessExitDuringStartup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := mustConfigureProcs(t, map[string]string{"supervisor/a.yaml": workerService}, l)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		l.proc(0).end(executor.Exit{Code: -1, Signal: "signal 40"}, nil) // outside the named signals
		synctest.Wait()
		evs := r.drain()
		if len(evs) != 1 || evs[0].Attributes["failure"] != string(FailureKilled) || evs[0].Attributes["signal"] != "signal 40" ||
			evs[0].Attributes["startup"] != true || evs[0].PreviousState != "unknown" {
			t.Fatalf("events = %+v", evs)
		}
		// The state file with that signal loads again, not as corrupt.
		r.stop(t)
		in2 := mustConfigureProcs(t, map[string]string{"supervisor/a.yaml": workerService}, &fakeLauncher{})
		r2 := start(t, in2, r.stateDir)
		defer r2.stop(t)
		if strings.Contains(r2.logs.String(), "corrupt") || statusOf(t, in2, "worker").LastExit.Signal != "signal 40" {
			t.Errorf("reload: %s", r2.logs.String())
		}
	})
}

// Start failures are classified; the output sink of a failed launch is
// closed.
func TestProcessStartFailures(t *testing.T) {
	tests := []struct {
		err  error
		want Failure
	}{
		{fs.ErrNotExist, FailureNotFound},
		{fs.ErrPermission, FailurePermission},
		{syscall.EPERM, FailurePermission},
		{executor.ErrUnsupported, FailureUnsupported},
		{errFakeExec, FailureExec},
	}
	for _, tt := range tests {
		t.Run(string(tt.want), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				l := &fakeLauncher{launchErr: tt.err}
				in := mustConfigureProcs(t, map[string]string{"supervisor/a.yaml": workerService}, l)
				r := start(t, in, "")
				defer r.stop(t)
				synctest.Wait()
				evs := r.drain()
				if len(evs) != 1 || evs[0].Attributes["failure"] != string(tt.want) {
					t.Fatalf("events = %+v", evs)
				}
				if s := statusOf(t, in, "worker"); s.Phase != phaseStartFailed || s.State != "failed" {
					t.Errorf("status = %+v", s)
				}
			})
		})
	}
}

// D-084 (1): with the switch off nothing is launched, but the group a
// previous run left is still cleaned up.
func TestProcessNotArmed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stateDir := filepath.Join(t.TempDir(), "state")
		writeStateFile(t, stateDir, `{"services": {}, "groups": {"worker": {"pgid": 4242, "sid": 50, "start": 9, "boot": "`+testBoot+`"}}}`)
		l := &fakeLauncher{}
		in := mustConfigureProcs(t, map[string]string{"sentinel.yaml": centralFile, "supervisor/a.yaml": workerService}, l)
		r := start(t, in, stateDir)
		defer r.stop(t)
		synctest.Wait()
		if l.launches() != 0 {
			t.Fatal("launched without modules.supervisor.processes")
		}
		if c := l.cleaned(); len(c) != 1 || c[0].Pgid != 4242 {
			t.Errorf("cleanups = %+v", c)
		}
		st, _ := in.status("")
		if st.ProcessesArmed || st.Services[0].Phase != phaseNotArmed || st.PendingCleanups != 0 {
			t.Errorf("status = %+v", st)
		}
	})
}

// writeStateFile writes a supervisor state file of the current version.
func writeStateFile(t *testing.T, stateDir, data string) {
	t.Helper()
	dir := filepath.Join(stateDir, moduleName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stateDir, 0o750); err != nil {
		t.Fatal(err)
	}
	doc := `{"schema_version": 2, "data": ` + data + `}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
}

// D-084 (2): a recorded group is cleaned up before the service's launch;
// records of services no longer configured as processes are cleaned up
// too; a failed cleanup keeps the service from starting and the record
// for the next run.
func TestProcessCleanupAtStart(t *testing.T) {
	groups := `"groups": {"worker": {"pgid": 4242, "sid": 50, "start": 9, "boot": "` + testBoot + `"},` +
		` "gone": {"pgid": 4343, "sid": 50, "start": 9, "boot": "` + testBoot + `"},` +
		` "web": {"pgid": 4444, "sid": 50, "start": 9, "boot": "` + testBoot + `"}}`
	files := func() map[string]string {
		return map[string]string{"supervisor/a.yaml": workerService + "  - {name: web, url: \"https://h/\"}\n"}
	}
	t.Run("success", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			stateDir := filepath.Join(t.TempDir(), "state")
			writeStateFile(t, stateDir, `{"services": {}, `+groups+`}`)
			l := &fakeLauncher{}
			r := start(t, mustConfigureProcs(t, files(), l), stateDir)
			defer r.stop(t)
			synctest.Wait()
			got := map[int]bool{}
			for _, g := range l.cleaned() {
				got[g.Pgid] = true
			}
			if len(got) != 3 || !got[4242] || !got[4343] || !got[4444] || l.launches() != 1 {
				t.Errorf("cleanups %v, launches %d", got, l.launches())
			}
			g := readFileState(t, stateDir).Groups
			if len(g) != 1 || g["worker"].Pgid != 1001 {
				t.Errorf("records = %+v: only the new process's", g)
			}
		})
	})
	t.Run("failure", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			stateDir := filepath.Join(t.TempDir(), "state")
			writeStateFile(t, stateDir, `{"services": {}, `+groups+`}`)
			l := &fakeLauncher{cleanupErr: executor.ErrCleanupIncomplete}
			in := mustConfigureProcs(t, files(), l)
			r := start(t, in, stateDir)
			defer r.stop(t)
			synctest.Wait()
			if l.launches() != 0 {
				t.Fatal("launched beside what a previous run left")
			}
			evs := r.drain()
			if len(evs) != 1 || evs[0].Attributes["failure"] != string(FailureCleanup) {
				t.Errorf("events = %+v", evs)
			}
			if g := readFileState(t, stateDir).Groups; len(g) != 3 {
				t.Errorf("records = %+v: all must stay", g)
			}
			if st, _ := in.status(""); st.PendingCleanups != 3 {
				t.Errorf("pending = %d", st.PendingCleanups)
			}
		})
	})
}

// AC5: Stop stops the process with its stop signal and timeout; that is
// not a failure; the record is cleared and the launcher closed.
func TestProcessStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := mustConfigureProcs(t, map[string]string{"supervisor/a.yaml": workerService}, l)
		r := start(t, in, "")
		synctest.Wait()
		r.stop(t)
		if c := l.proc(0).stopCalls(); len(c) != 1 || c[0] != (stopCall{syscall.SIGINT, 3 * time.Second}) {
			t.Errorf("stops = %+v", c)
		}
		if len(r.drain()) != 0 {
			t.Error("a stop by sentineld emitted an event")
		}
		if s := statusOf(t, in, "worker"); s.Phase != phaseStopped || s.State != "unknown" || s.LastFailure != "" {
			t.Errorf("status = %+v", s)
		}
		if g := readFileState(t, r.stateDir).Groups; len(g) != 0 || !l.closed {
			t.Errorf("records %+v, closed %v", g, l.closed)
		}
	})
}

// A process that does not end when stopped makes Stop fail at its
// deadline (the daemon treats the instance as not stopped, D-078) and its
// record stays for the next start.
func TestProcessStopStubborn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := mustConfigureProcs(t, map[string]string{"supervisor/a.yaml": workerService}, l)
		r := start(t, in, "")
		synctest.Wait()
		l.proc(0).stubborn = true
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := in.Stop(ctx); err == nil || !strings.Contains(err.Error(), "1 processes did not end") {
			t.Fatalf("Stop = %v", err)
		}
		if g := readFileState(t, r.stateDir).Groups; g["worker"].Pgid != 1001 {
			t.Errorf("records = %+v", g)
		}
	})
}

// C4: a launch that completes while the module stops is owned and
// stopped like any other.
func TestProcessLaunchDuringStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{gate: make(chan struct{})}
		in := mustConfigureProcs(t, map[string]string{"supervisor/a.yaml": workerService}, l)
		start(t, in, "")
		synctest.Wait() // the loop is in Launch
		stopped := make(chan error)
		go func() { stopped <- in.Stop(context.Background()) }()
		synctest.Wait()
		close(l.gate) // the launch succeeds after the cancellation
		if err := <-stopped; err != nil {
			t.Fatal(err)
		}
		if l.launches() != 1 || len(l.proc(0).stopCalls()) != 1 {
			t.Errorf("launches %d; the late process was not stopped", l.launches())
		}
	})
}

// Q3 (D-086): a relaunched process with an open incident (a previous
// run's crash) recovers after a minute of running, not at startup_grace.
func TestProcessRecovers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		files := map[string]string{"supervisor/a.yaml": workerService}
		in := mustConfigureProcs(t, files, l)
		r := start(t, in, "")
		synctest.Wait()
		l.proc(0).end(executor.Exit{Code: 3}, nil)
		synctest.Wait()
		failed := r.drain()[0]
		r.stop(t)
		l2 := &fakeLauncher{}
		in2 := mustConfigureProcs(t, map[string]string{"supervisor/a.yaml": workerService}, l2)
		r2 := start(t, in2, r.stateDir)
		defer r2.stop(t)
		tick(30 * time.Second)
		if s := statusOf(t, in2, "worker"); s.State != "failed" || len(r2.drain()) != 0 {
			t.Fatalf("recovered before stableAfter: %+v", s)
		}
		tick(30 * time.Second)
		evs := r2.drain()
		if len(evs) != 1 || evs[0].Type != EventServiceRecovered || evs[0].CorrelationID != failed.ID {
			t.Fatalf("events = %+v", evs)
		}
		if s := statusOf(t, in2, "worker"); s.State != "healthy" || s.IncidentID != "" {
			t.Errorf("status = %+v", s)
		}
	})
}

// A crash while an incident is open adds no second service_failed.
func TestProcessCrashDuringIncident(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := mustConfigureProcs(t, map[string]string{"supervisor/a.yaml": workerService}, l)
		r := start(t, in, "")
		synctest.Wait()
		l.proc(0).end(executor.Exit{Code: 3}, nil)
		synctest.Wait()
		incident := r.drain()[0].ID
		r.stop(t)
		l2 := &fakeLauncher{}
		in2 := mustConfigureProcs(t, map[string]string{"supervisor/a.yaml": workerService}, l2)
		r2 := start(t, in2, r.stateDir)
		defer r2.stop(t)
		synctest.Wait()
		l2.proc(0).end(executor.Exit{Code: 4}, nil)
		synctest.Wait()
		if evs := r2.drain(); len(evs) != 0 {
			t.Errorf("events = %+v", evs)
		}
		if s := statusOf(t, in2, "worker"); s.IncidentID != incident || s.Failures != 2 || s.LastExit.Code != 4 {
			t.Errorf("status = %+v", s)
		}
	})
}

// D-086: a process's log output is masked with the configuration's
// secrets and the environment's values (four bytes or more).
func TestProcessOutputRedacted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{output: []string{"key is literal-secret-42", "token tok-9f8e7d here", "short ab stays"}}
		files := map[string]string{"supervisor/a.yaml": workerService +
			"  - name: other\n    command: /bin/sh\n    environment: {T: \"${TOKEN}\"}\n    output: discard\n"}
		in := mustConfigureProcs(t, files, l)
		r := &rig{in: in, stateDir: filepath.Join(t.TempDir(), "state"), logs: &syncBuffer{}}
		dir, err := state.OpenDir(r.stateDir, clock.Real())
		if err != nil {
			t.Fatal(err)
		}
		defer dir.Close()
		rt := runtimeFor(r, dir)
		rt.Redact = func(s string) string { return strings.ReplaceAll(s, "tok-9f8e7d", "[CONFIG]") }
		if err := in.Start(context.Background(), rt); err != nil {
			t.Fatal(err)
		}
		defer r.stop(t)
		synctest.Wait()
		logs := r.logs.String()
		if strings.Contains(logs, "literal-secret-42") || strings.Contains(logs, "tok-9f8e7d") ||
			!strings.Contains(logs, "key is [REDACTED]") || !strings.Contains(logs, "token [CONFIG] here") ||
			!strings.Contains(logs, "short ab stays") || !strings.Contains(logs, "stream=stdout") {
			t.Errorf("logs:\n%s", logs)
		}
		for i := range l.launches() {
			if spec := l.spec(i); slices.Contains(spec.Env, "T=tok-9f8e7d") && spec.Stdout != nil { // "other" discards
				t.Error("discard has a sink")
			}
		}
	})
}

// D-009: an output file that cannot be opened is output_failed, and no
// process starts; one that can is the sink of both streams.
func TestProcessOutputFile(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		svc := func(name, file string) string {
			return "  - name: " + name + "\n    command: /bin/sh\n    output: {file: " + file + "}\n"
		}
		if err := os.Symlink("/etc/passwd", filepath.Join(dir, "planted.log")); err != nil {
			t.Fatal(err)
		}
		l := &fakeLauncher{}
		in := mustConfigureProcs(t, map[string]string{"supervisor/a.yaml": "version: 1\nservices:\n" +
			svc("good", dir+"/good.log") + svc("planted", dir+"/planted.log")}, l)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		if s := statusOf(t, in, "planted"); s.LastFailure != string(FailureOutput) {
			t.Errorf("planted link: %+v", s)
		}
		if l.launches() != 1 || l.spec(0).Stdout == nil || l.spec(0).Stdout != l.spec(0).Stderr {
			t.Errorf("good: %+v", l.specs)
		}
		if info, err := os.Stat(dir + "/good.log"); err != nil || info.Mode().Perm() != 0o640 {
			t.Errorf("good.log: %v %v", info, err)
		}
	})
}

// An output sink that failed is reported once the process ends.
func TestProcessOutputLost(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{}
		in := mustConfigureProcs(t, map[string]string{"supervisor/a.yaml": workerService}, l)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		l.proc(0).end(executor.Exit{Code: 1}, errors.New("disk full"))
		synctest.Wait()
		if s := statusOf(t, in, "worker"); !s.OutputLost {
			t.Errorf("status = %+v", s)
		}
	})
}
