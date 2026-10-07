package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"slices"

	"go.yaml.in/yaml/v3"
)

// MaxFileSize caps each configuration file to keep parsing bounded.
const MaxFileSize = 4 << 20

// Section is a mapping from a configuration file, after ${VARIABLE}
// expansion: the content of a module file, or a module's block in the
// central file. A module decodes its sections with Decode, which rejects
// unknown keys and reports problems with file and line.
//
// The zero Section is empty: Decode leaves its target unchanged.
type Section struct {
	// File is the path of the file the section comes from.
	File string
	node *yaml.Node // a mapping node, or nil
}

// IsZero reports whether the section has no keys.
func (s Section) IsZero() bool { return s.node == nil || len(s.node.Content) == 0 }

// Line returns the line where the section starts, or 0 for an empty section.
func (s Section) Line() int {
	if s.node == nil {
		return 0
	}
	return s.node.Line
}

// Keys returns the top-level keys of the section in file order.
func (s Section) Keys() []string {
	if s.node == nil {
		return nil
	}
	keys := make([]string, 0, len(s.node.Content)/2)
	for i := 0; i+1 < len(s.node.Content); i += 2 {
		keys = append(keys, s.node.Content[i].Value)
	}
	return keys
}

// Has reports whether the section has the top-level key.
func (s Section) Has(key string) bool { return slices.Contains(s.Keys(), key) }

// Decode decodes the section into v, which must be a non-nil pointer.
// Unknown keys and type mismatches are errors. The error is a
// *ValidationError listing every problem, each located by File and line.
func (s Section) Decode(v any) error {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return fmt.Errorf("config: Section.Decode needs a non-nil pointer, got %T", v)
	}
	if s.IsZero() {
		return nil
	}
	if err := checkKnownFields(s.node, rv.Type()); err != nil {
		return &ValidationError{Problems: problemsFromYAML(s.File, err)}
	}
	if err := s.node.Decode(v); err != nil {
		return &ValidationError{Problems: problemsFromYAML(s.File, err)}
	}
	return nil
}

// without returns a copy of mapping node m without the given top-level
// keys. The original node is not modified.
func without(m *yaml.Node, keys ...string) *yaml.Node {
	if m == nil {
		return nil
	}
	out := *m
	out.Content = nil
	for i := 0; i+1 < len(m.Content); i += 2 {
		if slices.Contains(keys, m.Content[i].Value) {
			continue
		}
		out.Content = append(out.Content, m.Content[i], m.Content[i+1])
	}
	return &out
}

// lookupKey returns the value node of a top-level key in mapping m.
func lookupKey(m *yaml.Node, key string) (*yaml.Node, bool) {
	if m == nil {
		return nil, false
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1], true
		}
	}
	return nil, false
}

// checkVersion verifies the mandatory `version: 1` of a file's root mapping.
func checkVersion(root *yaml.Node) error {
	v, ok := lookupKey(root, "version")
	if !ok {
		return fmt.Errorf("version is required (version: %d)", SchemaVersion)
	}
	var n int
	if v.Kind != yaml.ScalarNode || v.ShortTag() != "!!int" || v.Decode(&n) != nil {
		return fmt.Errorf("line %d: version must be the integer %d", v.Line, SchemaVersion)
	}
	if n != SchemaVersion {
		return fmt.Errorf("line %d: unsupported configuration version %d (this release supports %d)", v.Line, n, SchemaVersion)
	}
	return nil
}

// parseDocument turns the bytes of one file into its root mapping, with
// ${VARIABLE} references expanded. The file must hold exactly one YAML
// document whose top level is a mapping.
func parseDocument(data []byte, lookup LookupEnv) (*yaml.Node, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("file is empty")
		}
		return nil, err
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("multiple YAML documents are not allowed")
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("the top level must be a mapping (key: value)")
	}
	root := doc.Content[0]
	if err := expandNode(root, lookup); err != nil {
		return nil, err
	}
	return root, nil
}

// readFile reads a regular file of at most MaxFileSize bytes. check runs
// on the opened file's metadata before its content is read, so the file
// that is checked is the file that is read.
func readFile(path string, check func(os.FileInfo) error) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	if check != nil {
		if err := check(info); err != nil {
			return nil, err
		}
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxFileSize {
		return nil, fmt.Errorf("file exceeds %d bytes", MaxFileSize)
	}
	return data, nil
}
