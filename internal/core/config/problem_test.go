package config

import (
	"os"
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
