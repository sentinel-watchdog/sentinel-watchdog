package config

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

var unmarshalerType = reflect.TypeFor[yaml.Unmarshaler]()

// checkKnownFields walks node alongside the Go type t and reports every
// mapping key that does not correspond to a field, with its line number.
//
// yaml.v3's KnownFields option is lost as soon as a custom UnmarshalYAML
// calls Node.Decode, so strictness is enforced here instead, uniformly for
// the whole tree. Types implementing yaml.Unmarshaler validate themselves.
func checkKnownFields(node *yaml.Node, t reflect.Type) error {
	var errs []error
	walkKnownFields(node, t, &errs)
	return errors.Join(errs...)
}

func walkKnownFields(node *yaml.Node, t reflect.Type, errs *[]error) {
	if node == nil {
		return
	}
	switch node.Kind {
	case yaml.DocumentNode:
		if len(node.Content) > 0 {
			walkKnownFields(node.Content[0], t, errs)
		}
		return
	case yaml.AliasNode:
		walkKnownFields(node.Alias, t, errs)
		return
	}

	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if reflect.PointerTo(t).Implements(unmarshalerType) {
		return
	}

	switch t.Kind() {
	case reflect.Struct:
		if node.Kind != yaml.MappingNode {
			return // type mismatch is reported by the decoder
		}
		fields := yamlFields(t)
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, val := node.Content[i], node.Content[i+1]
			if key.ShortTag() == "!!merge" {
				walkKnownFields(val, t, errs)
				continue
			}
			ft, ok := fields[key.Value]
			if !ok {
				*errs = append(*errs, fmt.Errorf("line %d: unknown field %q (valid fields: %s)",
					key.Line, key.Value, strings.Join(sortedKeys(fields), ", ")))
				continue
			}
			walkKnownFields(val, ft, errs)
		}
	case reflect.Slice, reflect.Array:
		if node.Kind != yaml.SequenceNode {
			return
		}
		for _, item := range node.Content {
			walkKnownFields(item, t.Elem(), errs)
		}
	case reflect.Map:
		if node.Kind != yaml.MappingNode {
			return
		}
		for i := 1; i < len(node.Content); i += 2 {
			walkKnownFields(node.Content[i], t.Elem(), errs)
		}
	}
}

// yamlFields maps YAML keys to field types for struct t, flattening
// `yaml:",inline"` fields and skipping `yaml:"-"`.
func yamlFields(t reflect.Type) map[string]reflect.Type {
	out := make(map[string]reflect.Type)
	for f := range t.Fields() {
		if !f.IsExported() {
			continue
		}
		tag := f.Tag.Get("yaml")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if slices.Contains(strings.Split(opts, ","), "inline") {
			ft := f.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			for k, v := range yamlFields(ft) {
				out[k] = v
			}
			continue
		}
		if name == "" {
			name = strings.ToLower(f.Name)
		}
		out[name] = f.Type
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
