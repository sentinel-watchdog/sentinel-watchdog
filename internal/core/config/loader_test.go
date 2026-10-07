package config

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestLoadMinimalAppliesDefaults(t *testing.T) {
	cfg := mustLoad(t, map[string]string{"sentinel.yaml": "version: 1\n"}, nil)
	d := cfg.Daemon
	if d.Socket != DefaultSocket || d.SocketMode != DefaultSocketMode || d.StateDir != DefaultStateDir ||
		d.Timezone != DefaultTimezone || d.ShutdownTimeout != DefaultShutdownTimeout ||
		d.Log.Level != DefaultLogLevel || d.Log.Format != LogFormatAuto {
		t.Errorf("defaults not applied: %+v", d)
	}
	if len(cfg.Modules) != 0 || len(cfg.Warnings) != 0 {
		t.Errorf("modules = %v, warnings = %v", cfg.Modules, cfg.Warnings)
	}
	if len(cfg.Files) != 1 || filepath.Base(cfg.Files[0]) != "sentinel.yaml" {
		t.Errorf("Files = %v", cfg.Files)
	}
}

func TestLoadCentralFile(t *testing.T) {
	cfg := mustLoad(t, map[string]string{"sentinel.yaml": `
version: 1
daemon:
  socket: /run/sentinel/test.sock
  socket_mode: "0640"
  socket_group: sentinel
  state_dir: /var/lib/sentinel-test
  timezone: Europe/Rome
  shutdown_timeout: 1m
  log: {level: debug, format: json}
  access: {operator_group: ops}
notifications:
  channels:
    - name: ops
      type: webhook
      url: https://hooks.example.org/x
      headers:
        Authorization: Bearer ${TOKEN}
`}, map[string]string{"TOKEN": "s3cret"})

	d := cfg.Daemon
	if d.Socket != "/run/sentinel/test.sock" || d.SocketMode != 0o640 || d.SocketGroup != "sentinel" ||
		d.StateDir != "/var/lib/sentinel-test" || d.Timezone != "Europe/Rome" ||
		d.ShutdownTimeout.Std() != time.Minute || d.Log.Level != "debug" || d.Log.Format != LogFormatJSON ||
		d.Access.OperatorGroup != "ops" {
		t.Errorf("daemon = %+v", d)
	}
	ch, ok := cfg.Channel("ops")
	if !ok {
		t.Fatal("channel ops missing")
	}
	if ch.Method != http.MethodPost || ch.Retry.Attempts != 3 || ch.Headers["Content-Type"] != "application/json" {
		t.Errorf("channel defaults not applied: %+v", ch)
	}
	if ch.Headers["Authorization"] != "Bearer s3cret" {
		t.Errorf("${TOKEN} not expanded: %q", ch.Headers["Authorization"])
	}
}

