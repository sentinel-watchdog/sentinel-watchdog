package config

import (
	"os"
	"strings"
	"testing"
)

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

// Destination types can contain spaces (struct { Count int }); the quoted,
// shortened value must still disappear from type errors.
func TestTypeErrorsDropTheQuotedValue(t *testing.T) {
	type spaced struct {
		Value struct{ Count int } `yaml:"value"`
	}
	type mapped struct {
		Value map[string]any `yaml:"value"`
	}
	for name, dst := range map[string]any{"struct": &spaced{}, "map": &mapped{}} {
		t.Run(name, func(t *testing.T) {
			root, secrets, err := parseDocument([]byte("value: '${TOKEN}'\n"), env(map[string]string{"TOKEN": "s3cret-token"}))
			if err != nil {
				t.Fatal(err)
			}
			err = Section{File: "f.yaml", node: root, secrets: secrets}.Decode(dst)
			if err == nil {
				t.Fatal("expected a type error")
			}
			if strings.Contains(err.Error(), "s3cret") {
				t.Fatalf("the quoted value leaked: %v", err)
			}
		})
	}
}
