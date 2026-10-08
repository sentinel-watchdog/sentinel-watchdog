package config

import (
	"errors"
	"fmt"
	"strings"

	"go.yaml.in/yaml/v3"
)

// LookupEnv resolves a variable name. os.LookupEnv satisfies it.
type LookupEnv func(name string) (string, bool)

// errExpansionBudget reports that expansion would grow a document by more
// than its budget.
var errExpansionBudget = errors.New("expansion budget exceeded")

// expandString replaces ${NAME} references in s. The result may be at
// most budget bytes longer than s: the limit is checked before each value
// is copied, so a short value with many references to a large variable
// fails before allocating its expansion.
//
// Rules: NAME matches [A-Za-z_][A-Za-z0-9_]*; "$${" produces a literal
// "${"; a "$" not followed by "{" is kept verbatim; undefined variables,
// invalid names and unterminated references are errors. Expansion is not
// recursive: values containing "${" are inserted as-is.
func expandString(s string, lookup LookupEnv, budget int) (string, error) {
	if !strings.Contains(s, "$") {
		return s, nil
	}
	var b strings.Builder
	var errs []error
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '$' {
			b.WriteByte(c)
			continue
		}
		rest := s[i+1:]
		switch {
		case strings.HasPrefix(rest, "${"):
			b.WriteString("${")
			i += 2
		case strings.HasPrefix(rest, "{"):
			end := strings.IndexByte(rest, '}')
			if end < 0 {
				// Not quoting s: it may hold a secret written in the file.
				return "", errors.New("unterminated variable reference (missing '}')")
			}
			name := rest[1:end]
			i += end + 1
			if !isEnvName(name) {
				errs = append(errs, fmt.Errorf("invalid variable name %q", name))
				continue
			}
			v, ok := lookup(name)
			if !ok {
				errs = append(errs, fmt.Errorf("environment variable %q is not set", name))
				continue
			}
			// Growth so far: bytes written minus bytes consumed (s[:i+1]).
			if b.Len()+len(v)-(i+1) > budget {
				return "", errExpansionBudget
			}
			b.WriteString(v)
		default:
			b.WriteByte(c)
		}
	}
	if len(errs) > 0 {
		return "", errors.Join(errs...)
	}
	return b.String(), nil
}

func isEnvName(s string) bool {
	if s == "" {
		return false
	}
	for i, c := range s {
		switch {
		case c == '_', c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// maxExpansionGrowth bounds how many bytes ${VARIABLE} expansion may add to
// one document: a short reference repeated many times to a large variable
// must not turn a small file into a huge one.
const maxExpansionGrowth = MaxFileSize

// expandNode expands variables in every scalar value under n. Mapping keys
// are never expanded. A plain (unquoted) scalar that changed has its tag
// cleared so YAML re-resolves it: `max_attempts: ${N}` becomes an int.
//
// It returns the values it substituted: they are often secrets, and every
// problem reported about this document is redacted with them.
func expandNode(n *yaml.Node, lookup LookupEnv) (values []string, err error) {
	record := func(name string) (string, bool) {
		v, ok := lookup(name)
		if ok {
			values = append(values, v)
		}
		return v, ok
	}
	e := &expander{lookup: record, budget: maxExpansionGrowth}
	e.walk(n, false)
	if e.exceeded {
		e.errs = append(e.errs, fmt.Errorf("environment variable values add more than %d bytes to the file", maxExpansionGrowth))
	}
	return values, errors.Join(e.errs...)
}

type expander struct {
	lookup   LookupEnv
	budget   int
	exceeded bool
	errs     []error
}

func (e *expander) walk(n *yaml.Node, isKey bool) {
	if e.exceeded {
		return
	}
	switch n.Kind {
	case yaml.ScalarNode:
		if isKey {
			return
		}
		v, err := expandString(n.Value, e.lookup, e.budget)
		if errors.Is(err, errExpansionBudget) {
			e.exceeded = true
			return
		}
		if err != nil {
			e.errs = append(e.errs, fmt.Errorf("line %d: %w", n.Line, err))
			return
		}
		if v == n.Value {
			return
		}
		if e.budget -= len(v) - len(n.Value); e.budget < 0 {
			e.exceeded = true
			return
		}
		n.Value = v
		if n.Style == 0 {
			n.Tag = ""
		}
	case yaml.MappingNode:
		for i, c := range n.Content {
			e.walk(c, i%2 == 0)
		}
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, c := range n.Content {
			e.walk(c, false)
		}
	case yaml.AliasNode:
		// The anchored node is expanded where it is defined.
	}
}
