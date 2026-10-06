package config

import (
	"errors"
	"strings"
	"testing"
)

func TestValidationErrors(t *testing.T) {
	tests := []struct {
		name string
		mon  string
		want []string
	}{
		// common
		{"invalid name", "  - name: Bad_Name\n    type: http\n    url: https://e.org/\n", []string{"name", "must match"}},
		{"unknown channel", "  - name: x\n    type: http\n    url: https://e.org/\n    notifications: [nope]\n",
			[]string{"supervisors[x].notifications.channels", `unknown notification channel "nope"`}},
		{"channel twice", "  - name: x\n    type: http\n    url: https://e.org/\n    notifications: [hook, hook]\n",
			[]string{"listed twice"}},
		{"job event on http", "  - name: x\n    type: http\n    url: https://e.org/\n    notifications: {channels: [hook], events: [job_failed]}\n",
			[]string{`event "job_failed" does not apply to http supervisors`}},
		{"unknown event", "  - name: x\n    type: cron\n    schedule: '@daily'\n    command: /bin/true\n    notifications: {channels: [hook], events: [failure]}\n",
			[]string{`unknown event type "failure"`, "job_failed"}},
		{"events without channels", "  - name: x\n    type: http\n    url: https://e.org/\n    notifications: {events: [supervisor_failed]}\n",
			[]string{"no channel is listed"}},

		// systemd
		{"systemd missing service", "  - name: x\n    type: systemd\n", []string{"supervisors[x].service", "is required"}},
		{"systemd not a service", "  - name: x\n    type: systemd\n    service: foo.timer\n", []string{"not a valid systemd service"}},
		{"systemd injection", "  - name: x\n    type: systemd\n    service: 'a.service; rm -rf /'\n", []string{"not a valid systemd service"}},
		{"systemd option-like", "  - name: x\n    type: systemd\n    service: --now.service\n", []string{"not a valid systemd service"}},
		{"interval too small", "  - name: x\n    type: systemd\n    service: a.service\n    check_interval: 100ms\n",
			[]string{"check_interval", "out of range"}},
		{"negative interval", "  - name: x\n    type: systemd\n    service: a.service\n    check_interval: -5s\n",
			[]string{"check_interval", "out of range"}},
		{"recovery execute planned", "  - name: x\n    type: systemd\n    service: a.service\n    recovery: {action: execute}\n",
			[]string{`"execute" is planned`}},
		{"recovery unknown action", "  - name: x\n    type: systemd\n    service: a.service\n    recovery: {action: reboot}\n",
			[]string{`unknown recovery action "reboot"`}},
		{"recovery max_delay < delay", "  - name: x\n    type: systemd\n    service: a.service\n    recovery: {delay: 1m, max_delay: 10s}\n",
			[]string{"recovery.max_delay", "greater than or equal to delay"}},
		{"recovery bad backoff", "  - name: x\n    type: systemd\n    service: a.service\n    recovery: {backoff: linear}\n",
			[]string{"recovery.backoff", "allowed: fixed, exponential"}},
		{"recovery negative attempts", "  - name: x\n    type: systemd\n    service: a.service\n    recovery: {max_attempts: -1}\n",
			[]string{"max_attempts", "out of range"}},

		// process / exec
		{"command and script", "  - name: x\n    type: process\n    command: /bin/a\n    script: echo\n", []string{"mutually exclusive"}},
		{"no command", "  - name: x\n    type: process\n", []string{"one of command"}},
		{"relative command", "  - name: x\n    type: process\n    command: worker\n", []string{"command", "absolute path"}},
		{"unclean command", "  - name: x\n    type: process\n    command: /opt/../bin/sh\n", []string{"clean path"}},
		{"shell with command", "  - name: x\n    type: process\n    command: /bin/a\n    shell: /bin/bash\n", []string{"only valid with script"}},
		{"args with script", "  - name: x\n    type: process\n    script: echo\n    args: [a]\n", []string{"args", "not allowed with script"}},
		{"bad user", "  - name: x\n    type: process\n    command: /bin/a\n    user: 'root;id'\n", []string{"invalid user"}},
		{"bad env name", "  - name: x\n    type: process\n    command: /bin/a\n    environment: {'A-B': x}\n", []string{"invalid variable name"}},
		{"file output without path", "  - name: x\n    type: process\n    command: /bin/a\n    stdout: {type: file}\n",
			[]string{"stdout.path", "is required"}},
		{"path on log output", "  - name: x\n    type: process\n    command: /bin/a\n    stdout: {type: log, path: /tmp/x}\n",
			[]string{"only valid with type file"}},
		{"journald output not a type", "  - name: x\n    type: process\n    command: /bin/a\n    stdout: {type: journald}\n",
			[]string{"stdout.type", "allowed: log, file, discard"}},
		{"world writable log", "  - name: x\n    type: process\n    command: /bin/a\n    stdout: {type: file, path: /var/log/a, mode: '0666'}\n",
			[]string{"world-writable"}},
		{"same stdout stderr file", "  - name: x\n    type: process\n    command: /bin/a\n    stdout: {type: file, path: /var/log/a}\n    stderr: {type: file, path: /var/log/a}\n",
			[]string{"different files"}},
		{"bad stop signal", "  - name: x\n    type: process\n    command: /bin/a\n    health: {stop_signal: SIGKILL}\n",
			[]string{"stop_signal"}},
		{"empty limits", "  - name: x\n    type: process\n    command: /bin/a\n    limits: {action: kill}\n",
			[]string{"set max_cpu_percent and/or max_memory_bytes"}},
		{"bad limit action", "  - name: x\n    type: process\n    command: /bin/a\n    limits: {max_cpu_percent: 50, action: reboot}\n",
			[]string{"limits.action"}},
		{"recovery on http", "  - name: x\n    type: http\n    url: https://e.org/\n    recovery: {enabled: true}\n",
			[]string{`unknown field "recovery"`}},

		// http
		{"http bad scheme", "  - name: x\n    type: http\n    url: ftp://e.org/\n", []string{"scheme must be http or https"}},
		{"http no host", "  - name: x\n    type: http\n    url: https:///path\n", []string{"host is missing"}},
		{"http bad method", "  - name: x\n    type: http\n    url: https://e.org/\n    method: FETCH\n", []string{"method"}},
		{"http bad status", "  - name: x\n    type: http\n    url: https://e.org/\n    expect: {status_codes: [700]}\n",
			[]string{"700 is not a valid HTTP status code"}},
		{"http header injection", "  - name: x\n    type: http\n    url: https://e.org/\n    headers: {X-A: \"a\\r\\nX-B: b\"}\n",
			[]string{"CR, LF or NUL"}},
		{"http bad header name", "  - name: x\n    type: http\n    url: https://e.org/\n    headers: {'X A': b}\n",
			[]string{"invalid header name"}},
		{"http body too large cap", "  - name: x\n    type: http\n    url: https://e.org/\n    expect: {body_max_bytes: 1GiB}\n",
			[]string{"must not exceed 64MiB"}},
		{"http relative ca", "  - name: x\n    type: http\n    url: https://e.org/\n    tls: {ca_file: ca.pem}\n",
			[]string{"tls.ca_file", "absolute path"}},

		// cron
		{"cron missing schedule", "  - name: x\n    type: cron\n    command: /bin/true\n", []string{"schedule", "is required"}},
		{"cron bad schedule", "  - name: x\n    type: cron\n    schedule: '61 * * * *'\n    command: /bin/true\n",
			[]string{"minute field", "out of range"}},
		{"cron every syntax", "  - name: x\n    type: cron\n    schedule: 'every 5m'\n    command: /bin/true\n",
			[]string{"must have 5 fields"}},
		{"cron reboot", "  - name: x\n    type: cron\n    schedule: '@reboot'\n    command: /bin/true\n",
			[]string{"unsupported cron macro"}},
		{"cron bad tz", "  - name: x\n    type: cron\n    schedule: '@daily'\n    command: /bin/true\n    timezone: Mars/Olympus\n",
			[]string{"unknown time zone"}},
		{"cron bad concurrency", "  - name: x\n    type: cron\n    schedule: '@daily'\n    command: /bin/true\n    concurrency_policy: queue\n",
			[]string{"concurrency_policy"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadString(t, baseConfig(tt.mon), nil)
			requireProblem(t, err, tt.want...)
		})
	}
}

