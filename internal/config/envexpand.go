package config

import (
	"errors"
	"fmt"
	"strings"

	"go.yaml.in/yaml/v3"
)

// LookupEnv resolves a variable name. os.LookupEnv satisfies it.
type LookupEnv func(name string) (string, bool)

// expandString replaces ${NAME} references in s.
//
// Rules: NAME matches [A-Za-z_][A-Za-z0-9_]*; "$${" produces a literal
// "${"; a "$" not followed by "{" is kept verbatim; undefined variables,
// invalid names and unterminated references are errors. Expansion is not
// recursive: values containing "${" are inserted as-is.
func expandString(s string, lookup LookupEnv) (string, error) {
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
				return "", fmt.Errorf("unterminated variable reference in %q", s)
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

// expandNode expands variables in every scalar value under n. Mapping keys
// are never expanded. A plain (unquoted) scalar that changed has its tag
// cleared so YAML re-resolves it: `max_attempts: ${N}` becomes an int.
func expandNode(n *yaml.Node, lookup LookupEnv) error {
	var errs []error
	expandWalk(n, lookup, false, &errs)
	return errors.Join(errs...)
}

func expandWalk(n *yaml.Node, lookup LookupEnv, isKey bool, errs *[]error) {
	switch n.Kind {
	case yaml.ScalarNode:
		if isKey {
			return
		}
		v, err := expandString(n.Value, lookup)
		if err != nil {
			*errs = append(*errs, fmt.Errorf("line %d: %w", n.Line, err))
			return
		}
		if v != n.Value {
			n.Value = v
			if n.Style == 0 {
				n.Tag = ""
			}
		}
	case yaml.MappingNode:
		for i, c := range n.Content {
			expandWalk(c, lookup, i%2 == 0, errs)
		}
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, c := range n.Content {
			expandWalk(c, lookup, false, errs)
		}
	case yaml.AliasNode:
		// The anchored node is expanded where it is defined.
	}
}
