package config

import (
	"fmt"
	"reflect"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/sentinel-watchdog/sentinel/pkg/model"
)

// monitorDoc is the flat YAML shape of a monitor: common fields plus the
// fields of one type-specific spec.
type monitorDoc[T any] struct {
	MonitorCommon `yaml:",inline"`
	Spec          T `yaml:",inline"`
}

// UnmarshalYAML dispatches on the "type" key and decodes the remaining keys
// strictly into the matching spec.
func (m *Monitor) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return typeErr(n, "monitor must be a mapping")
	}
	typ, err := scalarField(n, "type")
	if err != nil {
		return err
	}
	mt := model.MonitorType(typ)

	var out Monitor
	switch mt {
	case model.MonitorSystemd:
		out.MonitorCommon, out.Systemd, err = decodeMonitor[SystemdSpec](n)
	case model.MonitorProcess:
		out.MonitorCommon, out.Process, err = decodeMonitor[ProcessSpec](n)
	case model.MonitorHTTP:
		out.MonitorCommon, out.HTTP, err = decodeMonitor[HTTPSpec](n)
	case model.MonitorCron:
		out.MonitorCommon, out.Cron, err = decodeMonitor[CronSpec](n)
	case "":
		return typeErr(n, "monitor is missing required field \"type\" (one of %s)", joinTypes(model.ImplementedMonitorTypes()))
	default:
		if mt.IsPlanned() {
			return typeErr(n, "monitor type %q is planned but not implemented in this release (implemented: %s)",
				typ, joinTypes(model.ImplementedMonitorTypes()))
		}
		return typeErr(n, "unknown monitor type %q (implemented: %s)", typ, joinTypes(model.ImplementedMonitorTypes()))
	}
	if err != nil {
		return err
	}
	*m = out
	return nil
}

func decodeMonitor[T any](n *yaml.Node) (MonitorCommon, *T, error) {
	var doc monitorDoc[T]
	if err := checkKnownFields(n, reflect.TypeOf(doc)); err != nil {
		return MonitorCommon{}, nil, toTypeErr(err)
	}
	if err := n.Decode(&doc); err != nil {
		return MonitorCommon{}, nil, err
	}
	return doc.MonitorCommon, &doc.Spec, nil
}

// MarshalYAML renders the monitor back to its flat YAML shape.
func (m Monitor) MarshalYAML() (any, error) {
	switch {
	case m.Systemd != nil:
		return monitorDoc[SystemdSpec]{MonitorCommon: m.MonitorCommon, Spec: *m.Systemd}, nil
	case m.Process != nil:
		return monitorDoc[ProcessSpec]{MonitorCommon: m.MonitorCommon, Spec: *m.Process}, nil
	case m.HTTP != nil:
		return monitorDoc[HTTPSpec]{MonitorCommon: m.MonitorCommon, Spec: *m.HTTP}, nil
	case m.Cron != nil:
		return monitorDoc[CronSpec]{MonitorCommon: m.MonitorCommon, Spec: *m.Cron}, nil
	}
	return nil, fmt.Errorf("monitor %q has no spec", m.Name)
}

// UnmarshalYAML accepts either a list of channel names or a mapping with
// "channels" and "events".
func (r *NotificationRefs) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.SequenceNode:
		var channels []string
		if err := n.Decode(&channels); err != nil {
			return err
		}
		*r = NotificationRefs{Channels: channels}
		return nil
	case yaml.MappingNode:
		type plain NotificationRefs // drop methods to avoid recursion
		if err := checkKnownFields(n, reflect.TypeFor[plain]()); err != nil {
			return toTypeErr(err)
		}
		var p plain
		if err := n.Decode(&p); err != nil {
			return err
		}
		*r = NotificationRefs(p)
		return nil
	}
	return typeErr(n, "notifications must be a list of channel names or a mapping with \"channels\" and \"events\"")
}

// MarshalYAML emits the short form when no event filter is set.
func (r NotificationRefs) MarshalYAML() (any, error) {
	if len(r.Events) == 0 {
		return r.Channels, nil
	}
	type plain NotificationRefs
	return plain(r), nil
}

// scalarField returns the value of a scalar key in mapping n ("" if absent).
func scalarField(n *yaml.Node, key string) (string, error) {
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value != key {
			continue
		}
		v := n.Content[i+1]
		if v.Kind != yaml.ScalarNode {
			return "", typeErr(v, "field %q must be a string", key)
		}
		return v.Value, nil
	}
	return "", nil
}

// typeErr builds a *yaml.TypeError so the decoder keeps collecting errors
// from sibling nodes instead of aborting at the first one.
func typeErr(n *yaml.Node, format string, args ...any) error {
	return &yaml.TypeError{Errors: []string{fmt.Sprintf("line %d: ", n.Line) + fmt.Sprintf(format, args...)}}
}

func toTypeErr(err error) error {
	var msgs []string
	for _, e := range unwrapJoined(err) {
		msgs = append(msgs, e.Error())
	}
	return &yaml.TypeError{Errors: msgs}
}

func unwrapJoined(err error) []error {
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		return j.Unwrap()
	}
	return []error{err}
}

func joinTypes(types []model.MonitorType) string {
	s := make([]string, len(types))
	for i, t := range types {
		s[i] = string(t)
	}
	return strings.Join(s, ", ")
}