func TestLoadCentralErrors(t *testing.T) {
	tests := []struct {
		name string
		main string
		want []string
	}{
		{"empty file", "", []string{"file is empty"}},
		{"missing version", "daemon: {}\n", []string{"version is required"}},
		{"newer version", "version: 2\n", []string{"unsupported configuration version 2"}},
		{"version as string", "version: \"1\"\n", []string{"must be the integer 1"}},
		{"two documents", "version: 1\n---\nversion: 1\n", []string{"multiple YAML documents"}},
		{"top level list", "- version: 1\n", []string{"top level must be a mapping"}},
		{"syntax error", "version: 1\ndaemon: [\n", []string{"sentinel.yaml"}},
		{"unknown top-level key", "version: 1\nsettings: {}\n", []string{`line 2: unknown field "settings"`}},
		{"unknown daemon key", "version: 1\ndaemon:\n  sockets: /x\n", []string{`line 3: unknown field "sockets"`, "valid fields"}},
		{"duration as integer", "version: 1\ndaemon:\n  shutdown_timeout: 30\n", []string{"duration must be a string"}},
		{"relative socket", "version: 1\ndaemon: {socket: run/s.sock}\n", []string{"daemon.socket", "absolute path"}},
		{"unclean state dir", "version: 1\ndaemon: {state_dir: /var/lib/../x}\n", []string{"daemon.state_dir", "clean path"}},
		{"world-writable socket", "version: 1\ndaemon: {socket_mode: \"0666\"}\n", []string{"daemon.socket_mode", "world-writable"}},
		{"unknown time zone", "version: 1\ndaemon: {timezone: Mars/Olympus}\n", []string{"daemon.timezone"}},
		{"log level", "version: 1\ndaemon: {log: {level: verbose}}\n", []string{"daemon.log.level", "allowed: debug, info, warn, error"}},
		{"bad group", "version: 1\ndaemon: {access: {admin_group: \"Bad Group\"}}\n", []string{"daemon.access.admin_group"}},
		{"planned channel type", "version: 1\nnotifications:\n  channels:\n    - {name: s, type: slack, url: https://x.org}\n",
			[]string{"notifications.channels[s].type", "planned but not implemented"}},
		{"channel URL scheme", "version: 1\nnotifications:\n  channels:\n    - {name: w, type: webhook, url: ftp://x.org}\n",
			[]string{"notifications.channels[w].url", "http or https"}},
		{"duplicate channel", "version: 1\nnotifications:\n  channels:\n    - {name: w, type: webhook, url: https://x.org}\n    - {name: w, type: webhook, url: https://y.org}\n",
			[]string{"duplicate channel name"}},
		{"header injection", "version: 1\nnotifications:\n  channels:\n    - {name: w, type: webhook, url: https://x.org, headers: {X-A: \"a\\r\\nX-B: b\"}}\n",
			[]string{"CR, LF or NUL"}},
		{"undefined variable", "version: 1\ndaemon:\n  state_dir: ${NOPE}\n", []string{`"NOPE" is not set`, "line 3"}},
		{"channel type missing", channelDoc(`url: https://x.org`), []string{"notifications.channels[w].type", "is required"}},
		{"channel type unknown", channelDoc(`type: smtp, url: https://x.org`), []string{`unknown notification type "smtp"`}},
		{"channel URL missing", channelDoc(`type: webhook`), []string{"notifications.channels[w].url", "is required"}},
		{"channel URL host missing", channelDoc(`type: webhook, url: "https:///path"`), []string{"host is missing"}},
		{"channel method", channelDoc(`type: webhook, url: https://x.org, method: GET`), []string{"channels[w].method", "allowed: POST, PUT"}},
		{"channel timeout", channelDoc(`type: webhook, url: https://x.org, timeout: 10m`), []string{"channels[w].timeout", "out of range"}},
		{"retry attempts", channelDoc(`type: webhook, url: https://x.org, retry: {attempts: 50}`), []string{"retry.attempts", "out of range (1 - 10)"}},
		{"retry delays", channelDoc(`type: webhook, url: https://x.org, retry: {delay: 2m, max_delay: 1m}`), []string{"retry.max_delay", "greater than or equal"}},
		{"retry backoff", channelDoc(`type: webhook, url: https://x.org, retry: {backoff: linear}`), []string{"retry.backoff"}},
		{"status code", channelDoc(`type: webhook, url: https://x.org, success_status_codes: [700]`), []string{"700 is not a valid HTTP status code"}},
		{"CA file relative", channelDoc(`type: webhook, url: https://x.org, tls: {ca_file: ca.pem}`), []string{"tls.ca_file", "absolute path"}},
		{"bad header name", channelDoc(`type: webhook, url: https://x.org, headers: {"Bad Header": x}`), []string{"invalid header name"}},
		{"invalid channel name", "version: 1\nnotifications:\n  channels:\n    - {name: Bad_Name, type: webhook, url: https://x.org}\n",
			[]string{"notifications.channels[0].name", "must match"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := load(t, map[string]string{"sentinel.yaml": tt.main}, nil)
			requireProblem(t, err, tt.want...)
		})
	}
}

