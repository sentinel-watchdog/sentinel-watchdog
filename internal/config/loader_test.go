package config

import (
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/model"
)

func TestLoadExampleConfig(t *testing.T) {
	res, err := Load(LoadOptions{
		MainFile:   filepath.Join("..", "..", "configs", "sentinel.example.yaml"),
		IncludeDir: filepath.Join("..", "..", "configs", "conf.d"),
		Lookup: env(map[string]string{
			"SENTINEL_WEBHOOK_URL":   "https://hooks.example.org/services/abc",
			"SENTINEL_WEBHOOK_TOKEN": "token-value",
		}),
	})
	if err != nil {
		t.Fatalf("example configuration must be valid: %v", err)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("example configuration has warnings: %v", res.Warnings)
	}
	cfg := res.Config
	var names []string
	for _, m := range cfg.Monitors {
		names = append(names, m.Name)
	}
	want := []string{"mycustom", "worker", "nightly-backup", "api-health"}
	if !slices.Equal(names, want) {
		t.Fatalf("monitors = %v, want %v (main file first, then conf.d)", names, want)
	}

	w, _ := cfg.Monitor("worker")
	if w.Process.Limits.MaxMemoryBytes != 1<<30 {
		t.Errorf("max_memory_bytes = %d", w.Process.Limits.MaxMemoryBytes)
	}
	if w.Process.Recovery.Window.Std() != 10*time.Minute {
		t.Errorf("recovery window = %s", w.Process.Recovery.Window)
	}
	ch, _ := cfg.Channel("main-webhook")
	if ch.Headers["Authorization"] != "Bearer token-value" {
		t.Errorf("Authorization header not expanded: %q", ch.Headers["Authorization"])
	}
	b, _ := cfg.Monitor("nightly-backup")
	if b.Cron.Timezone != "Europe/Rome" {
		t.Errorf("cron timezone should default to settings.timezone, got %q", b.Cron.Timezone)
	}
	if len(b.Notifications.Events) != 3 {
		t.Errorf("long-form notification events = %v", b.Notifications.Events)
	}
}

func TestDefaults(t *testing.T) {
	cfg := mustLoad(t, baseConfig(`
  - name: svc
    type: systemd
    service: nginx.service
    recovery: {}
    notifications: [hook]
  - name: api
    type: http
    url: https://example.org/
  - name: job
    type: cron
    schedule: "@daily"
    script: echo hello
    notifications: [hook]
`), nil)

	s := cfg.Settings
	if s.StateFile != DefaultStateFile || s.Socket != DefaultSocket || s.SocketMode != 0o660 ||
		s.LogLevel != "info" || s.LogFormat != LogFormatAuto || s.HistoryLimit != DefaultHistoryLimit {
		t.Errorf("unexpected settings defaults: %+v", s)
	}

	ch, _ := cfg.Channel("hook")
	if !ch.IsEnabled() || ch.Method != http.MethodPost || ch.Retry.Attempts != 3 || ch.Headers["Content-Type"] != "application/json" {
		t.Errorf("unexpected channel defaults: %+v", ch)
	}

	svc, _ := cfg.Monitor("svc")
	if !svc.IsEnabled() {
		t.Error("monitor should default to enabled")
	}
	r := svc.Systemd.Recovery
	if !r.IsEnabled() || r.Action != RecoveryRestart || r.MaxAttempts != 5 || r.Backoff != BackoffExponential {
		t.Errorf("unexpected recovery defaults: %+v", r)
	}
	if svc.Systemd.CheckInterval != DefaultCheckInterval || *svc.Systemd.JournalLines != 20 {
		t.Errorf("unexpected systemd defaults: %+v", svc.Systemd)
	}
	if !slices.Equal(svc.Notifications.Events, DefaultEvents(model.MonitorSystemd)) {
		t.Errorf("default events = %v", svc.Notifications.Events)
	}

	api, _ := cfg.Monitor("api")
	if api.HTTP.Method != http.MethodGet || api.HTTP.FollowRedirects || api.HTTP.TLS.InsecureSkipVerify ||
		api.HTTP.FailurePolicy.ConsecutiveFailures != 3 || api.HTTP.Expect.BodyMaxBytes != 1<<20 {
		t.Errorf("unexpected http defaults: %+v", api.HTTP)
	}
	if len(api.Notifications.Events) != 0 {
		t.Errorf("no channels means no default events, got %v", api.Notifications.Events)
	}

	job, _ := cfg.Monitor("job")
	if job.Cron.Shell != "/bin/sh" || job.Cron.ConcurrencyPolicy != ConcurrencyForbid ||
		job.Cron.MissedRuns != MissedSkip || job.Cron.Stdout.Type != OutputLog {
		t.Errorf("unexpected cron defaults: %+v", job.Cron)
	}
	if !slices.Equal(job.Notifications.Events, []model.EventType{model.EventJobFailed, model.EventJobTimeout}) {
		t.Errorf("cron default events = %v", job.Notifications.Events)
	}
}

