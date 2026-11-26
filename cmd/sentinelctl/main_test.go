package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runCmd(args ...string) (int, string, string) {
	var stdout, stderr strings.Builder
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func writeConfig(t *testing.T, extra string) string {
	t.Helper()
	dir := t.TempDir()
	body := fmt.Sprintf("version: 1\ndaemon:\n  state_dir: %s\n%s", filepath.Join(dir, "state"), extra)
	path := filepath.Join(dir, "sentinel.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCommands(t *testing.T) {
	valid := writeConfig(t, "")
	invalid := writeConfig(t, "modules:\n  firewall: {enabled: true}\n")
	tests := []struct {
		name   string
		args   []string
		code   int
		stdout string
		stderr string
	}{
		{"no command", nil, exitUsage, "", "usage:"},
		{"help", []string{"help"}, exitOK, "usage:", ""},
		{"version", []string{"version"}, exitOK, "sentinelctl ", ""},
		{"version with argument", []string{"version", "x"}, exitUsage, "", "no arguments"},
		{"unknown", []string{"frobnicate"}, exitUsage, "", "unknown command"},
		{"config without show", []string{"config"}, exitUsage, "", "unknown command"},
		{"valid", []string{"validate", "-config", valid}, exitOK, "configuration is valid", ""},
		{"valid, all", []string{"validate", "-all", "-config", valid}, exitOK, "configuration is valid", ""},
		{"invalid", []string{"validate", "-config", invalid}, exitFailure, "error: ", ""},
		{"missing file", []string{"validate", "-config", filepath.Join(t.TempDir(), "none.yaml")}, exitFailure, "error: ", ""},
		{"bad flag", []string{"validate", "-nope"}, exitUsage, "", "flag provided but not defined"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, out, errOut := runCmd(tt.args...)
			if code != tt.code || !strings.Contains(out, tt.stdout) || !strings.Contains(errOut, tt.stderr) {
				t.Errorf("exit %d (want %d)\nstdout: %s\nstderr: %s", code, tt.code, out, errOut)
			}
		})
	}
}

// validate warns about a program that will run as root, whoever runs it
// (D-105, Q-A of review 3e-2); a disabled supervisor runs nothing, even
// when -all validates its files.
func TestValidateRootWarning(t *testing.T) {
	tests := []struct {
		name, enabled string
		args          []string
		want          bool
	}{
		{"enabled", "true", nil, true},
		{"disabled, all", "false", []string{"-all"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeConfig(t, "modules:\n  supervisor: {enabled: "+tt.enabled+", processes: true}\n")
			dir := filepath.Join(filepath.Dir(path), "supervisor")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "a.yaml"), []byte("version: 1\nservices:\n  - {name: w, command: /bin/sh}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			code, out, errOut := runCmd(append(append([]string{"validate"}, tt.args...), "-config", path)...)
			if code != exitOK || strings.Contains(out, "runs as root") != tt.want {
				t.Errorf("exit %d, root warning want %v\nstdout: %s\nstderr: %s", code, tt.want, out, errOut)
			}
		})
	}
}
