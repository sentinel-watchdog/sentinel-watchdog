package supervisor

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/config"
)

// D-058, D-086: name and command are enough; every default applies.
func TestProcessDefaults(t *testing.T) {
	in := mustConfigureProcs(t, map[string]string{"supervisor/a.yaml": "version: 1\nservices:\n  - name: worker\n    command: /bin/sh\n"}, &fakeLauncher{})
	def := in.byName["worker"].def
	want := Process{Command: "/bin/sh", Args: []string{}, WorkingDir: "/", Env: []string{"PATH=" + defaultPath},
		Output: Output{Kind: outputLog}, StopSignal: syscall.SIGTERM, StopTimeout: 10 * time.Second, StartupGrace: 5 * time.Second}
	if def.Type != typeProcess || def.URL != nil || !equalProcess(def.Process, want) {
		t.Errorf("def = %+v\nwant %+v", def.Process, want)
	}
	if !in.processes || len(in.warnings) != 0 {
		t.Errorf("armed %v, warnings %v", in.processes, in.warnings)
	}
}

func equalProcess(a, b Process) bool {
	return a.Command == b.Command && slices.Equal(a.Args, b.Args) && a.Script == b.Script && a.User == b.User &&
		a.Group == b.Group && (a.Cred == nil) == (b.Cred == nil) && a.WorkingDir == b.WorkingDir &&
		slices.Equal(a.Env, b.Env) && a.Output == b.Output && a.StopSignal == b.StopSignal &&
		a.StopTimeout == b.StopTimeout && a.StartupGrace == b.StartupGrace
}

// Every key of a process service, decoded.
func TestProcessKeys(t *testing.T) {
	logDir := t.TempDir()
	body := "version: 1\nservices:\n" +
		"  - name: worker\n    command: /bin/sh\n    args: [-c, \"exit 0\"]\n    user: worker\n    group: staff\n" +
		"    working_dir: /tmp\n    environment: {QUEUE: jobs, PATH: /opt/bin, API_TOKEN: \"${TOKEN}\"}\n" +
		"    output: {file: " + logDir + "/w.log, mode: \"0600\"}\n    stop_signal: INT\n    stop_timeout: 30s\n    startup_grace: 0s\n" +
		"    notifications: [ops]\n" +
		"  - name: job\n    script: \"echo hi\"\n    output: discard\n" +
		"  - name: job2\n    type: process\n    script: \"echo hi\"\n    shell: /bin/sh\n"
	in := mustConfigureProcs(t, map[string]string{"supervisor/a.yaml": body}, &fakeLauncher{})
	w := in.byName["worker"].def.Process
	if w.Cred == nil || w.Cred.UID != 1001 || w.Cred.GID != 50 || len(w.Cred.Groups) != 0 {
		t.Errorf("cred = %+v", w.Cred)
	}
	wantEnv := []string{"API_TOKEN=tok-9f8e7d", "PATH=/opt/bin", "QUEUE=jobs"}
	if !slices.Equal(w.Env, wantEnv) || !slices.Equal(w.Args, []string{"-c", "exit 0"}) || w.WorkingDir != "/tmp" ||
		w.Output != (Output{Kind: outputFile, File: logDir + "/w.log", Mode: 0o600}) || w.StopSignal != syscall.SIGINT ||
		w.StopTimeout != 30*time.Second || w.StartupGrace != 0 {
		t.Errorf("worker = %+v", w)
	}
	j := in.byName["job"].def.Process
	if !j.Script || j.Command != defaultShell || !slices.Equal(j.Args, []string{"-c", "echo hi"}) || j.Output.Kind != outputDiscard {
		t.Errorf("job = %+v", j)
	}
	if in.byName["job2"].def.Type != typeProcess {
		t.Error("explicit type: process not taken")
	}
}