func TestIncludeDirOrderAndFiltering(t *testing.T) {
	frag := func(name string) string {
		return "version: 1\nmonitors:\n  - name: " + name + "\n    type: http\n    url: https://example.org/\n"
	}
	main := writeTree(t, "version: 1\n", map[string]string{
		"20-b.yaml":     frag("b"),
		"10-a.yml":      frag("a"),
		"30-c.yaml":     frag("c"),
		".hidden.yaml":  frag("hidden"),
		"README.md":     "not yaml",
		"40-d.yaml.bak": frag("backup"),
		"05-empty.yml~": "",
	})
	res, err := Load(LoadOptions{MainFile: main, Lookup: env(nil)})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, m := range res.Config.Monitors {
		names = append(names, m.Name)
	}
	if !slices.Equal(names, []string{"a", "b", "c"}) {
		t.Fatalf("monitors = %v", names)
	}
	if len(res.Files) != 4 || res.Files[0] != main {
		t.Errorf("files = %v", res.Files)
	}
}

func TestMissingIncludeDirIsFine(t *testing.T) {
	main := writeTree(t, "version: 1\n", nil)
	if _, err := Load(LoadOptions{MainFile: main, IncludeDir: "/nonexistent/sentinel/conf.d", Lookup: env(nil)}); err != nil {
		t.Fatal(err)
	}
}

func TestDuplicateNames(t *testing.T) {
	mon := "  - name: dup\n    type: http\n    url: https://example.org/\n"
	t.Run("same file", func(t *testing.T) {
		_, err := loadString(t, baseConfig(mon+mon), nil)
		requireProblem(t, err, "monitors[dup]", "duplicate monitor name")
	})
	t.Run("across files", func(t *testing.T) {
		main := writeTree(t, baseConfig(mon), map[string]string{
			"10-x.yaml": "version: 1\nmonitors:\n" + mon,
		})
		_, err := Load(LoadOptions{MainFile: main, Lookup: env(nil)})
		requireProblem(t, err, "10-x.yaml", "duplicate monitor name", "sentinel.yaml")
	})
	t.Run("channels", func(t *testing.T) {
		_, err := loadString(t, `version: 1
notifications:
  - {name: a, type: webhook, url: "https://x.example/"}
  - {name: a, type: webhook, url: "https://y.example/"}
`, nil)
		requireProblem(t, err, "duplicate notification channel name")
	})
}

func TestFileLevelErrors(t *testing.T) {
	tests := []struct {
		name      string
		main      string
		fragments map[string]string
		want      []string
	}{
		{"empty file", "", nil, []string{"file is empty"}},
		{"missing version", "settings: {log_level: info}\n", nil, []string{"version", "is required"}},
		{"wrong version", "version: 2\n", nil, []string{"unsupported configuration version 2"}},
		{"multiple documents", "version: 1\n---\nversion: 1\n", nil, []string{"multiple YAML documents"}},
		{"syntax error", "version: 1\nmonitors: [\n", nil, []string{"sentinel.yaml"}},
		{"unknown top-level key", "version: 1\nmonitor: []\n", nil, []string{`line 2: unknown field "monitor"`}},
		{"duplicate key", "version: 1\nversion: 1\n", nil, []string{"already defined"}},
		{"settings in fragment", "version: 1\n", map[string]string{"a.yaml": "version: 1\nsettings: {log_level: debug}\n"},
			[]string{"a.yaml", "settings", "only allowed in the main configuration file"}},
		{"fragment without version", "version: 1\n", map[string]string{"a.yaml": "monitors: []\n"},
			[]string{"a.yaml", "version", "is required"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(LoadOptions{MainFile: writeTree(t, tt.main, tt.fragments), Lookup: env(nil)})
			requireProblem(t, err, tt.want...)
		})
	}
}

