package supervisor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/config"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/platform/systemd"
)

// D-058: a systemd service needs only its name and unit.
func TestUnitDefaults(t *testing.T) {
	in, err := configure(t, map[string]string{"supervisor/a.yaml": "version: 1\nservices:\n  - name: web\n    unit: nginx.service\n"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	def := in.byName["web"].def
	want := Unit{Name: systemd.UnitName{Prefix: "nginx", Type: "service"}, TransitionGrace: 90 * time.Second}
	if def.Type != typeSystemd || def.Unit != want || def.CheckInterval != 30*time.Second || def.Timeout != 10*time.Second ||
		def.Policy != (Policy{Failures: 3, Successes: 1}) || def.URL != nil {
		t.Errorf("def = %+v", def)
	}
	if len(in.warnings) != 0 {
		t.Errorf("warnings %v", in.warnings)
	}
}

func TestUnitKeys(t *testing.T) {
	body := "version: 1\nservices:\n  - name: getty\n    type: systemd\n    unit: getty@tty1.service\n" +
		"    check_interval: 1m\n    timeout: 3s\n    transition_grace: 0s\n" +
		"    failure_policy: {consecutive_failures: 2, consecutive_successes: 3}\n    notifications: [ops]\n    enabled: false\n"
	in, err := configure(t, map[string]string{"supervisor/a.yaml": body}, nil)
	if err != nil {
		t.Fatal(err)
	}
	def := in.byName["getty"].def
	if def.Unit.Name.String() != "getty@tty1.service" || def.Unit.TransitionGrace != 0 || def.CheckInterval != time.Minute ||
		def.Timeout != 3*time.Second || def.Policy != (Policy{2, 3}) || !def.Disabled || len(def.Route.Channels) != 1 {
		t.Errorf("def = %+v", def)
	}
}

// Every problem is located by file, path and line, and none quotes the
// unit's value, which may come from the environment (D-006).
func TestUnitProblems(t *testing.T) {
	body := "version: 1\nservices:\n" +
		"  - name: a\n    unit: nginx.socket\n" + // line 4
		"  - name: b\n    unit: \"bad/name.service\"\n" +
		"  - name: c\n    unit: tmpl@.service\n" +
		"  - name: d\n    unit: d.service\n    recovery:\n" + // line 11
		"  - name: e\n    unit: e.service\n    url: https://e.example.org/\n" +
		"  - name: f\n    type: systemd\n" +
		"  - name: g\n    unit: g.service\n    transition_grace: 11m\n" +
		"  - name: h\n    unit: h.service\n    limits: {memory: 1GiB}\n    startup_grace: 1s\n" +
		"  - name: i\n    unit: i.service\n" + // line 24
		"  - name: j\n    unit: i.service\n"
	_, err := configure(t, map[string]string{"supervisor/a.yaml": body}, nil)
	requireProblems(t, err,
		[]string{"a.yaml", "services[0].unit", "line 4", ".service unit"},
		[]string{"services[1].unit", "line 6", "character"},
		[]string{"services[2].unit", "template"},
		[]string{"services[3].recovery", "line 11", "must be a mapping"},
		[]string{"services[4]", "ambiguous"},
		[]string{"services[5]", "unit is required"},
		[]string{"services[6].transition_grace", "between"},
		[]string{"services[7].limits", "process services"},
		[]string{"services[7].startup_grace", "transition_grace"},
		[]string{"services[9].unit", "line 27", "already observed by the service in", "line 25"},
	)
	for _, secret := range []string{"bad/name", "tmpl@"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("a problem quotes the value %q:\n%v", secret, err)
		}
	}
}

// Restarts are asked twice: a recovery mapping on the service and the
// central switch (D-095, Q1); each without the other is a warning.
func TestUnitRecoveryConfig(t *testing.T) {
	defaults := Recovery{MaxAttempts: defaultMaxAttempts, Window: defaultWindow, Delay: defaultDelay,
		Backoff: config.BackoffExponential, MaxDelay: defaultMaxDelay, StableAfter: defaultStableAfter,
		Cooldown: defaultCooldown, RestartTimeout: 2 * time.Minute}
	tests := []struct {
		name     string
		central  string
		service  string
		restart  bool
		recovery Recovery
		warning  string
	}{
		{"absent: observed only", centralFile, "", false, defaults, ""},
		{"empty mapping: the defaults", centralFile + "    systemd_restart: true\n", "    recovery: {}\n", true, defaults, ""},
		{"keys", centralFile + "    systemd_restart: true\n",
			"    recovery: {max_attempts: 2, window: 1h, restart_timeout: 15m, cooldown: \"0s\"}\n", true,
			func() Recovery {
				r := defaults
				r.MaxAttempts, r.Window, r.RestartTimeout, r.Cooldown = 2, time.Hour, 15*time.Minute, 0
				return r
			}(), ""},
		{"switch off", centralFile, "    recovery: {}\n", true, defaults, "systemd_restart is false"},
		{"switch on, nothing asks", centralFile + "    systemd_restart: true\n", "", false, defaults, "nothing is restarted"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in, err := configure(t, map[string]string{"sentinel.yaml": tt.central,
				"supervisor/a.yaml": "version: 1\nservices:\n  - name: web\n    unit: nginx.service\n" + tt.service}, nil)
			if err != nil {
				t.Fatal(err)
			}
			def := in.byName["web"].def
			if def.Unit.HasRecovery != tt.restart || def.Recovery != tt.recovery {
				t.Errorf("restart %v, recovery %+v; want %v, %+v", def.Unit.HasRecovery, def.Recovery, tt.restart, tt.recovery)
			}
			got := ""
			for _, w := range in.warnings {
				got += w.Message
			}
			if (tt.warning == "") != (got == "") || !strings.Contains(got, tt.warning) {
				t.Errorf("warnings %q, want %q", got, tt.warning)
			}
		})
	}
}