// channelDoc returns a central file with one channel named w and fields.
func channelDoc(fields string) string {
	return "version: 1\nnotifications:\n  channels:\n    - {name: w, " + fields + "}\n"
}

func TestLoadCentralWarnings(t *testing.T) {
	cfg := mustLoad(t, map[string]string{"sentinel.yaml": `
version: 1
daemon: {access: {admin_group: wheel}}
notifications:
  channels:
    - {name: w, type: webhook, url: "http://x.org", tls: {insecure_skip_verify: true}}
`}, nil)
	for _, want := range []string{"root-equivalent", "plain HTTP", "verification is disabled"} {
		if !hasWarning(cfg.Warnings, want) {
			t.Errorf("missing warning %q in %v", want, cfg.Warnings)
		}
	}
}

func TestLoadModuleSwitches(t *testing.T) {
	tests := []struct {
		name string
		mods string
		want []string
	}{
		{"unknown module", "  gamma: {enabled: true}\n", []string{"modules.gamma", `unknown module "gamma"`, "alpha (available)"}},
		{"unknown even if disabled", "  gamma: {enabled: false}\n", []string{`unknown module "gamma"`}},
		{"enabled planned", "  planned: {enabled: true}\n", []string{"modules.planned", "planned but not implemented"}},
		{"enabled not built", "  excluded: {enabled: true}\n", []string{"modules.excluded", "not built into this binary"}},
		{"enabled not boolean", "  alpha: {enabled: \"yes\"}\n", []string{"modules.alpha.enabled", "true or false"}},
		{"block not a mapping", "  alpha: [enabled]\n", []string{"modules.alpha", "must be a mapping"}},
		{"duplicate module", "  alpha: {}\n  alpha: {}\n", []string{`line 4: duplicate key "alpha" (first defined on line 3)`}},
		{"duplicate switch", "  alpha: {enabled: true, enabled: false}\n", []string{`duplicate key "enabled"`}},
		{"merge key in a module block", "  alpha: {<<: {enabled: true}}\n", []string{"merge keys (<<) are not supported"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := load(t, map[string]string{"sentinel.yaml": "version: 1\nmodules:\n" + tt.mods}, nil)
			requireProblem(t, err, tt.want...)
		})
	}
}

func TestLoadDisabledPlannedModulesAreAccepted(t *testing.T) {
	cfg := mustLoad(t, map[string]string{"sentinel.yaml": `
version: 1
modules:
  planned: {enabled: false}
  excluded: {}
  alpha:
`}, nil)
	var names []string
	for _, m := range cfg.Modules {
		names = append(names, m.Name)
		if m.Enabled {
			t.Errorf("module %s enabled by default", m.Name)
		}
	}
	if want := []string{"alpha", "excluded", "planned"}; !slices.Equal(names, want) {
		t.Errorf("modules = %v, want %v (sorted)", names, want)
	}
}

