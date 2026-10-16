package config

import (
	"errors"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/model"
)

func TestRouteForms(t *testing.T) {
	tests := []struct {
		name, src string
		want      Route
		wantErr   string
	}{
		{"short form", "route: [ops, oncall]\n", Route{Channels: []string{"ops", "oncall"}}, ""},
		{"long form", "route: {channels: [ops], events: [daemon_error]}\n",
			Route{Channels: []string{"ops"}, Events: []model.EventType{model.EventDaemonError}}, ""},
		{"alias", "a: &r [ops]\nroute: *r\n", Route{Channels: []string{"ops"}}, ""},
		{"unknown key", "route: {channels: [ops], on: [x]}\n", Route{}, `line 1: unknown field "on"`},
		{"scalar", "route: ops\n", Route{}, "a route is a list of channel names"},
		{"not strings", "route: [[ops]]\n", Route{}, "line 1: cannot unmarshal !!seq into string"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var dst struct {
				A     []string `yaml:"a"`
				Route Route    `yaml:"route"`
			}
			err := section(t, tt.src).Decode(&dst)
			if tt.wantErr != "" {
				requireProblem(t, err, tt.wantErr)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(dst.Route.Channels, ",") != strings.Join(tt.want.Channels, ",") ||
				len(dst.Route.Events) != len(tt.want.Events) {
				t.Errorf("route = %+v, want %+v", dst.Route, tt.want)
			}
		})
	}
}

func TestRouteMatches(t *testing.T) {
	all := Route{Channels: []string{"ops"}}
	some := Route{Channels: []string{"ops"}, Events: []model.EventType{"job_failed"}}
	if !all.Matches("anything") || !some.Matches("job_failed") || some.Matches("job_succeeded") {
		t.Error("Matches does not follow the events filter")
	}
}

func TestRouteProblems(t *testing.T) {
	channels := []string{"ops", "oncall"}
	events := []model.EventType{"job_failed", "job_succeeded"}
	tests := []struct {
		name  string
		route Route
		want  []string
	}{
		{"valid", Route{Channels: []string{"ops"}, Events: []model.EventType{"job_failed"}}, nil},
		{"empty", Route{}, nil},
		{"unknown channel", Route{Channels: []string{"pager"}}, []string{`unknown notification channel "pager"`}},
		{"duplicate channel", Route{Channels: []string{"ops", "ops"}}, []string{`channel "ops" listed twice`}},
		{"unknown event", Route{Channels: []string{"ops"}, Events: []model.EventType{"failure"}}, []string{`unknown event type "failure"`}},
		{"events without channels", Route{Events: []model.EventType{"job_failed"}}, []string{"no channel is listed"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ps := tt.route.Problems("f.yaml", "jobs[backup].notifications", channels, events)
			if len(ps) != len(tt.want) {
				t.Fatalf("problems %v, want %v", ps, tt.want)
			}
			for i, p := range ps {
				if !strings.Contains(p.Message, tt.want[i]) || p.Path != "jobs[backup].notifications" {
					t.Errorf("problem %v, want %q", p, tt.want[i])
				}
			}
		})
	}
}

// Design review 2026-10-16 Q12: every problem of a route is reported, and
// a bad route does not hide the problems of the keys after it.
func TestRouteReportsEveryProblem(t *testing.T) {
	var dst struct {
		A     Route `yaml:"a"`
		B     Route `yaml:"b"`
		Count int   `yaml:"count"`
	}
	err := section(t, "a: {chanels: [ops], evnts: [x]}\nb: {channels: [[ops]]}\ncount: many\n").Decode(&dst)
	for _, want := range []string{`line 1: unknown field "chanels"`, `line 1: unknown field "evnts"`,
		"line 2: cannot unmarshal !!seq into string", "line 3: cannot unmarshal !!str"} {
		requireProblem(t, err, want)
	}
}

// Design review 2026-10-16 Q12: the problems of every joined error are
// kept, even when one of them is a YAML type error.
func TestProblemsFromYAMLKeepsJoinedErrors(t *testing.T) {
	err := errors.Join(errors.New("line 1: unknown field \"x\""), &yaml.TypeError{Errors: []string{"line 2: cannot unmarshal !!str into int"}})
	ps := problemsFromYAML("f.yaml", err)
	if len(ps) != 2 {
		t.Fatalf("problems %v", ps)
	}
}