func TestSettingsValidation(t *testing.T) {
	tests := []struct {
		name     string
		settings string
		want     []string
	}{
		{"relative state file", "state_file: state.json", []string{"settings.state_file", "absolute path"}},
		{"world writable socket", "socket_mode: \"0666\"", []string{"settings.socket_mode", "world-writable"}},
		{"socket mode without owner rw", "socket_mode: \"0060\"", []string{"must grant read and write to the owner"}},
		{"socket path too long", "socket: /" + strings.Repeat("a", 120), []string{"Unix socket limit"}},
		{"log level", "log_level: verbose", []string{"settings.log_level"}},
		{"log format", "log_format: xml", []string{"settings.log_format"}},
		{"timezone", "timezone: Nowhere/City", []string{"unknown time zone"}},
		{"history limit", "history_limit: -1", []string{"settings.history_limit"}},
		{"daemon notifications", "daemon_notifications: [missing]", []string{"settings.daemon_notifications", "unknown notification channel"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadString(t, "version: 1\nsettings:\n  "+tt.settings+"\n", nil)
			requireProblem(t, err, tt.want...)
		})
	}
}

func TestChannelValidation(t *testing.T) {
	tests := []struct {
		name    string
		channel string
		want    []string
	}{
		{"slack planned", "{name: s, type: slack, url: 'https://x.example/'}", []string{`"slack" is planned`}},
		{"unknown type", "{name: s, type: smtp, url: 'https://x.example/'}", []string{`unknown notification type "smtp"`}},
		{"missing type", "{name: s, url: 'https://x.example/'}", []string{"type", "is required"}},
		{"missing url", "{name: s, type: webhook}", []string{"url", "is required"}},
		{"method", "{name: s, type: webhook, url: 'https://x.example/', method: GET}", []string{"method"}},
		{"retry attempts", "{name: s, type: webhook, url: 'https://x.example/', retry: {attempts: 50}}", []string{"retry.attempts"}},
		{"success codes", "{name: s, type: webhook, url: 'https://x.example/', success_status_codes: [42]}", []string{"not a valid HTTP status code"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadString(t, "version: 1\nnotifications:\n  - "+tt.channel+"\n", nil)
			requireProblem(t, err, tt.want...)
		})
	}
}

