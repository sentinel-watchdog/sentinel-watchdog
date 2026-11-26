package supervisor

import (
	"strings"
	"testing"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/clock"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/platform/privilege"
)

// rootLookup resolves root and worker, for the root warning.
var rootLookup = privilege.Lookup{
	User: func(name string) (uint32, uint32, error) {
		switch name {
		case "root":
			return 0, 0, nil
		case "worker":
			return 1001, 1001, nil
		}
		return 0, 0, privilege.ErrUnknownUser
	},
	Group: func(string) (uint32, error) { return 0, privilege.ErrUnknownGroup },
}

// D7: a process service or job without `user` runs as sentineld, so as
// root when sentineld is root. That is allowed, never blocked, but said
// once per program; `user: root` says it is meant and silences it. A
// program that cannot run (processes: false, enabled: false) is not
// warned about.
func TestRootWarning(t *testing.T) {
	const marker = "runs as root"
	tests := []struct {
		name    string
		central string
		euid    int
		user    string // "" = no user key
		want    bool
	}{
		{"root, no user", armedCentral, 0, "", true},
		{"root, user root", armedCentral, 0, "root", false},
		{"root, user worker", armedCentral, 0, "worker", false},
		{"not root, no user", armedCentral, 1000, "", false},
		{"root, processes off", centralFile, 0, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user := ""
			if tt.user != "" {
				user = ", user: " + tt.user
			}
			// The name comes from the environment and is too short to be
			// masked: the warning locates the program by file and line.
			// An http service runs nothing; disabled programs never run.
			body := "version: 1\nservices:\n  - {name: \"${P}\", command: /bin/sh" + user + "}\n" +
				"  - {name: web, url: \"https://h/\"}\n" +
				"  - {name: off, command: /bin/sh, enabled: false}\n" +
				"jobs:\n  - {name: nightly, schedule: \"@daily\", command: /bin/sh" + user + "}\n" +
				"  - {name: offjob, schedule: \"@daily\", command: /bin/sh, enabled: false}\n"
			mc, err := moduleConfig(t, map[string]string{"sentinel.yaml": tt.central, "supervisor/a.yaml": body}, map[string]string{"P": "zq"})
			if err != nil {
				t.Fatal(err)
			}
			c, err := New(Options{NewChecker: newFakes().build, Lookup: rootLookup, EUID: func() int { return tt.euid },
				NewLauncher: func(clock.Clock) (launcher, error) { return &fakeLauncher{}, nil }}).Configure(mc)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, w := range c.(*instance).warnings {
				if strings.Contains(w.Message, marker) {
					got = append(got, w.String())
				}
			}
			if !tt.want {
				if len(got) != 0 {
					t.Errorf("unexpected root warnings %v", got)
				}
				return
			}
			if len(got) != 2 || !strings.Contains(got[0], "services: line 3: this process service "+marker) ||
				!strings.Contains(got[1], "jobs: line 7: this job "+marker) || !strings.Contains(got[0], "user: root") {
				t.Errorf("root warnings = %v", got)
			}
			if strings.Contains(strings.Join(got, "\n"), "zq") {
				t.Errorf("a warning quotes the expanded name: %v", got)
			}
		})
	}
}

// B-001: the warnings about the central switches name the line of the
// switch, or of the module block when the switch is left out (line 8:
// the block starts at its first key).
func TestSwitchWarningsCarryTheirLine(t *testing.T) {
	body := "version: 1\nservices:\n  - {name: a, command: /bin/sh}\n" +
		"jobs:\n  - {name: j, schedule: \"@daily\", command: /bin/sh}\n"
	in := mustConfigureProcs(t, map[string]string{"sentinel.yaml": centralFile + "    systemd_restart: true\n",
		"supervisor/a.yaml": body}, &fakeLauncher{})
	want := []string{
		"modules.supervisor.processes: line 8: 1 process services",
		"modules.supervisor.processes: line 8: 1 jobs",
		"modules.supervisor.systemd_restart: line 9: modules.supervisor.systemd_restart is true",
	}
	for _, w := range want {
		found := false
		for _, p := range in.warnings {
			found = found || strings.Contains(p.String(), w)
		}
		if !found {
			t.Errorf("no warning containing %q in %v", w, in.warnings)
		}
	}
}

// B-027: "/" is refused where a file is meant, never as a working
// directory, where it is an ordinary choice.
func TestRootDirectoryAsWorkingDir(t *testing.T) {
	body := "version: 1\nservices:\n  - {name: a, command: /bin/sh, working_dir: /}\n"
	in := mustConfigureProcs(t, map[string]string{"supervisor/a.yaml": body}, &fakeLauncher{})
	if got := in.services[0].def.Process.WorkingDir; got != "/" {
		t.Errorf("working_dir = %q", got)
	}
}
