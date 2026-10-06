package config

import (
	"fmt"
	"reflect"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/model"
)

// supervisorDoc is the flat YAML shape of a supervisor: common fields plus the
// fields of one type-specific spec.
type supervisorDoc[T any] struct {
	SupervisorCommon `yaml:",inline"`
	Spec             T `yaml:",inline"`
}

// UnmarshalYAML dispatches on the "type" key and decodes the remaining keys
// strictly into the matching spec.
func (m *Supervisor) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return typeErr(n, "supervisor must be a mapping")
	}
	typ, err := scalarField(n, "type")
	if err != nil {
		return err
	}
	mt := model.SupervisorType(typ)

	var out Supervisor
	switch mt {
	case model.SupervisorSystemd:
		out.SupervisorCommon, out.Systemd, err = decodeSupervisor[SystemdSpec](n)
	case model.SupervisorProcess:
		out.SupervisorCommon, out.Process, err = decodeSupervisor[ProcessSpec](n)
	case model.SupervisorHTTP:
		out.SupervisorCommon, out.HTTP, err = decodeSupervisor[HTTPSpec](n)
	case model.SupervisorCron:
		out.SupervisorCommon, out.Cron, err = decodeSupervisor[CronSpec](n)
	case "":
		return typeErr(n, "supervisor is missing required field \"type\" (one of %s)", joinTypes(model.ImplementedSupervisorTypes()))
	default:
		if mt.IsPlanned() {
			return typeErr(n, "supervisor type %q is planned but not implemented in this release (implemented: %s)",
				typ, joinTypes(model.ImplementedSupervisorTypes()))
		}
		return typeErr(n, "unknown supervisor type %q (implemented: %s)", typ, joinTypes(model.ImplementedSupervisorTypes()))
	}
	if err != nil {
		return err
	}
	*m = out
	return nil
}

func decodeSupervisor[T any](n *yaml.Node) (SupervisorCommon, *T, error) {
	var doc supervisorDoc[T]
	if err := checkKnownFields(n, reflect.TypeOf(doc)); err != nil {
		return SupervisorCommon{}, nil, toTypeErr(err)
	}
	if err := n.Decode(&doc); err != nil {
		return SupervisorCommon{}, nil, err
	}
	return doc.SupervisorCommon, &doc.Spec, nil
}

// MarshalYAML renders the supervisor back to its flat YAML shape.
func (m Supervisor) MarshalYAML() (any, error) {
	switch {
	case m.Systemd != nil:
		return supervisorDoc[SystemdSpec]{SupervisorCommon: m.SupervisorCommon, Spec: *m.Systemd}, nil
	case m.Process != nil:
		return supervisorDoc[ProcessSpec]{SupervisorCommon: m.SupervisorCommon, Spec: *m.Process}, nil
	case m.HTTP != nil:
		return supervisorDoc[HTTPSpec]{SupervisorCommon: m.SupervisorCommon, Spec: *m.HTTP}, nil
	case m.Cron != nil:
		return supervisorDoc[CronSpec]{SupervisorCommon: m.SupervisorCommon, Spec: *m.Cron}, nil
	}
	return nil, fmt.Errorf("supervisor %q has no spec", m.Name)
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

func joinTypes(types []model.SupervisorType) string {
	s := make([]string, len(types))
	for i, t := range types {
		s[i] = string(t)
	}
	return strings.Join(s, ", ")
}
