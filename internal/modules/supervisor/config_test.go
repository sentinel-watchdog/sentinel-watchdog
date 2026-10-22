package supervisor

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/config"
)

// requireProblems fails unless every want is in one problem of err.
func requireProblems(t *testing.T, err error, wants ...[]string) {
	t.Helper()
	var ve *config.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *config.ValidationError", err)
	}
	for _, want := range wants {
		found := false
		for _, p := range ve.Problems {
			ok := true
			for _, sub := range want {
				ok = ok && strings.Contains(p.String(), sub)
			}
			found = found || ok
		}
		if !found {
			t.Errorf("no problem contains %q; got:\n%v", want, err)
		}
	}
}

// AC1: a service needs only its name and URL; everything else has a safe
// default (D-058).
func TestConfigureMinimalService(t *testing.T) {
	in, err := configure(t, map[string]string{"supervisor/web.yaml": "version: 1\nservices:\n  - name: web\n    url: https://web.example.org/health\n"},
		newFakes().build)
	if err != nil {
		t.Fatal(err)
	}
	if len(in.services) != 1 {
		t.Fatalf("services %d", len(in.services))
	}
	s := in.services[0].def
	if s.Type != "http" || s.CheckInterval != 30*time.Second || s.Timeout != 10*time.Second ||
		s.Policy != (Policy{Failures: 3, Successes: 1}) || in.settings.MaxConcurrentChecks != 16 {
		t.Errorf("defaults: %+v, settings %+v", s, in.settings)
	}
	if len(in.Warnings()) != 0 {
		t.Errorf("warnings: %v", in.Warnings())
	}
}

// Settings set the defaults of every service, whatever the file order;
// a service overrides them key by key.
func TestSettingsAndOverrides(t *testing.T) {
	in, err := configure(t, map[string]string{
		"supervisor/10-web.yaml": "version: 1\nservices:\n  - {name: web, url: \"http://web:8080/\"}\n" +
			"  - {name: api, url: \"https://api/\", check_interval: 5s, failure_policy: {consecutive_successes: 2}}\n",
		"supervisor/20-settings.yaml": "version: 1\nsettings:\n  max_concurrent_checks: 4\n  check_interval: 1m\n" +
			"  timeout: 2s\n  failure_policy: {consecutive_failures: 5}\n",
	}, newFakes().build)
	if err != nil {
		t.Fatal(err)
	}
	api, web := in.byName["api"].def, in.byName["web"].def
	if in.settings.MaxConcurrentChecks != 4 || web.CheckInterval != time.Minute || web.Timeout != 2*time.Second ||
		web.Policy != (Policy{5, 1}) || api.CheckInterval != 5*time.Second || api.Policy != (Policy{5, 2}) {
		t.Errorf("web %+v, api %+v", web, api)
	}
	if in.services[0].def.Name != "api" { // sorted by name
		t.Errorf("order %s", in.services[0].def.Name)
	}
}

// AC1: every problem of every file at once, with file and line, none
// quoting a value.
func TestConfigureReportsEveryProblem(t *testing.T) {
	_, err := configure(t, map[string]string{
		"supervisor/a.yaml": "version: 1\n" +
			"services:\n" +
			"  - name: web\n" + // 3
			"    url: https://web/\n" +
			"    method: TRACE\n" + // 5
			"    check_interval: soon\n" + // 6
			"    timeout: 1h\n" + // 7
			"  - name: db\n" + // 8
			"    type: systemd\n" + // 9
			"  - name: worker\n" + // 10
			"    type: process\n" + // 11
			"  - name: legacy\n" + // 12
			"    url: ftp://legacy/\n" + // 13
			"    recovery: {action: restart}\n" + // 14
			"  - name: Bad_Name\n" + // 15
			"    url: https://x/\n" +
			"  - name: nohost\n" + // 17
			"    url: \"https://:8080/\"\n" + // 18
			"    notifications: [ops, nowhere]\n" + // 19
			"jobs:\n" + // 20
			"  - name: backup\n" +
			"extra: 1\n", // 22
		"supervisor/b.yaml": "version: 1\nsettings:\n  max_concurrent_checks: 0\n  failure_policy: {consecutive_failures: 101}\n" +
			"services:\n  - name: web\n    url: https://web2/\n  - type: tcp\n    name: x\n  - 7\n",
	}, newFakes().build)
	requireProblems(t, err,
		[]string{"a.yaml", "services[0].method", "line 5", "GET, HEAD, POST, PUT"},
		[]string{"a.yaml", "line 6", "invalid duration"},
		[]string{"a.yaml", "services[0].timeout", "line 7", "between 100ms and 5m0s"},
		[]string{"a.yaml", "line 9", `"systemd" is planned but not implemented`},
		[]string{"a.yaml", "line 10", "a process service needs command or script"},
		[]string{"a.yaml", "services[3].url", "line 13", "http:// or https://"},
		[]string{"a.yaml", "services[3].recovery", "line 14", "D-012"},
		[]string{"a.yaml", "services[4].name", "line 15", "D-011"},
		[]string{"a.yaml", "services[5].url", "line 18", "no host"},
		[]string{"a.yaml", "services[5].notifications", "line 19", "channel 2 of the route is not a configured"},
		[]string{"a.yaml", "line 20", "jobs are planned but not implemented"},
		[]string{"a.yaml", "line 22", `unknown key "extra"`},
		[]string{"b.yaml", "settings.max_concurrent_checks", "line 3"},
		[]string{"b.yaml", "settings.failure_policy.consecutive_failures", "line 4"},
		[]string{"b.yaml", "services[0].name", "line 6", "already used", "a.yaml at line 3"},
		[]string{"b.yaml", "services[1].type", "line 8", "unknown service type"},
		[]string{"b.yaml", "line 10", "services[2] must be a mapping"},
	)
	if strings.Contains(err.Error(), "Bad_Name") || strings.Contains(err.Error(), "ftp://") || strings.Contains(err.Error(), "tcp") ||
		strings.Contains(err.Error(), "TRACE") {
		t.Errorf("a problem quotes a value:\n%v", err)
	}
}

