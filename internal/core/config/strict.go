package config

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// maxWalkNodes bounds the work of checkKnownFields. YAML aliases let a small
// file reference the same subtree many times (the "billion laughs" pattern);
// counting every visited node, aliases included, keeps the walk linear in a
// budget that no legitimate configuration file approaches.
const maxWalkNodes = 1 << 20

var (
	unmarshalerType = reflect.TypeFor[yaml.Unmarshaler]()
	nodeType        = reflect.TypeFor[yaml.Node]()
)

// checkKnownFields walks node alongside the Go type t and reports every
// mapping key that does not correspond to a field, with its line number.
//
// yaml.v3's KnownFields option is lost as soon as a custom UnmarshalYAML
// calls Node.Decode, so strictness is enforced here instead, uniformly for
// the whole tree. Types implementing yaml.Unmarshaler validate themselves;
// yaml.Node fields are left to whoever decodes them later.
func checkKnownFields(node *yaml.Node, t reflect.Type) error {
	w := &fieldWalker{budget: maxWalkNodes}
	w.walk(node, t)
	if w.exhausted {
		return errors.New("configuration is too complex to check (too many nodes or YAML alias expansions)")
	}
	return errors.Join(w.errs...)
}

type fieldWalker struct {
	errs      []error
	budget    int
	exhausted bool
}

func (w *fieldWalker) walk(node *yaml.Node, t reflect.Type) {
	if node == nil || w.exhausted {
		return
	}
	if w.budget--; w.budget < 0 {
		w.exhausted = true
		return
	}
	switch node.Kind {
	case yaml.DocumentNode:
		if len(node.Content) > 0 {
			w.walk(node.Content[0], t)
		}
		return
	case yaml.AliasNode:
		w.walk(node.Alias, t)
		return
	}

	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nodeType || reflect.PointerTo(t).Implements(unmarshalerType) {
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
				w.walk(val, t)
				continue
			}
			ft, ok := fields[key.Value]
			if !ok {
				w.errs = append(w.errs, fmt.Errorf("line %d: unknown field %q (valid fields: %s)",
					key.Line, key.Value, strings.Join(sortedKeys(fields), ", ")))
				continue
			}
			w.walk(val, ft)
		}
	case reflect.Slice, reflect.Array:
		if node.Kind != yaml.SequenceNode {
			return
		}
		for _, item := range node.Content {
			w.walk(item, t.Elem())
		}
	case reflect.Map:
		if node.Kind != yaml.MappingNode {
			return
		}
		for i := 1; i < len(node.Content); i += 2 {
			w.walk(node.Content[i], t.Elem())
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
