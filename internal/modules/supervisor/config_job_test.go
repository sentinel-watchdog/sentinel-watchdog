package supervisor

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A job needs a name, a schedule and a program; every other key has a
// default (D-058, brief 3c-4 Q11): the process defaults, timeout 1h,
// forbid, skip, the daemon's time zone.
func TestJobDefaults(t *testing.T) {
	in := mustConfigureProcs(t, map[string]string{"supervisor/a.yaml": "version: 1\njobs:\n  - name: backup\n    schedule: \"30 2 * * *\"\n    command: /bin/sh\n"}, &fakeLauncher{})
	j, ok := in.jobByName["backup"]
	if !ok {
		t.Fatalf("no job backup in %v", in.jobByName)
	}
	def := j.def
	want := Process{Command: "/bin/sh", Args: []string{}, WorkingDir: "/", Env: []string{"PATH=" + defaultPath},
		Output: Output{Kind: outputLog}, StopSignal: syscall.SIGTERM, StopTimeout: 10 * time.Second}
	if !equalProcess(def.Program, want) {
		t.Errorf("program = %+v\nwant %+v", def.Program, want)
	}
	if def.Expr != "30 2 * * *" || def.Schedule == nil || def.Timeout != time.Hour || def.Concurrency != concurrencyForbid ||
		def.Missed != missedSkip || def.TZ != "" || def.Location != time.Local || def.Disabled { // "Local": its name is "UTC" without /etc/localtime
		t.Errorf("job = %+v", def)
	}
	if len(in.services) != 0 || len(in.warnings) != 0 {
		t.Errorf("services %d, warnings %v", len(in.services), in.warnings)
	}
}

// Every key of a job, decoded; the time zone is the job's own or the
// daemon's.
func TestJobKeys(t *testing.T) {
	body := "version: 1\njobs:\n" +
		"  - name: report\n    type: cron\n    schedule: \"@daily\"\n    timezone: America/New_York\n    timeout: 30s\n" +
		"    concurrency_policy: forbid\n    missed_runs: skip\n    script: \"echo hi\"\n    user: worker\n    group: staff\n" +
		"    environment: {API_TOKEN: \"${TOKEN}\"}\n    output: discard\n    stop_signal: INT\n    stop_timeout: 3s\n" +
		"    notifications: {channels: [ops], events: [job_failed]}\n    enabled: false\n" +
		"  - {name: inherit, schedule: \"0 * * * *\", command: /bin/sh}\n"
	central := strings.Replace(armedCentral, "version: 1\n", "version: 1\ndaemon: {timezone: Europe/Rome}\n", 1)
	in := mustConfigureProcs(t, map[string]string{"sentinel.yaml": central, "supervisor/a.yaml": body}, &fakeLauncher{})
	r := in.jobByName["report"].def
	if r.TZ != "America/New_York" || r.Location.String() != "America/New_York" || r.Timeout != 30*time.Second ||
		!r.Disabled || !r.Program.Script || r.Program.Output.Kind != outputDiscard || r.Program.StopSignal != syscall.SIGINT ||
		r.Program.StopTimeout != 3*time.Second || r.Program.Cred == nil || r.Program.Cred.UID != 1001 ||
		!slices.Contains(r.Program.Env, "API_TOKEN=tok-9f8e7d") || len(r.Route.Channels) != 1 {
		t.Errorf("report = %+v", r)
	}
	if i := in.jobByName["inherit"].def; i.TZ != "" || i.Location.String() != "Europe/Rome" {
		t.Errorf("inherited zone: TZ %q, location %s", i.TZ, i.Location)
	}
}