// Every invalid key is reported with its line, and no problem quotes a
// value: arguments, scripts and environment values may hold secrets.
func TestProcessProblems(t *testing.T) {
	untrusted := filepath.Join(t.TempDir(), "open")
	if err := os.MkdirAll(untrusted, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(untrusted+"/prog", []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(untrusted, 0o777); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, service string
		want          string // in the problem
	}{
		{"ambiguous", "{name: a, url: \"https://h/\", command: /bin/sh}", "ambiguous service"},
		{"neither", "{name: a}", "needs url (an http service) or command or script"},
		{"command and script", "{name: a, command: /bin/sh, script: \"x\"}", "command and script exclude each other"},
		{"relative command", "{name: a, command: sh}", "command must be an absolute path"},
		{"unclean command", "{name: a, command: /bin/../bin/sh}", "must be a clean path"},
		{"shell with command", "{name: a, command: /bin/sh, shell: /bin/sh}", "shell goes with script"},
		{"args with script", "{name: a, script: x, args: [a]}", "args go with command"},
		{"empty script", "{name: a, script: \"\"}", "script is empty"},
		{"relative shell", "{name: a, script: x, shell: sh}", "shell must be an absolute path"},
		{"missing command", "{name: a, command: /nonexistent/prog}", "the program cannot be run"},
		{"untrusted command", "{name: a, command: " + untrusted + "/prog}", "untrusted executable"},
		{"unknown user", "{name: a, command: /bin/sh, user: nobody-here}", "user not found"},
		{"numeric user without group", "{name: a, command: /bin/sh, user: \"1001\"}", "needs a group"},
		{"empty user", "{name: a, command: /bin/sh, user: \"\"}", "must not be empty"},
		{"relative working_dir", "{name: a, command: /bin/sh, working_dir: tmp}", "working_dir must be an absolute path"},
		{"bad env name", "{name: a, command: /bin/sh, environment: {\"1X\": v}}", "invalid variable name"},
		{"NUL in env", "{name: a, command: /bin/sh, environment: {X: \"a\\0b\"}}", "contains a NUL byte"},
		{"bad output", "{name: a, command: /bin/sh, output: journal}", "output must be log, discard"},
		{"integer mode", "{name: a, command: /bin/sh, output: {file: /tmp/x.log, mode: 0640}}", "quoted"},
		{"writable mode", "{name: a, command: /bin/sh, output: {file: /tmp/x.log, mode: \"0666\"}}", "must not be writable"},
		{"relative output", "{name: a, command: /bin/sh, output: {file: x.log}}", "output file must be an absolute path"},
		{"stop signal", "{name: a, command: /bin/sh, stop_signal: SIGSEGV}", "stop_signal must be one of"},
		{"stop timeout", "{name: a, command: /bin/sh, stop_timeout: 10m}", "must be between 1s and 5m0s"},
		{"startup grace", "{name: a, command: /bin/sh, startup_grace: 1h}", "must be between 0s and 10m0s"},
		{"failure_policy", "{name: a, command: /bin/sh, failure_policy: {consecutive_failures: 2}}", "every exit of a process is a failure"},
		{"check_interval", "{name: a, command: /bin/sh, check_interval: 10s}", "check_interval is for checked services"},
		{"recovery", "{name: a, command: /bin/sh, recovery: {}}", "Phase 3b-3"},
		{"limits", "{name: a, command: /bin/sh, limits: {}}", "Phase 3b-6"},
		{"enabled", "{name: a, command: /bin/sh, enabled: false}", "Phase 3b-5"},
		{"http key", "{name: a, command: /bin/sh, method: GET}", "unknown field"},
		{"process key on http", "{name: a, url: \"https://h/\", user: worker}", "unknown field"},
		{"explicit http with command", "{name: a, type: http, command: /bin/sh}", "unknown field"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := "version: 1\nservices:\n  - " + tt.service + "\n"
			_, err := configureProcs(t, map[string]string{"supervisor/a.yaml": body}, &fakeLauncher{})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
			if !strings.Contains(err.Error(), "line ") {
				t.Errorf("no line in %v", err)
			}
		})
	}
}

// No problem quotes an argument, a script or an environment value, even
// when they are invalid (secrets, threat model T-08).
func TestProcessProblemsHideValues(t *testing.T) {
	long := strings.Repeat("s3cr3t", maxArgBytes)
	body := "version: 1\nservices:\n  - name: a\n    command: /bin/sh\n    args: [\"" + long + "\", \"x\\0SECRETARG\"]\n" +
		"    environment: {TOKEN: \"" + strings.Repeat("v", maxEnvValue+1) + "\", OTHER: \"y\\0SECRETENV\", \"bad name\": \"${TOKEN}\"}\n" +
		"  - name: b\n    script: \"x\\0SECRETSCRIPT\"\n"
	_, err := configureProcs(t, map[string]string{"supervisor/a.yaml": body}, &fakeLauncher{})
	if err == nil {
		t.Fatal("accepted")
	}
	for _, secret := range []string{"s3cr3t", "SECRETARG", "SECRETENV", "SECRETSCRIPT", "tok-9f8e7d", "vvvv"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("a problem quotes %q: %v", secret, err)
		}
	}
}

// D-084 (1): without the central switch, process services are validated
// and a warning says they will not run; the switch is the only central key.
func TestProcessSwitch(t *testing.T) {
	files := map[string]string{"sentinel.yaml": centralFile,
		"supervisor/a.yaml": "version: 1\nservices:\n  - {name: a, command: /bin/sh}\n  - {name: b, script: x}\n"}
	in := mustConfigureProcs(t, files, &fakeLauncher{})
	if in.processes || len(in.warnings) != 1 || !strings.Contains(in.warnings[0].Message, "2 process services are configured but modules.supervisor.processes is false") {
		t.Errorf("armed %v, warnings %+v", in.processes, in.warnings)
	}
	files = map[string]string{"sentinel.yaml": centralFile + "    processes: true\n    mode: enforce\n",
		"supervisor/a.yaml": "version: 1\nservices:\n  - {name: a, command: /bin/sh}\n"}
	if _, err := configureProcs(t, files, &fakeLauncher{}); err == nil || !strings.Contains(err.Error(), "mode") {
		t.Errorf("unknown central key: %v", err)
	}
	var verr *config.ValidationError
	files = map[string]string{"sentinel.yaml": centralFile + "    processes: true\n",
		"supervisor/a.yaml": "version: 1\nservices:\n  - {name: a, command: relative}\n"}
	if _, err := configureProcs(t, files, &fakeLauncher{}); err == nil || !asValidation(err, &verr) {
		t.Errorf("an armed switch must not skip validation: %v", err)
	}
}
