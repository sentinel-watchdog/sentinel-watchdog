package module_test

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/config"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/module"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/api"
)

// commanding is a test module that declares commands; its configured
// module handles them unless mute is set.
type commanding struct {
	name     string
	commands []api.Command
	mute     bool
}

func (c commanding) Name() string            { return c.name }
func (c commanding) Commands() []api.Command { return c.commands }
func (c commanding) Configure(config.ModuleConfig) (module.Configured, error) {
	if c.mute {
		return &echoConfigured{}, nil
	}
	return handling{}, nil
}

type handling struct{}

func (handling) Start(context.Context, module.Runtime) error { return nil }
func (handling) Stop(context.Context) error                  { return nil }
func (handling) Handle(context.Context, string, jsontext.Value) (any, *api.Error) {
	return nil, nil
}

// panicCommands is a module whose Commands method panics.
type panicCommands struct{ commanding }

func (panicCommands) Commands() []api.Command { panic("s3cret") }

func TestRegisterCommands(t *testing.T) {
	list := api.Command{Name: "cmd.list", Tier: api.TierRead, Summary: "list"}
	restart := api.Command{Name: "cmd.restart", Tier: api.TierOperate, Summary: "restart"}
	r := module.NewRegistry()
	if err := r.Register(func() module.Module {
		return commanding{name: "cmd", commands: []api.Command{restart, list}}
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(func() module.Module { return echo{name: "echo"} }); err != nil {
		t.Fatal(err)
	}
	if err := r.Planned("future"); err != nil {
		t.Fatal(err)
	}
	got := r.Commands()
	if len(got) != 1 || !slices.Equal(got["cmd"], []api.Command{list, restart}) {
		t.Errorf("Commands() = %v, want cmd's two commands sorted by name", got)
	}
	got["cmd"][0].Tier = api.TierAdmin // a copy: the registry keeps its own
	if r.Commands()["cmd"][0].Tier != api.TierRead {
		t.Error("Commands() returned the registry's own slice")
	}

	tests := []struct {
		name     string
		commands []api.Command
		want     string
	}{
		{"other namespace", []api.Command{{Name: "core.status", Tier: api.TierRead}}, `"core.status" is not in the namespace "bad."`},
		{"invalid name", []api.Command{{Name: "bad.Restart", Tier: api.TierRead}}, `invalid command name "bad.Restart"`},
		{"no tier", []api.Command{{Name: "bad.list"}}, `command "bad.list" has an invalid tier ""`},
		{"unknown tier", []api.Command{{Name: "bad.list", Tier: "root"}}, `invalid tier "root"`},
		{"duplicate", []api.Command{{Name: "bad.list", Tier: api.TierRead}, {Name: "bad.list", Tier: api.TierAdmin}},
			`command "bad.list" declared twice`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := module.NewRegistry()
			err := r.Register(func() module.Module { return commanding{name: "bad", commands: tt.commands} })
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want %q", err, tt.want)
			}
			if _, known := r.Availability()["bad"]; known {
				t.Error("a module with invalid commands was registered")
			}
		})
	}

	t.Run("Commands panics", func(t *testing.T) {
		err := module.NewRegistry().Register(func() module.Module { return panicCommands{commanding{name: "bad"}} })
		if err == nil || !strings.Contains(err.Error(), "panicked while registering") || strings.Contains(err.Error(), "s3cret") {
			t.Errorf("err = %v, want a panic report without its value", err)
		}
	})
}

// A module that declares commands must handle them once configured: the
// wiring error is found by Configure (and so by validate), not by the
// first client.
func TestConfigureRequiresHandlerForCommands(t *testing.T) {
	r := module.NewRegistry()
	commands := []api.Command{{Name: "mute.list", Tier: api.TierRead}}
	for _, f := range []module.Factory{
		func() module.Module { return commanding{name: "mute", commands: commands, mute: true} },
		func() module.Module {
			return commanding{name: "loud", commands: []api.Command{{Name: "loud.list", Tier: api.TierRead}}}
		},
		func() module.Module { return commanding{name: "quiet", mute: true} }, // no commands, no handler
	} {
		if err := r.Register(f); err != nil {
			t.Fatal(err)
		}
	}
	cfg := loadTree(t, r, map[string]string{"sentinel.yaml": `
version: 1
modules:
  mute: {enabled: true}
  loud: {enabled: true}
  quiet: {enabled: true}
`}, false)
	_, err := r.Configure(cfg)
	requireProblem(t, err, "modules.mute", "declares commands but does not handle them")
	var ve *config.ValidationError
	if !errors.As(err, &ve) || len(ve.Problems) != 1 {
		t.Errorf("want only mute's problem, got %v", err)
	}
}