func TestLoadModuleDirectory(t *testing.T) {
	cfg := mustLoad(t, map[string]string{
		"sentinel.yaml": `
version: 1
modules:
  alpha:
    enabled: true
    mode: read_only
`,
		"alpha/20-b.yml":          "version: 1\nitems: [b]\n",
		"alpha/10-a.yaml":         "version: 1\nsettings: {x: 1}\nitems: [\"${ITEM}\"]\n",
		"alpha/.hidden.yaml":      "not: [valid",
		"alpha/README.md":         "ignored",
		"alpha/old/30-c.yaml":     "version: 1\n",
		"alpha/notes.yaml.bak":    "ignored",
		"beta/10-ignored.yaml":    "not: [valid",
		"unknown-dir/10-any.yaml": "not: [valid",
	}, map[string]string{"ITEM": "a"})

	alpha, ok := cfg.Module("alpha")
	if !ok || !alpha.Enabled || alpha.Availability != ModuleAvailable || !alpha.DirExists {
		t.Fatalf("alpha = %+v", alpha)
	}
	if alpha.Central.Has("enabled") || !alpha.Central.Has("mode") {
		t.Errorf("central keys = %v, want [mode]", alpha.Central.Keys())
	}
	var got []string
	for _, f := range alpha.Files {
		got = append(got, filepath.Base(f.File))
		if f.Has("version") {
			t.Errorf("%s: version key not removed", f.File)
		}
	}
	if want := []string{"10-a.yaml", "20-b.yml"}; !slices.Equal(got, want) {
		t.Errorf("files = %v, want %v", got, want)
	}

	var first struct {
		Settings struct {
			X int `yaml:"x"`
		} `yaml:"settings"`
		Items []string `yaml:"items"`
	}
	if err := alpha.Files[0].Decode(&first); err != nil {
		t.Fatal(err)
	}
	if first.Settings.X != 1 || !slices.Equal(first.Items, []string{"a"}) {
		t.Errorf("decoded %+v (${ITEM} must be expanded)", first)
	}

	if !hasWarning(cfg.Warnings, "subdirectories are not read") {
		t.Errorf("no warning for alpha/old: %v", cfg.Warnings)
	}
	if len(cfg.IgnoredDirs) != 1 || filepath.Base(cfg.IgnoredDirs[0]) != "beta" {
		t.Errorf("IgnoredDirs = %v, want [.../beta]", cfg.IgnoredDirs)
	}
	if len(cfg.Files) != 3 {
		t.Errorf("Files = %v, want central + 2 module files", cfg.Files)
	}
}

func TestLoadEnabledModuleWithoutDirectory(t *testing.T) {
	cfg := mustLoad(t, map[string]string{"sentinel.yaml": "version: 1\nmodules:\n  alpha: {enabled: true}\n"}, nil)
	alpha, _ := cfg.Module("alpha")
	if alpha.DirExists || len(alpha.Files) != 0 {
		t.Errorf("alpha = %+v, want no directory", alpha)
	}
}

func TestLoadDisabledModuleDirectoryIsNotRead(t *testing.T) {
	files := map[string]string{
		"sentinel.yaml":   "version: 1\nmodules:\n  alpha: {enabled: false}\n",
		"alpha/10-x.yaml": "version: 1\nvalue: ${UNDEFINED}\n",
	}
	cfg, err := load(t, files, nil)
	if err != nil {
		t.Fatalf("disabled module directory was read: %v", err)
	}
	if len(cfg.IgnoredDirs) != 1 {
		t.Errorf("IgnoredDirs = %v", cfg.IgnoredDirs)
	}

	// validate --all reads it and reports the problem.
	_, err = Load(LoadOptions{MainFile: writeTree(t, files), Modules: testModules, Lookup: env(nil), IncludeDisabled: true})
	requireProblem(t, err, "10-x.yaml", `"UNDEFINED" is not set`)
}

func TestLoadModuleFileErrors(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{"missing version", map[string]string{"alpha/a.yaml": "items: []\n"}, []string{"a.yaml", "version is required"}},
		{"settings twice", map[string]string{"alpha/a.yaml": "version: 1\nsettings: {}\n", "alpha/b.yaml": "version: 1\nsettings: {}\n"},
			[]string{"modules.alpha", "`settings` may appear in only one file"}},
		{"syntax error", map[string]string{"alpha/a.yaml": "version: 1\nitems: [\n"}, []string{"a.yaml"}},
		{"directory is a file", map[string]string{"alpha": "x"}, []string{"alpha", "not a directory"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.files["sentinel.yaml"] = "version: 1\nmodules:\n  alpha: {enabled: true}\n"
			_, err := load(t, tt.files, nil)
			requireProblem(t, err, tt.want...)
		})
	}
}

