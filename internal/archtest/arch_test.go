package archtest

import (
	"bufio"
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

const modulePath = "github.com/sentinel-watchdog/sentinel-watchdog"

// legacy lists prototype packages still present until their phase
// replaces them (D-064). They may import only pkg and each other.
var legacy = []string{"internal/state"}

// yamlOwners may import the YAML library; modules decode their sections
// through config.Section instead (ADR-0015 rule 7).
var yamlOwners = []string{"internal/core/config"}

// violation returns why package pkg may not import imp under the rules of
// ADR-0015, or "" if the import is allowed. Both are full import paths.
// The rules are allowlists:
//
//   - internal/core may import internal/core, internal/version and pkg;
//   - internal/platform may also import internal/platform;
//   - internal/modules/<name> may also import its own sub-packages, never
//     another module;
//   - internal/daemon and cmd may import everything;
//   - pkg may import only pkg;
//   - only the packages in yamlOwners may import the YAML library.
//
// Other external imports are governed by the dependency policy (ADR-0013).
func violation(pkg, imp string) string {
	p := strings.TrimPrefix(pkg, modulePath+"/")
	if within(imp, "go.yaml.in/yaml") && !anyWithin(p, yamlOwners) && !within(p, "internal/archtest") {
		return "only internal/core/config may import the YAML library; decode through config.Section"
	}
	if !within(imp, modulePath) {
		return ""
	}
	i := strings.TrimPrefix(imp, modulePath+"/")

	switch {
	case within(p, "internal/daemon"), within(p, "cmd"), within(p, "internal/archtest"):
		return ""
	case within(p, "pkg"):
		if !within(i, "pkg") {
			return "public packages under pkg may import only pkg"
		}
	case within(p, "internal/core"):
		if !anyWithin(i, []string{"internal/core", "internal/version", "pkg"}) {
			return "internal/core may import only internal/core, internal/version and pkg"
		}
	case within(p, "internal/platform"):
		if !anyWithin(i, []string{"internal/core", "internal/platform", "internal/version", "pkg"}) {
			return "internal/platform may import only internal/core, internal/platform, internal/version and pkg"
		}
	case within(p, "internal/modules"):
		if within(i, "internal/modules") {
			if moduleOf(i) != moduleOf(p) {
				return "a module must not import another module; use events or an interface injected by the daemon"
			}
			return ""
		}
		if !anyWithin(i, []string{"internal/core", "internal/platform", "internal/version", "pkg"}) {
			return "a module may import only internal/core, internal/platform, its own packages and pkg"
		}
	case within(p, "internal/version"):
		return "internal/version is a leaf and imports nothing from this repository"
	case anyWithin(p, legacy):
		if !anyWithin(i, legacy) && !within(i, "pkg") {
			return "prototype packages may import only pkg and other prototype packages"
		}
	default:
		return "package outside the ADR-0015 layers (internal/core, internal/platform, internal/modules, internal/daemon, cmd, pkg)"
	}
	return ""
}

func anyWithin(path string, prefixes []string) bool {
	for _, p := range prefixes {
		if within(path, p) {
			return true
		}
	}
	return false
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
		// Allowlist cases found in review.
		{m("internal/core/config"), m("internal/state"), false},
		{m("internal/state"), m("internal/modules/supervisor"), false},
		{m("internal/state"), m("pkg/model"), true},
		{m("internal/modules/supervisor"), "go.yaml.in/yaml/v3", false},
		{m("internal/core/config"), "go.yaml.in/yaml/v3", true},
		{m("internal/core/module"), "go.yaml.in/yaml/v3", false},
		{m("internal/newthing"), m("pkg/model"), false},
		{m("internal/core/clock"), m("internal/version"), true},
	}
	for _, tt := range tests {
		t.Run(strings.TrimPrefix(tt.pkg, modulePath+"/")+"->"+strings.TrimPrefix(tt.imp, modulePath+"/"), func(t *testing.T) {
			if got := violation(tt.pkg, tt.imp) == ""; got != tt.allowed {
				t.Errorf("allowed = %v, want %v (%s)", got, tt.allowed, violation(tt.pkg, tt.imp))
			}
		})
	}
}

// buildConfigs are the build configurations whose imports are checked:
// the host default, and Linux with the integration tag (Linux-only and
// tagged files would otherwise escape the check).
var buildConfigs = []struct{ goos, tags string }{
	{"", ""},
	{"linux", "integration"},
}

// TestDependencyRules checks every package of the repository, including
// the imports of its tests.
func TestDependencyRules(t *testing.T) {
	const format = `{{.ImportPath}}{{range .Imports}} {{.}}{{end}}` +
		`{{range .TestImports}} {{.}}{{end}}{{range .XTestImports}} {{.}}{{end}}`
	for _, bc := range buildConfigs {
		args := []string{"list", "-f", format}
		if bc.tags != "" {
			args = append(args, "-tags", bc.tags)
		}
		cmd := exec.CommandContext(t.Context(), "go", append(args, modulePath+"/...")...)
		if bc.goos != "" {
			cmd.Env = append(cmd.Environ(), "GOOS="+bc.goos)
		}
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
		}

		packages := 0
		sc := bufio.NewScanner(bytes.NewReader(out))
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) == 0 {
				continue
			}
			packages++
			for _, imp := range fields[1:] {
				if why := violation(fields[0], imp); why != "" {
					t.Errorf("[GOOS=%s tags=%s] %s imports %s: %s", bc.goos, bc.tags, fields[0], imp, why)
				}
			}
		}
		if err := sc.Err(); err != nil {
			t.Fatal(err)
		}
		if packages == 0 {
			t.Fatalf("go list returned no packages for GOOS=%q tags=%q", bc.goos, bc.tags)
		}
	}
}
