package config

import (
	"io/fs"
	"os"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

// section parses a YAML mapping into a Section for tests.
func section(t *testing.T, src string) Section {
	t.Helper()
	root, _, err := parseDocument([]byte(src), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	return Section{File: "test.yaml", node: root}
}

type gateConfig struct {
	Mode    string   `yaml:"mode"`
	DryRun  bool     `yaml:"dry_run"`
	Timeout Duration `yaml:"timeout"`
}

func TestSectionDecode(t *testing.T) {
	got := gateConfig{Mode: "read_only", DryRun: true} // defaults set by the module
	if err := section(t, "dry_run: false\ntimeout: 2m\n").Decode(&got); err != nil {
		t.Fatal(err)
	}
	want := gateConfig{Mode: "read_only", DryRun: false, Timeout: Duration(2 * time.Minute)}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestSectionDecodeErrors(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want []string
	}{
		{"unknown key", "mode: x\ndryrun: true\n", []string{"test.yaml", `line 2: unknown field "dryrun"`, "valid fields: dry_run, mode, timeout"}},
		{"wrong type", "dry_run: [true]\n", []string{"test.yaml", "line 1"}},
		{"bad duration", "timeout: soon\n", []string{"test.yaml", "invalid duration"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var c gateConfig
			requireProblem(t, section(t, tt.src).Decode(&c), tt.want...)
		})
	}
}

func TestSectionZeroAndHelpers(t *testing.T) {
	var zero Section
	c := gateConfig{Mode: "keep"}
	if err := zero.Decode(&c); err != nil || c.Mode != "keep" {
		t.Errorf("zero Decode changed the target or failed: %+v, %v", c, err)
	}
	if !zero.IsZero() || zero.Line() != 0 || zero.Keys() != nil || zero.Has("x") {
		t.Error("zero Section helpers")
	}
	if err := zero.Decode(c); err == nil {
		t.Error("Decode into a non-pointer succeeded")
	}

	s := section(t, "b: 1\na: 2\n")
	if got := strings.Join(s.Keys(), ","); got != "b,a" {
		t.Errorf("Keys() = %q, want file order b,a", got)
	}
	if s.Line() != 1 || !s.Has("a") || s.IsZero() {
		t.Error("Section helpers")
	}
	if w := without(s.node, "b"); len(w.Content) != 2 || len(s.node.Content) != 4 {
		t.Error("without must copy, not modify")
	}
}

func TestSectionDecodeInlineAndSkippedFields(t *testing.T) {
	type Common struct {
		Name string `yaml:"name"`
	}
	type item struct {
		Common   `yaml:",inline"`
		Value    int    `yaml:"value"`
		Internal string `yaml:"-"`
	}
	var got struct {
		Items []item `yaml:"items"`
	}
	if err := section(t, "items:\n  - {name: a, value: 1}\n").Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 1 || got.Items[0].Name != "a" || got.Items[0].Value != 1 {
		t.Errorf("got %+v", got)
	}
	err := section(t, "items:\n  - {name: a, internal: x}\n").Decode(&got)
	requireProblem(t, err, `unknown field "internal"`, "valid fields: name, value")
}

// An inline map collects the keys that match no field, as in yaml.v3; their
// values are still checked against the map's element type.
func TestSectionDecodeInlineMap(t *testing.T) {
	type labels struct {
		Name  string         `yaml:"name"`
		Extra map[string]int `yaml:",inline"`
	}
	var got labels
	if err := section(t, "name: a\nx: 1\ny: 2\n").Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Name != "a" || got.Extra["x"] != 1 || got.Extra["y"] != 2 {
		t.Errorf("got %+v", got)
	}
	var nested struct {
		Items map[string]labels `yaml:",inline"`
	}
	err := section(t, "first: {name: a, x: {bad: 1}}\n").Decode(&nested)
	requireProblem(t, err, "line 1")
}

func TestValidationErrorMessage(t *testing.T) {
	err := &ValidationError{Problems: []Problem{
		{File: "/etc/sentinel/sentinel.yaml", Path: "daemon.socket", Message: "is required"},
		{Message: "plain"},
	}}
	want := "invalid configuration (2 problem(s)):\n  - /etc/sentinel/sentinel.yaml: daemon.socket: is required\n  - plain"
	if err.Error() != want {
		t.Errorf("Error() =\n%s\nwant\n%s", err.Error(), want)
	}
	if got := ProblemsOf(os.ErrNotExist, "f", "p"); len(got) != 1 || got[0].File != "f" || got[0].Path != "p" {
		t.Errorf("ProblemsOf(plain error) = %+v", got)
	}
	if ProblemsOf(nil, "f", "p") != nil {
		t.Error("ProblemsOf(nil) != nil")
	}
}

func TestCheckKnownFieldsBoundsAliasExpansion(t *testing.T) {
	// Each level references the previous one ten times: walking the last
	// item naively would visit about ten million nodes.
	src := "- &a [x, x, x, x, x, x, x, x, x, x]\n"
	prev := "a"
	for _, name := range []string{"b", "c", "d", "e", "f", "g"} {
		refs := strings.TrimSuffix(strings.Repeat("*"+prev+", ", 10), ", ")
		src += "- &" + name + " [" + refs + "]\n"
		prev = name
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
		t.Fatal(err)
	}
	type deep [][][][][][][][]string
	start := time.Now()
	err := checkKnownFields(&doc, reflect.TypeFor[deep]())
	if err == nil || !strings.Contains(err.Error(), "too complex") {
		t.Fatalf("err = %v, want a complexity error", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("walk took %s", elapsed)
	}
}

// fakeInfo is a fs.FileInfo with chosen mode and owner.
type fakeInfo struct {
	mode fs.FileMode
	sys  any
}

func (f fakeInfo) Name() string       { return "f" }
func (f fakeInfo) Size() int64        { return 0 }
func (f fakeInfo) Mode() fs.FileMode  { return f.mode }
func (f fakeInfo) ModTime() time.Time { return time.Time{} }
func (f fakeInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakeInfo) Sys() any           { return f.sys }

func TestCheckOwnership(t *testing.T) {
	euid := os.Geteuid()
	other := euid + 1
	if euid == 0 {
		other = 4242
	}
	tests := []struct {
		name string
		info fakeInfo
		want string // "" = allowed
	}{
		{"root owned", fakeInfo{0o644, &syscall.Stat_t{Uid: 0}}, ""},
		{"owned by the daemon user", fakeInfo{0o600, &syscall.Stat_t{Uid: uint32(euid)}}, ""},
		{"owned by another user", fakeInfo{0o644, &syscall.Stat_t{Uid: uint32(other)}}, "must be owned by root"},
		{"group writable", fakeInfo{0o664, &syscall.Stat_t{Uid: 0}}, "writable by group or others"},
		{"world writable directory", fakeInfo{fs.ModeDir | 0o757, &syscall.Stat_t{Uid: 0}}, "writable by group or others"},
		{"no owner information", fakeInfo{0o644, nil}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkOwnership(tt.info, euid)
			switch {
			case tt.want == "" && err != nil:
				t.Errorf("unexpected error %v", err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Errorf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestCheckAncestor(t *testing.T) {
	euid := os.Geteuid()
	other := euid + 1
	if euid == 0 {
		other = 4242
	}
	tests := []struct {
		name string
		info fakeInfo
		want string
	}{
		{"root directory", fakeInfo{fs.ModeDir | 0o755, &syscall.Stat_t{Uid: 0}}, ""},
		{"sticky world-writable (like /tmp)", fakeInfo{fs.ModeDir | fs.ModeSticky | 0o777, &syscall.Stat_t{Uid: 0}}, ""},
		{"world-writable without sticky bit", fakeInfo{fs.ModeDir | 0o777, &syscall.Stat_t{Uid: 0}}, "without the sticky bit"},
		{"owned by another user", fakeInfo{fs.ModeDir | 0o755, &syscall.Stat_t{Uid: uint32(other)}}, "must be owned by root"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkAncestor(tt.info, euid)
			switch {
			case tt.want == "" && err != nil:
				t.Errorf("unexpected error %v", err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Errorf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestRedacted(t *testing.T) {
	cfg := mustLoad(t, map[string]string{"sentinel.yaml": `
version: 1
notifications:
  channels:
    - name: w
      type: webhook
      url: https://hooks.example.org/services/T000/SECRET
      headers: {Authorization: Bearer tok, User-Agent: sentinel}
`}, nil)
	r := cfg.Redacted()
	ch := r.Notifications.Channels[0]
	if strings.Contains(ch.URL, "SECRET") || ch.Headers["Authorization"] == "Bearer tok" || ch.Headers["User-Agent"] != "sentinel" {
		t.Errorf("redacted channel = %+v", ch)
	}
	if orig := cfg.Notifications.Channels[0]; !strings.Contains(orig.URL, "SECRET") || orig.Headers["Authorization"] != "Bearer tok" {
		t.Error("Redacted modified the original configuration")
	}
}
