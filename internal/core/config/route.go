package config

import (
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
func (r *Route) UnmarshalYAML(n *yaml.Node) error {
	n = resolveAlias(n)
	switch n.Kind {
	case yaml.SequenceNode:
		var channels []string
		if err := n.Decode(&channels); err != nil {
			return err // a yaml.TypeError: its own messages carry the line
		}
		*r = Route{Channels: channels}
		return nil
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			if key := n.Content[i]; key.Value != "channels" && key.Value != "events" {
				return fmt.Errorf("line %d: unknown field %q in a route (valid fields: channels, events)", key.Line, key.Value)
			}
		}
		type plain Route // without UnmarshalYAML, to avoid recursion
		var p plain
		if err := n.Decode(&p); err != nil {
			return err // a yaml.TypeError: its own messages carry the line
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
// and path.
func (r Route) Problems(file, path string, channels []string, events []model.EventType) []Problem {
	var ps []Problem
	seen := map[string]bool{}
	for _, c := range r.Channels {
		switch {
		case seen[c]:
			ps = append(ps, Problem{File: file, Path: path, Message: fmt.Sprintf("channel %q listed twice", c)})
		case !slices.Contains(channels, c):
			ps = append(ps, Problem{File: file, Path: path, Message: fmt.Sprintf("unknown notification channel %q", c)})
		}
		seen[c] = true
	}
	if len(r.Channels) == 0 && len(r.Events) > 0 {
		ps = append(ps, Problem{File: file, Path: path, Message: "events are filtered but no channel is listed"})
	}
	for _, e := range r.Events {
		if !slices.Contains(events, e) {
			ps = append(ps, Problem{File: file, Path: path, Message: fmt.Sprintf("unknown event type %q", e)})
		}
	}
	return ps
}
