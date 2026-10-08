package config

import (
	"strings"
	"testing"
	"time"
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
