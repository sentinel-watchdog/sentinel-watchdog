package archtest

import (
	"bufio"
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

const modulePath = "github.com/sentinel-watchdog/sentinel-watchdog"

// violation returns why package pkg may not import imp under the rules of
// ADR-0015, or "" if the import is allowed. Both are full import paths.
//
//   - internal/core imports neither internal/platform, internal/modules,
//     internal/daemon nor cmd;
//   - internal/platform imports neither internal/modules, internal/daemon
//     nor cmd;
//   - a module imports neither another module, internal/daemon nor cmd;
//   - pkg (public types) imports nothing under internal.
//
// Imports outside this Go module are governed by the dependency policy
// (ADR-0013), not by these rules.
func violation(pkg, imp string) string {
	if !within(imp, modulePath) {
		return ""
	}
	p := strings.TrimPrefix(pkg, modulePath+"/")
	i := strings.TrimPrefix(imp, modulePath+"/")
	upper := within(i, "internal/daemon") || within(i, "cmd")

	switch {
	case within(p, "internal/core"):
		if upper || within(i, "internal/platform") || within(i, "internal/modules") {
			return "internal/core must not depend on platform adapters, modules, the daemon or commands"
		}
	case within(p, "internal/platform"):
		if upper || within(i, "internal/modules") {
			return "internal/platform must not depend on modules, the daemon or commands"
		}
	case within(p, "internal/modules"):
		if upper {
			return "a module must not depend on the daemon or commands"
		}
		if within(i, "internal/modules") && moduleOf(i) != moduleOf(p) {
			return "a module must not import another module; use events or an interface injected by the daemon"
		}
	case within(p, "pkg"):
		if within(i, "internal") {
			return "public packages under pkg must not depend on internal packages"
		}
	}
	return ""
}

// within reports whether path is prefix or below it.
func within(path, prefix string) bool {
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

// moduleOf returns "internal/modules/<name>" for a path below a module.
func moduleOf(p string) string {
	parts := strings.SplitN(p, "/", 4)
	if len(parts) < 3 {
		return p
	}
	return strings.Join(parts[:3], "/")
}

func TestViolationRules(t *testing.T) {
	m := func(s string) string { return modulePath + "/" + s }
	tests := []struct {
		pkg, imp string
		allowed  bool
	}{
		{m("internal/core/config"), m("internal/core/redact"), true},
		{m("internal/core/config"), "go.yaml.in/yaml/v3", true},
		{m("internal/core/module"), m("internal/modules/supervisor"), false},
		{m("internal/core/events"), m("internal/platform/executor"), false},
		{m("internal/core/config"), m("internal/daemon"), false},
		{m("internal/platform/systemd"), m("internal/core/clock"), true},
		{m("internal/platform/systemd"), m("internal/modules/supervisor"), false},
		{m("internal/modules/supervisor"), m("internal/modules/supervisor/recovery"), true},
		{m("internal/modules/supervisor/recovery"), m("internal/modules/supervisor"), true},
		{m("internal/modules/supervisor"), m("internal/platform/executor"), true},
		{m("internal/modules/supervisor"), m("internal/modules/firewall"), false},
		{m("internal/modules/firewall/nftables"), m("internal/modules/supervisor/recovery"), false},
		{m("internal/modules/firewall"), m("internal/daemon"), false},
		{m("internal/modules/firewall"), m("cmd/sentineld"), false},
		{m("internal/daemon"), m("internal/modules/firewall"), true},
		{m("cmd/sentineld"), m("internal/daemon"), true},
		{m("pkg/model"), m("internal/core/config"), false},
		{m("pkg/api"), m("pkg/model"), true},
	}
	for _, tt := range tests {
		t.Run(strings.TrimPrefix(tt.pkg, modulePath+"/")+"->"+strings.TrimPrefix(tt.imp, modulePath+"/"), func(t *testing.T) {
			if got := violation(tt.pkg, tt.imp) == ""; got != tt.allowed {
				t.Errorf("allowed = %v, want %v (%s)", got, tt.allowed, violation(tt.pkg, tt.imp))
			}
		})
	}
}

// TestDependencyRules checks every package of the repository, including
// the imports of its tests.
func TestDependencyRules(t *testing.T) {
	const format = `{{.ImportPath}}{{range .Imports}} {{.}}{{end}}` +
		`{{range .TestImports}} {{.}}{{end}}{{range .XTestImports}} {{.}}{{end}}`
	cmd := exec.CommandContext(t.Context(), "go", "list", "-f", format, modulePath+"/...")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, stderr.String())
	}

	packages := 0
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 {
			continue
		}
		packages++
		pkg := fields[0]
		for _, imp := range fields[1:] {
			if why := violation(pkg, imp); why != "" {
				t.Errorf("%s imports %s: %s", pkg, imp, why)
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if packages == 0 {
		t.Fatal("go list returned no packages")
	}
}