func TestLoadRejectsWritableFiles(t *testing.T) {
	tests := []struct {
		name  string
		chmod string // path relative to the temp dir; "" is the dir itself
		mode  os.FileMode
		want  string
	}{
		{"central file group-writable", "sentinel.yaml", 0o620, "sentinel.yaml"},
		{"config directory world-writable", "", 0o777, "directory is writable by group or others"},
		{"module directory group-writable", "alpha", 0o770, "alpha: directory is writable"},
		{"module file world-writable", "alpha/a.yaml", 0o606, "a.yaml"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			main := writeTree(t, map[string]string{
				"sentinel.yaml": "version: 1\nmodules:\n  alpha: {enabled: true}\n",
				"alpha/a.yaml":  "version: 1\n",
			})
			target := filepath.Join(filepath.Dir(main), tt.chmod)
			if err := os.Chmod(target, tt.mode); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(target, 0o700) })
			_, err := Load(LoadOptions{MainFile: main, Modules: testModules, Lookup: env(nil)})
			requireProblem(t, err, tt.want, "writable by group or others")
		})
	}
}

func TestLoadEnvValuesDoNotInjectStructure(t *testing.T) {
	// The value stays one string: it is rejected as a group name instead
	// of adding a `modules` key to the document.
	_, err := load(t, map[string]string{"sentinel.yaml": `
version: 1
daemon:
  socket_group: ${GROUP}
`}, map[string]string{"GROUP": "x\nmodules: {alpha: {enabled: true}}"})
	requireProblem(t, err, "daemon.socket_group", `invalid group name "x\nmodules:`)
}

func TestLoadDoesNotExpandKeys(t *testing.T) {
	_, err := load(t, map[string]string{"sentinel.yaml": "version: 1\ndaemon:\n  ${KEY}: /x\n"},
		map[string]string{"KEY": "socket"})
	requireProblem(t, err, `unknown field "${KEY}"`)
}

func TestLoadMainFileRequired(t *testing.T) {
	if _, err := Load(LoadOptions{}); err == nil {
		t.Fatal("Load with an empty MainFile succeeded")
	}
	_, err := Load(LoadOptions{MainFile: filepath.Join(t.TempDir(), "missing.yaml")})
	requireProblem(t, err, "missing.yaml", "no such file")
}

func TestExampleConfiguration(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "configs", "sentinel.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	// Copy into a private directory: the checkout's modes depend on umask.
	main := writeTree(t, map[string]string{"sentinel.yaml": string(data)})
	_, err = Load(LoadOptions{
		MainFile: main,
		Modules: map[string]ModuleAvailability{
			"supervisor": ModulePlanned, "firewall": ModulePlanned, "remote": ModulePlanned,
		},
		Lookup: env(map[string]string{"SENTINEL_WEBHOOK_URL": "https://hooks.example.org/x", "SENTINEL_WEBHOOK_TOKEN": "t"}),
	})
	if err != nil {
		t.Fatalf("configs/sentinel.yaml: %v", err)
	}
}

func TestLoadRejectsAmbiguousYAML(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{"duplicate version", map[string]string{"sentinel.yaml": "version: 1\nversion: 2\n"},
			[]string{`duplicate key "version"`}},
		{"merged settings in a module file", map[string]string{
			"sentinel.yaml": "version: 1\nmodules:\n  alpha: {enabled: true}\n",
			"alpha/a.yaml":  "version: 1\n<<: {settings: {x: 1}}\n",
		}, []string{"a.yaml", "merge keys"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := load(t, tt.files, nil)
			requireProblem(t, err, tt.want...)
		})
	}
}