// Every invalid job is reported with its line and path.
func TestJobProblems(t *testing.T) {
	tests := []struct {
		name, job, want string
	}{
		{"no schedule", "{name: a, command: /bin/sh}", "jobs[0].schedule: line 3: a job needs a schedule"},
		{"bad schedule", "{name: a, schedule: \"61 * * * *\", command: /bin/sh}", "schedule is not a valid cron expression"},
		{"impossible schedule", "{name: a, schedule: \"0 0 30 2 *\", command: /bin/sh}", "schedule can never run"},
		{"no program", "{name: a, schedule: \"@daily\"}", "a job needs command or script"},
		{"bad zone", "{name: a, schedule: \"@daily\", timezone: Mars/Olympus, command: /bin/sh}", "jobs[0].timezone: line 3: unknown time zone"},
		{"empty zone", "{name: a, schedule: \"@daily\", timezone: \"\", command: /bin/sh}", "unknown time zone"},
		{"timeout too short", "{name: a, schedule: \"@daily\", timeout: 500ms, command: /bin/sh}", "must be between 1s and 24h0m0s"},
		{"timeout too long", "{name: a, schedule: \"@daily\", timeout: 25h, command: /bin/sh}", "must be between 1s and 24h0m0s"},
		{"bad policy", "{name: a, schedule: \"@daily\", concurrency_policy: queue, command: /bin/sh}", "jobs[0].concurrency_policy: line 3: concurrency_policy must be one of forbid, allow, replace"},
		{"bad missed", "{name: a, schedule: \"@daily\", missed_runs: all, command: /bin/sh}", "jobs[0].missed_runs: line 3: missed_runs must be one of skip, run_once"},
		{"other type", "{name: a, type: process, schedule: \"@daily\", command: /bin/sh}", "jobs[0].type: line 3: a job's type is cron"},
		{"recovery", "{name: a, schedule: \"@daily\", command: /bin/sh, recovery: {max_attempts: 2}}", "jobs[0].recovery: line 3: jobs are not restarted"},
		{"failure_policy", "{name: a, schedule: \"@daily\", command: /bin/sh, failure_policy: {consecutive_failures: 2}}", "jobs[0].failure_policy"},
		{"startup_grace", "{name: a, schedule: \"@daily\", command: /bin/sh, startup_grace: 5s}", "jobs[0].startup_grace"},
		{"limits", "{name: a, schedule: \"@daily\", command: /bin/sh, limits: {memory: 64MiB}}", "jobs[0].limits"},
		{"check_interval", "{name: a, schedule: \"@daily\", command: /bin/sh, check_interval: 10s}", "jobs[0].check_interval"},
		{"restart", "{name: a, schedule: \"@daily\", command: /bin/sh, restart: always}", "jobs[0].restart"},
		{"url", "{name: a, schedule: \"@daily\", url: \"https://h/\"}", "jobs[0].url"},
		{"unit", "{name: a, schedule: \"@daily\", unit: x.service}", "jobs[0].unit"},
		{"unknown key", "{name: a, schedule: \"@daily\", command: /bin/sh, colour: red}", "unknown field"},
		{"bad name", "{name: A!, schedule: \"@daily\", command: /bin/sh}", "jobs[0].name"},
		{"process check", "{name: a, schedule: \"@daily\", command: sh}", "command must be an absolute path"},
		{"route event", "{name: a, schedule: \"@daily\", command: /bin/sh, notifications: {channels: [ops], events: [service_failed]}}", "not an event type this emitter produces"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := "version: 1\njobs:\n  - " + tt.job + "\n"
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

// Problems never quote a schedule: it may hold an expanded secret, and
// the cron parser's own messages quote what they reject (review C9).
func TestJobScheduleProblemsQuoteNothing(t *testing.T) {
	for _, schedule := range []string{"*/${TOKEN} * * * *", "${TOKEN} * * * *", "@${TOKEN}", "0 0 ${TOKEN}", "1-${TOKEN} * * * *"} {
		body := fmt.Sprintf("version: 1\njobs:\n  - {name: a, schedule: %q, command: /bin/sh}\n", schedule)
		_, err := configureProcs(t, map[string]string{"supervisor/a.yaml": body}, &fakeLauncher{})
		if err == nil {
			t.Fatalf("%s accepted", schedule)
		}
		if strings.Contains(err.Error(), "tok-9f8e7d") {
			t.Errorf("%s: the problem quotes the secret: %v", schedule, err)
		}
	}
}

// Services and jobs share one name space and one cap (Q2, Q17).
func TestJobNamesAndCap(t *testing.T) {
	body := "version: 1\nservices:\n  - {name: same, command: /bin/sh}\njobs:\n  - {name: same, schedule: \"@daily\", command: /bin/sh}\n"
	_, err := configureProcs(t, map[string]string{"supervisor/a.yaml": body}, &fakeLauncher{})
	if err == nil || !strings.Contains(err.Error(), "jobs[0].name") || !strings.Contains(err.Error(), "already used") {
		t.Errorf("shared name: err = %v", err)
	}
	var b strings.Builder
	b.WriteString("version: 1\nservices:\n")
	for i := range maxServices / 2 {
		fmt.Fprintf(&b, "  - {name: s%d, command: /bin/sh}\n", i)
	}
	b.WriteString("jobs:\n")
	for i := range maxServices/2 + 1 {
		fmt.Fprintf(&b, "  - {name: j%d, schedule: \"@daily\", command: /bin/sh}\n", i)
	}
	_, err = configureProcs(t, map[string]string{"supervisor/a.yaml": b.String()}, &fakeLauncher{})
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("more than %d services and jobs", maxServices)) {
		t.Errorf("cap: err = %v", err)
	}
}

// Jobs run programs: like process services they need the central
// switch, and the configuration says so (AC2).
func TestJobsNotArmedWarning(t *testing.T) {
	body := "version: 1\njobs:\n  - {name: a, schedule: \"@daily\", command: /bin/sh}\n"
	in := mustConfigureProcs(t, map[string]string{"sentinel.yaml": centralFile, "supervisor/a.yaml": body}, &fakeLauncher{})
	if len(in.warnings) != 1 || !strings.Contains(in.warnings[0].Message, "1 jobs are configured but modules.supervisor.processes is false") {
		t.Errorf("warnings = %v", in.warnings)
	}
}

// AC1: every policy is accepted and reaches the definition.
func TestJobPolicies(t *testing.T) {
	tests := []struct{ keys, concurrency, missed string }{
		{"", concurrencyForbid, missedSkip},
		{", concurrency_policy: forbid, missed_runs: skip", concurrencyForbid, missedSkip},
		{", concurrency_policy: allow", concurrencyAllow, missedSkip},
		{", concurrency_policy: replace, missed_runs: run_once", concurrencyReplace, missedRunOnce},
	}
	for _, tt := range tests {
		t.Run(tt.concurrency+"/"+tt.missed, func(t *testing.T) {
			body := "version: 1\njobs:\n  - {name: a, schedule: \"@daily\", command: /bin/sh" + tt.keys + "}\n"
			in := mustConfigureProcs(t, map[string]string{"supervisor/a.yaml": body}, &fakeLauncher{})
			if d := in.jobByName["a"].def; d.Concurrency != tt.concurrency || d.Missed != tt.missed {
				t.Errorf("%q: concurrency %q, missed %q", tt.keys, d.Concurrency, d.Missed)
			}
		})
	}
}

// AC1 (Q4): settings.max_concurrent_runs has a default and a range; a
// problem names file, path and line.
func TestMaxConcurrentRuns(t *testing.T) {
	job := "jobs:\n  - {name: a, schedule: \"@daily\", command: /bin/sh}\n"
	if in := mustConfigureProcs(t, map[string]string{"supervisor/a.yaml": "version: 1\n" + job}, &fakeLauncher{}); in.settings.MaxConcurrentRuns != 32 {
		t.Errorf("default = %d", in.settings.MaxConcurrentRuns)
	}
	for _, n := range []int{1, maxServices} {
		body := fmt.Sprintf("version: 1\nsettings: {max_concurrent_runs: %d}\n%s", n, job)
		in := mustConfigureProcs(t, map[string]string{"supervisor/a.yaml": body}, &fakeLauncher{})
		if in.settings.MaxConcurrentRuns != n {
			t.Errorf("%d read as %d", n, in.settings.MaxConcurrentRuns)
		}
	}
	for _, v := range []string{"0", "-1", "257", "many"} {
		t.Run(v, func(t *testing.T) {
			body := "version: 1\nsettings: {max_concurrent_runs: " + v + "}\n" + job
			_, err := configureProcs(t, map[string]string{"supervisor/a.yaml": body}, &fakeLauncher{})
			if err == nil || !strings.Contains(err.Error(), "line 2") || (v != "many" && !strings.Contains(err.Error(), "settings.max_concurrent_runs")) {
				t.Errorf("err = %v", err)
			}
		})
	}
}

// The example file configures (with its programs and accounts faked).
func TestJobsExample(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "configs", "supervisor", "jobs.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	logDir := t.TempDir()
	body := strings.ReplaceAll(string(data), "/var/log/backup/backup.log", logDir+"/backup.log")
	for _, prog := range []string{"/usr/sbin/logrotate", "/opt/backup/bin/backup", "/usr/local/bin/fetch-feeds", "/usr/local/bin/refresh-cache"} {
		body = strings.ReplaceAll(body, "command: "+prog, "command: /bin/sh")
	}
	body = strings.ReplaceAll(body, "${BACKUP_KEY}", "${TOKEN}")
	body = strings.ReplaceAll(body, "user: backup", "user: worker")
	body = strings.ReplaceAll(body, "group: backup", "group: staff")
	body = strings.ReplaceAll(body, "working_dir: /var/lib/backup", "working_dir: /tmp")
	in, err := configureProcs(t, map[string]string{"supervisor/jobs.yaml": body}, &fakeLauncher{})
	if err != nil {
		t.Fatalf("configs/supervisor/jobs.yaml: %v", err)
	}
	if len(in.jobs) != 5 || len(in.Warnings()) != 0 {
		t.Errorf("%d jobs, warnings %v", len(in.jobs), in.Warnings())
	}
}
