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

// An unknown key must not hide a bad value elsewhere in the section: every
// problem is reported at once.
func TestSectionDecodeReportsUnknownKeysAndBadValuesTogether(t *testing.T) {
	var c gateConfig
	err := section(t, "dryrun: true\ntimeout: soon\n").Decode(&c)
	requireProblem(t, err, `line 1: unknown field "dryrun"`)
	requireProblem(t, err, "invalid duration")
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

// Phase 3a-3: a module locates its own nested problems through Child,
// Items and KeyLine, without the YAML library (ADR-0015 rule 7).
func TestSectionChildItemsKeyLine(t *testing.T) {
	src := "settings:\n  check_interval: 30s\nservices:\n  - name: a\n  - &b {name: b}\n  - 7\n  - *b\nempty:\nlist: [1]\n"
	s := section(t, src)
	if got := s.KeyLine("services"); got != 3 {
		t.Errorf("KeyLine(services) = %d", got)
	}
	if got := s.KeyLine("missing"); got != 0 {
		t.Errorf("KeyLine(missing) = %d", got)
	}
	settings, err := s.Child("settings")
	if err != nil || settings.File != "test.yaml" || settings.Line() != 2 || !settings.Has("check_interval") {
		t.Fatalf("Child(settings) = %+v, %v", settings, err)
	}
	for _, key := range []string{"missing", "empty"} {
		if c, err := s.Child(key); err != nil || !c.IsZero() {
			t.Errorf("Child(%s) = %+v, %v", key, c, err)
		}
	}
	if _, err := s.Child("list"); err == nil {
		t.Error("Child of a list was accepted")
	} else {
		requireProblem(t, err, "test.yaml", "line 9", "list must be a mapping")
	}
	items, err := s.Items("services")
	requireProblem(t, err, "test.yaml", "line 6", "services[2] must be a mapping")
	if len(items) != 3 {
		t.Fatalf("Items = %d sections", len(items))
	}
	var first struct {
		Name string `yaml:"name"`
	}
	if err := items[0].Decode(&first); err != nil || first.Name != "a" || items[0].Line() != 4 {
		t.Errorf("first item %+v at line %d: %v", first, items[0].Line(), err)
	}
	if items[2].Line() != 5 { // the alias resolves to the anchored mapping
		t.Errorf("aliased item at line %d", items[2].Line())
	}
	for _, key := range []string{"missing", "empty"} {
		if got, err := s.Items(key); err != nil || got != nil {
			t.Errorf("Items(%s) = %v, %v", key, got, err)
		}
	}
	if _, err := s.Items("settings"); err == nil {
		t.Error("Items of a mapping was accepted")
	} else {
		requireProblem(t, err, "line 2", "settings must be a list")
	}
}

// Phase 3a-3: a bad scalar value does not stop decoding, so every bad
// value of a section is reported at once.
func TestScalarErrorsAreCollected(t *testing.T) {
	var dst struct {
		A Duration `yaml:"a"`
		B Duration `yaml:"b"`
		C FileMode `yaml:"c"`
		D ByteSize `yaml:"d"`
		E int      `yaml:"e"`
	}
	err := section(t, "a: soon\nb: 5\nc: \"0999\"\nd: lots\ne: x\n").Decode(&dst)
	for _, want := range []string{"line 1: invalid duration", "line 2: duration must be a string",
		"line 3: invalid file mode", "line 4: invalid size", "line 5: cannot unmarshal"} {
		requireProblem(t, err, want)
	}
}

// Phase 3a-3 (Codex critique C6): scalar problems never quote the value,
// which may be a secret too short for redaction to mask.
func TestScalarErrorsDoNotEchoValues(t *testing.T) {
	for _, src := range []string{"a: ${T}\n", "c: \"${T}\"\n", "d: ${T}\n"} {
		root, secrets, err := parseDocument([]byte(src), env(map[string]string{"T": "Zq"}))
		if err != nil {
			t.Fatal(err)
		}
		var dst struct {
			A Duration `yaml:"a"`
			C FileMode `yaml:"c"`
			D ByteSize `yaml:"d"`
		}
		err = Section{File: "f.yaml", node: root, secrets: secrets}.Decode(&dst)
		if err == nil || strings.Contains(err.Error(), "Zq") {
			t.Errorf("%q: %v", src, err)
		}
	}
}
