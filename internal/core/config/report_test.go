package config

import (
	"errors"
	"strings"
	"testing"
)

func TestWriteDiagnostics(t *testing.T) {
	warn := Problem{File: "/etc/sentinel/sentinel.yaml", Path: "daemon.access.admin_group", Message: "root-equivalent"}
	tests := []struct {
		name string
		cfg  *Config
		err  error
		want string
	}{
		{"valid", &Config{}, nil, ""},
		{"valid with warnings and ignored directories",
			&Config{Warnings: []Problem{warn}, IgnoredDirs: []string{"/etc/sentinel/firewall"}}, nil,
			"warning: /etc/sentinel/sentinel.yaml: daemon.access.admin_group: root-equivalent\n" +
				"ignored: /etc/sentinel/firewall (module disabled or not available)\n"},
		{"invalid, warnings kept", nil,
			&ValidationError{Problems: []Problem{{File: "f", Path: "daemon.socket", Message: "is required"}, {Message: "line 3: bad"}},
				Warnings: []Problem{warn}},
			"error: f: daemon.socket: is required\nerror: line 3: bad\n" +
				"warning: /etc/sentinel/sentinel.yaml: daemon.access.admin_group: root-equivalent\n"},
		{"plain error", nil, errors.New("open /etc/sentinel/sentinel.yaml: permission denied"),
			"error: open /etc/sentinel/sentinel.yaml: permission denied\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var b strings.Builder
			if err := WriteDiagnostics(&b, tt.cfg, tt.err); err != nil {
				t.Fatal(err)
			}
			if b.String() != tt.want {
				t.Errorf("got\n%s\nwant\n%s", b.String(), tt.want)
			}
		})
	}
}