func TestLoadSymlinksStayInsideTheirDirectory(t *testing.T) {
	main := writeTree(t, map[string]string{
		"sentinel.yaml":   "version: 1\nmodules:\n  alpha: {enabled: true}\n",
		"alpha/10-a.yaml": "version: 1\nitems: [a]\n",
		"outside.yaml":    "version: 1\nitems: [stolen]\n",
	})
	dir := filepath.Dir(main)
	// Inside the module directory: allowed.
	if err := os.Symlink("10-a.yaml", filepath.Join(dir, "alpha", "20-link.yaml")); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(LoadOptions{MainFile: main, Modules: testModules, Lookup: env(nil)})
	if err != nil {
		t.Fatalf("symlink inside the module directory: %v", err)
	}
	if alpha, _ := cfg.Module("alpha"); len(alpha.Files) != 2 {
		t.Errorf("files = %d, want 2", len(alpha.Files))
	}

	// Leaving the module directory: rejected.
	if err := os.Symlink("../outside.yaml", filepath.Join(dir, "alpha", "30-escape.yaml")); err != nil {
		t.Fatal(err)
	}
	_, err = Load(LoadOptions{MainFile: main, Modules: testModules, Lookup: env(nil)})
	requireProblem(t, err, "30-escape.yaml")
}

func TestLoadCentralFileSymlinkMustStayInside(t *testing.T) {
	elsewhere := writeTree(t, map[string]string{"sentinel.yaml": "version: 1\n"})
	dir := t.TempDir()
	main := filepath.Join(dir, "sentinel.yaml")
	if err := os.Symlink(elsewhere, main); err != nil {
		t.Fatal(err)
	}
	_, err := Load(LoadOptions{MainFile: main, Modules: testModules, Lookup: env(nil)})
	requireProblem(t, err, "sentinel.yaml")
}

func TestLoadSkipsFIFOWithoutBlocking(t *testing.T) {
	main := writeTree(t, map[string]string{
		"sentinel.yaml":   "version: 1\nmodules:\n  alpha: {enabled: true}\n",
		"alpha/10-a.yaml": "version: 1\n",
	})
	fifo := filepath.Join(filepath.Dir(main), "alpha", "20-pipe.yaml")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		cfg, err := Load(LoadOptions{MainFile: main, Modules: testModules, Lookup: env(nil)})
		if err == nil && !hasWarning(cfg.Warnings, "not a regular file") {
			err = errors.New("no warning for the FIFO")
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Load blocked on a FIFO")
	}
}

func TestLoadTotalSizeLimit(t *testing.T) {
	files := map[string]string{"sentinel.yaml": "version: 1\nmodules:\n  alpha: {enabled: true}\n"}
	padding := strings.Repeat("#", 4_000_000)
	for i := range 5 {
		files[filepath.Join("alpha", "f"+strconv.Itoa(i)+".yaml")] = "version: 1\n" + padding + "\n"
	}
	_, err := load(t, files, nil)
	requireProblem(t, err, "exceeds 16777216 bytes in total")
}

func TestLoadExpansionGrowthLimit(t *testing.T) {
	big := strings.Repeat("x", 1<<20)
	_, err := load(t, map[string]string{"sentinel.yaml": `
version: 1
daemon:
  socket_group: ${BIG}${BIG}${BIG}${BIG}${BIG}
`}, map[string]string{"BIG": big})
	requireProblem(t, err, "environment variable values add more than")
}

func TestLoadIncludeDisabledReadsUnnamedModules(t *testing.T) {
	main := writeTree(t, map[string]string{
		"sentinel.yaml":  "version: 1\n",
		"beta/10-x.yaml": "not: [valid",
	})
	if _, err := Load(LoadOptions{MainFile: main, Modules: testModules, Lookup: env(nil)}); err != nil {
		t.Fatalf("an unnamed module directory was read: %v", err)
	}
	_, err := Load(LoadOptions{MainFile: main, Modules: testModules, Lookup: env(nil), IncludeDisabled: true})
	requireProblem(t, err, "10-x.yaml")
}
