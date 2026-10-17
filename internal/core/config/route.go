package config

import (
	"errors"
	"fmt"
	"slices"

	"go.yaml.in/yaml/v3"

	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/model"
)

// Route sends events to notification channels (D-004). In YAML it is
// either a list of channel names:
//
//	notifications: [ops, oncall]
//
// or a mapping that also filters event types:
//
//	notifications: {channels: [ops], events: [service_failed]}
//
// No events means every event; no channels means no notification. The
// core routes its own events with notifications.core; modules use a Route
// on the items they configure.
type Route struct {
	Channels []string          `yaml:"channels" json:"channels"`
	Events   []model.EventType `yaml:"events,omitempty" json:"events,omitempty"`
}

// UnmarshalYAML accepts the short and the long form. Strict decoding
// skips types that unmarshal themselves, so unknown keys are rejected here.
// Problems are returned as a *yaml.TypeError, which yaml.v3 collects and
// then goes on decoding: every problem of the route, and of the keys after
// it, is reported at once.
func (r *Route) UnmarshalYAML(n *yaml.Node) error {
	n = resolveAlias(n)
	switch n.Kind {
	case yaml.SequenceNode:
		var channels []string
		if err := n.Decode(&channels); err != nil {
			return err // a *yaml.TypeError: its own messages carry the line
		}
		*r = Route{Channels: channels}
		return nil
	case yaml.MappingNode:
		var problems []string
		for i := 0; i+1 < len(n.Content); i += 2 {
			if key := n.Content[i]; key.Value != "channels" && key.Value != "events" {
				problems = append(problems, fmt.Sprintf("line %d: unknown field %q in a route (valid fields: channels, events)", key.Line, key.Value))
			}
		}
		type plain Route // without UnmarshalYAML, to avoid recursion
		var p plain
		err := n.Decode(&p)
		var te *yaml.TypeError
		switch {
		case errors.As(err, &te):
			problems = append(problems, te.Errors...) // their own messages carry the line
		case err != nil:
			return err
		}
		if len(problems) > 0 {
			return &yaml.TypeError{Errors: problems}
		}
		*r = Route(p)
		return nil
	}
	return fmt.Errorf("line %d: a route is a list of channel names or {channels: [...], events: [...]}", n.Line)
}

// Matches reports whether the route applies to events of type t.
func (r Route) Matches(t model.EventType) bool {
	return len(r.Events) == 0 || slices.Contains(r.Events, t)
}

// Problems checks the route against the configured channel names and the
// event types its emitter can produce, and locates each problem at file
// and path. Entries are named by their position, never quoted: a route's
// values may come from the environment, and a short secret is too short
// for redaction to mask.
func (r Route) Problems(file, path string, channels []string, events []model.EventType) []Problem {
	var ps []Problem
	seen := map[string]bool{}
	for i, c := range r.Channels {
		switch {
		case seen[c]:
			ps = append(ps, Problem{File: file, Path: path, Message: fmt.Sprintf("channel %d of the route is listed twice", i+1)})
		case !slices.Contains(channels, c):
			ps = append(ps, Problem{File: file, Path: path, Message: fmt.Sprintf("channel %d of the route is not a configured notification channel", i+1)})
		}
		seen[c] = true
	}
	if len(r.Channels) == 0 && len(r.Events) > 0 {
		ps = append(ps, Problem{File: file, Path: path, Message: "events are filtered but no channel is listed"})
	}
	for i, e := range r.Events {
		if !slices.Contains(events, e) {
			ps = append(ps, Problem{File: file, Path: path, Message: fmt.Sprintf("event %d of the route is not an event type this emitter produces", i+1)})
		}
	}
	return ps
}