func TestValidationURLErrorsDoNotLeakSecrets(t *testing.T) {
	_, err := loadString(t, "version: 1\nnotifications:\n  - {name: s, type: webhook, url: \"https://h\\x7f/${TOKEN}\"}\n",
		map[string]string{"TOKEN": "supersecret"})
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "supersecret") {
		t.Fatalf("secret leaked in error: %v", err)
	}
}

func TestValidationCollectsAllProblems(t *testing.T) {
	_, err := loadString(t, baseConfig(`
  - name: a
    type: systemd
    service: bad
  - name: b
    type: http
    url: ftp://e.org/
`), nil)
	var ve *ValidationError
	if !errors.As(err, &ve) || len(ve.Problems) != 2 {
		t.Fatalf("expected 2 problems, got %v", err)
	}
}

func TestWarnings(t *testing.T) {
	res, err := loadString(t, `version: 1
notifications:
  - {name: plain, type: webhook, url: "http://hooks.internal/x"}
  - {name: off, type: webhook, enabled: false, url: "https://hooks.internal/x"}
supervisors:
  - name: api
    type: http
    url: https://e.org/
    timeout: 30s
    check_interval: 15s
    tls: {insecure_skip_verify: true}
    notifications: [off]
`, nil)
	if err != nil {
		t.Fatal(err)
	}
	var all []string
	for _, w := range res.Warnings {
		all = append(all, w.String())
	}
	joined := strings.Join(all, "\n")
	for _, want := range []string{"plain HTTP", "is disabled", "certificate verification is disabled", "not shorter than check_interval"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing warning %q in:\n%s", want, joined)
		}
	}
}
