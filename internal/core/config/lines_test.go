package config

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// B-001: every semantic problem of the central file names the line of the
// value it is about, or of the deepest node that exists on the way when
// the value was left out. Locations are found from segments, so a channel
// or header name with a dot in it is no obstacle, and a duplicate channel
// is located at its own item, not at the first one with that name.
func TestCentralProblemsCarryTheirLine(t *testing.T) {
	main := `version: 1
daemon:
  socket: run/s.sock
  state_dir: /
notifications:
  core: [nope]
  channels:
    - name: ops.eu
      type: webhook
      url: https://x.org:0/hook
      headers:
        X.Trace: "a\x01b"
        x.trace: c
    - name: ops.eu
      type: webhook
      url: https://y.org
    - name: w
      type: webhook
    - name: ca
      type: webhook
      url: https://z.org
      tls:
        ca_file: /
modules:
  gamma: {}
  planned:
    enabled: true
`
	_, err := load(t, map[string]string{"sentinel.yaml": main}, nil)
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v", err)
	}
	want := []struct{ path, line, text string }{
		{"daemon.socket", "line 3: ", "absolute path"},
		{"daemon.state_dir", "line 4: ", "root directory"},
		{"notifications.core", "line 6: ", "not a configured notification channel"},
		{"notifications.channels[ops.eu].url", "line 10: ", "port"},
		{"notifications.channels[ops.eu].headers.X.Trace", "line 12: ", "control character"},
		{"notifications.channels[ops.eu].headers", "line 13: ", `"x.trace"`},
		{"notifications.channels[ops.eu]", "line 14: ", "duplicate channel name"},
		{"notifications.channels[w].url", "line 17: ", "is required"},
		{"notifications.channels[ca].tls.ca_file", "line 23: ", "root directory"},
		{"modules.gamma", "line 25: ", "unknown module"},
		{"modules.planned", "line 26: ", "planned but not implemented"},
	}
	for _, w := range want {
		i := slices.IndexFunc(ve.Problems, func(p Problem) bool {
			return p.Path == w.path && strings.HasPrefix(p.Message, w.line) && strings.Contains(p.Message, w.text)
		})
		if i < 0 {
			t.Errorf("no problem at %s starting %q and containing %q", w.path, w.line, w.text)
		}
	}
	for _, p := range ve.Problems {
		if !strings.HasPrefix(p.Message, "line ") {
			t.Errorf("problem without a line: %s", p)
		}
	}
	if t.Failed() {
		t.Logf("got:\n%v", err)
	}
}

// Warnings name the line of the value they are about, like problems.
func TestCentralWarningsCarryTheirLine(t *testing.T) {
	cfg := mustLoad(t, map[string]string{"sentinel.yaml": `version: 1
daemon:
  access:
    admin_group: wheel
notifications:
  channels:
    - name: w
      type: webhook
      url: "http://x.org"
      tls:
        insecure_skip_verify: true
`}, nil)
	for _, w := range []struct{ path, line string }{
		{"daemon.access.admin_group", "line 4: "},
		{"notifications.channels[w].url", "line 9: "},
		{"notifications.channels[w].tls.insecure_skip_verify", "line 11: "},
	} {
		if !slices.ContainsFunc(cfg.Warnings, func(p Problem) bool {
			return p.Path == w.path && strings.HasPrefix(p.Message, w.line)
		}) {
			t.Errorf("no warning at %s starting %q in %v", w.path, w.line, cfg.Warnings)
		}
	}
}

// Review 3e-2 (CR R4, C2 R2): yaml.v3 leaves null entries out of a decoded
// list, so the n-th value is not always the n-th item; an alias is
// located where it is written, not at its anchor.
func TestLinesSkipNullItemsAndKeepAliasLines(t *testing.T) {
	tests := []struct{ name, main, want string }{
		{"null before a channel", "version: 1\nnotifications:\n  channels:\n    - null\n    - name: w\n      type: webhook\n" +
			"      url: https://x.org:0/\n", "notifications.channels[w].url: line 7: port"},
		{"null before a status code", "version: 1\nnotifications:\n  channels:\n    - name: w\n      type: webhook\n" +
			"      url: https://x.org\n      success_status_codes:\n        - null\n        - 301\n", "success_status_codes: line 9: 301"},
		{"aliased duplicate", "version: 1\nnotifications:\n  channels:\n    - &b\n      name: ops\n      type: webhook\n" +
			"      url: https://x.org\n    - *b\n", "notifications.channels[ops]: line 8: duplicate channel name"},
		{"root socket", "version: 1\ndaemon:\n  socket: /\n", "daemon.socket: line 3: is the root directory"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := load(t, map[string]string{"sentinel.yaml": tt.main}, nil)
			requireProblem(t, err, tt.want)
		})
	}
}