func TestUnitRecoveryProblems(t *testing.T) {
	body := "version: 1\nservices:\n" +
		"  - name: a\n    unit: a.service\n    recovery: {restart_timeout: 30s}\n" + // line 5
		"  - name: b\n    unit: b.service\n    recovery: {restart_timeout: 16m}\n" +
		"  - name: c\n    unit: c.service\n    recovery: restart\n" +
		"  - name: w\n    command: /bin/true\n    recovery: {restart_timeout: 2m}\n" // line 14
	_, err := configure(t, map[string]string{"supervisor/a.yaml": body}, nil)
	requireProblems(t, err,
		[]string{"services[0].recovery.restart_timeout", "line 5", "between"},
		[]string{"services[1].recovery.restart_timeout", "between"},
		[]string{"services[2].recovery", "mapping"},
		[]string{"services[3].recovery.restart_timeout", "line 14", "systemd services"},
	)
}

func TestUnitNameFromEnvironmentNotQuoted(t *testing.T) {
	mc, err := moduleConfig(t, map[string]string{"supervisor/a.yaml": "version: 1\nservices:\n  - name: a\n    unit: \"${UNIT}\"\n"},
		map[string]string{"UNIT": "s3cr3t/x.service"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = New(Options{}).Configure(mc)
	requireProblems(t, err, []string{"services[0].unit"})
	if strings.Contains(err.Error(), "s3cr3t") {
		t.Errorf("the expanded value is quoted:\n%v", err)
	}
}

// The shipped example stays valid.
func TestUnitExampleFile(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "configs", "supervisor", "systemd.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	mc, err := moduleConfig(t, map[string]string{"supervisor/systemd.yaml": string(data)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(Options{}).Configure(mc)
	if err != nil {
		t.Fatalf("configs/supervisor/systemd.yaml: %v", err)
	}
	if in := c.(*instance); len(in.services) != 2 || len(in.Warnings()) != 0 {
		t.Errorf("%d services, warnings %v", len(in.services), in.Warnings())
	}
}

// Validation never reaches systemd: Configure builds no reader.
func TestUnitConfigureDoesNotDial(t *testing.T) {
	mc, err := moduleConfig(t, map[string]string{"supervisor/a.yaml": "version: 1\nservices:\n  - name: a\n    unit: a.service\n"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{NewUnitClient: func() unitClient { t.Fatal("a reader was built at Configure"); return nil }}
	if _, err := New(opts).Configure(mc); err != nil {
		t.Fatal(err)
	}
}