// AC1, D-080 (6): settings in two files is the loader's problem; the
// module is then not configured (no partial loading).
func TestSettingsInTwoFilesIsALoaderProblem(t *testing.T) {
	_, err := moduleConfig(t, map[string]string{
		"supervisor/a.yaml": "version: 1\nsettings: {check_interval: 1m}\n",
		"supervisor/b.yaml": "version: 1\nsettings: {check_interval: 2m}\n",
	}, nil)
	requireProblems(t, err, []string{"b.yaml", "line 2", "settings"})
}

func TestServiceLimit(t *testing.T) {
	var b strings.Builder
	b.WriteString("version: 1\nservices:\n")
	for i := range maxServices + 1 {
		fmt.Fprintf(&b, "  - {name: s%d, url: \"https://h/\"}\n", i)
	}
	_, err := configure(t, map[string]string{"supervisor/many.yaml": b.String()}, newFakes().build)
	requireProblems(t, err, []string{"many.yaml", "more than 256 services"})
}

// ADR-0015 rule 1: the central block holds only the switch.
func TestCentralBlockRejectsKeys(t *testing.T) {
	_, err := configure(t, map[string]string{"sentinel.yaml": centralFile + "    interval: 5s\n",
		"supervisor/a.yaml": "version: 1\n"}, newFakes().build)
	requireProblems(t, err, []string{"sentinel.yaml", `unknown field "interval"`})
}

// ADR-0015 rule 4: no directory, or no services, starts empty with a warning.
func TestEmptySupervisorWarns(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"no directory": {},
		"no services":  {"supervisor/a.yaml": "version: 1\n"},
	} {
		t.Run(name, func(t *testing.T) {
			in, err := configure(t, files, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(in.Warnings()) != 1 || len(in.services) != 0 {
				t.Errorf("warnings %v", in.Warnings())
			}
		})
	}
}

// A secret in a value never reaches a problem, even one too short for
// redaction to mask.
func TestProblemsDoNotQuoteValues(t *testing.T) {
	mc, err := moduleConfig(t, map[string]string{"supervisor/a.yaml": "version: 1\nservices:\n" +
		"  - {name: web, url: \"gopher://u:${T}@h/\", check_interval: \"${T}\", timeout: \"${T}s\"}\n" +
		"  - {name: \"${T}\", url: \"https://h/?k=${T}\", type: \"${T}\"}\n"}, map[string]string{"T": "Zq"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = New(Options{NewChecker: newFakes().build}).Configure(mc)
	if err == nil || strings.Contains(err.Error(), "Zq") {
		t.Fatalf("problems quote the value:\n%v", err)
	}
}

// Review R2 (3a-3): problems of a route never quote its entries, which
// may come from the environment.
func TestRouteProblemsDoNotQuoteValues(t *testing.T) {
	for _, route := range []string{`["${T}"]`, `[ops, ops]`, `{channels: [ops], events: ["${T}"]}`} {
		mc, err := moduleConfig(t, map[string]string{"supervisor/a.yaml": "version: 1\nservices:\n" +
			"  - {name: web, url: \"https://web/\", notifications: " + route + "}\n"}, map[string]string{"T": "Zq"})
		if err != nil {
			t.Fatal(err)
		}
		_, err = New(Options{NewChecker: newFakes().build}).Configure(mc)
		if err == nil || strings.Contains(err.Error(), "Zq") || strings.Contains(err.Error(), `"ops"`) {
			t.Errorf("%s: %v", route, err)
		}
	}
}

// Review R3 (3a-3): a valid name expanded from the environment is not
// quoted either: problems locate a service by position and line.
func TestExpandedNameIsNotQuoted(t *testing.T) {
	mc, err := moduleConfig(t, map[string]string{
		"supervisor/a.yaml": "version: 1\nservices:\n  - {name: \"${T}\", url: \"ftp://h/\"}\n",
		"supervisor/b.yaml": "version: 1\nservices:\n  - {name: \"${T}\", url: \"https://h/\"}\n",
	}, map[string]string{"T": "zq"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = New(Options{NewChecker: newFakes().build}).Configure(mc)
	requireProblems(t, err, []string{"a.yaml", "services[0].url", "line 3"},
		[]string{"b.yaml", "services[0].name", "line 3", "already used", "a.yaml at line 3"})
	if strings.Contains(err.Error(), "zq") {
		t.Errorf("an expanded name was quoted:\n%v", err)
	}
}