func TestMainFileErrors(t *testing.T) {
	if _, err := Load(LoadOptions{}); err == nil {
		t.Error("empty main file path must fail")
	}
	_, err := Load(LoadOptions{MainFile: filepath.Join(t.TempDir(), "missing.yaml"), Lookup: env(nil)})
	requireProblem(t, err, "missing.yaml", "no such file")

	big := writeTree(t, "version: 1\n#"+strings.Repeat("x", MaxFileSize)+"\n", nil)
	_, err = Load(LoadOptions{MainFile: big, Lookup: env(nil)})
	requireProblem(t, err, "exceeds")

	dir := t.TempDir()
	_, err = Load(LoadOptions{MainFile: dir, Lookup: env(nil)})
	requireProblem(t, err, "not a regular file")
}

func TestMonitorTypeDispatch(t *testing.T) {
	tests := []struct {
		name string
		mon  string
		want []string
	}{
		{"planned type", "  - name: data\n    type: mount\n    path: /mnt/data\n",
			[]string{`monitor type "mount" is planned but not implemented`}},
		{"unknown type", "  - name: x\n    type: banana\n", []string{`unknown monitor type "banana"`}},
		{"missing type", "  - name: x\n    service: a.service\n", []string{`missing required field "type"`}},
		{"field of another type", "  - name: x\n    type: systemd\n    service: a.service\n    url: https://x/\n",
			[]string{`line 10: unknown field "url"`}},
		{"unknown nested field", "  - name: x\n    type: systemd\n    service: a.service\n    recovery:\n      retries: 3\n",
			[]string{`unknown field "retries"`}},
		{"draft http interval key", "  - name: x\n    type: http\n    url: https://x/\n    interval: 30s\n",
			[]string{`unknown field "interval"`}},
		{"wrong value type", "  - name: x\n    type: systemd\n    service: a.service\n    recovery:\n      max_attempts: many\n",
			[]string{"cannot unmarshal"}},
		{"integer duration", "  - name: x\n    type: systemd\n    service: a.service\n    check_interval: 15\n",
			[]string{"duration must be a string"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadString(t, baseConfig(tt.mon), nil)
			requireProblem(t, err, tt.want...)
		})
	}
}

func TestDecodeErrorsAreCollectedAcrossMonitors(t *testing.T) {
	_, err := loadString(t, baseConfig(`
  - name: a
    type: mount
  - name: b
    type: banana
`), nil)
	requireProblem(t, err, `"mount" is planned`)
	requireProblem(t, err, `"banana"`)
}

func TestNotificationForms(t *testing.T) {
	cfg := mustLoad(t, baseConfig(`
  - name: short
    type: http
    url: https://example.org/
    notifications: [hook]
  - name: long
    type: http
    url: https://example.org/
    notifications:
      channels: [hook]
      events: [monitor_recovered]
`), nil)
	short, _ := cfg.Monitor("short")
	long, _ := cfg.Monitor("long")
	if !slices.Equal(short.Notifications.Channels, []string{"hook"}) || len(short.Notifications.Events) != 3 {
		t.Errorf("short form = %+v", short.Notifications)
	}
	if !slices.Equal(long.Notifications.Events, []model.EventType{model.EventMonitorRecovered}) {
		t.Errorf("long form = %+v", long.Notifications)
	}

	_, err := loadString(t, baseConfig(`
  - name: x
    type: http
    url: https://example.org/
    notifications:
      on: [failure]
      channels: [hook]
`), nil)
	requireProblem(t, err, `unknown field "on"`)
}

func TestSymlinkedFragment(t *testing.T) {
	main := writeTree(t, "version: 1\n", map[string]string{"placeholder.txt": ""})
	target := filepath.Join(t.TempDir(), "real.yaml")
	if err := os.WriteFile(target, []byte("version: 1\nmonitors:\n  - {name: l, type: http, url: \"https://e.org/\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(filepath.Dir(main), "conf.d", "50-link.yaml")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	cfg, err := Load(LoadOptions{MainFile: main, Lookup: env(nil)})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Config.Monitor("l"); !ok {
		t.Error("symlinked fragment not loaded")
	}
}
