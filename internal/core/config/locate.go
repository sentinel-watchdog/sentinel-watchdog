package config

import (
	"fmt"

	"go.yaml.in/yaml/v3"
)

// loc is where a value sits in a file: the dotted path that Problem.Path
// shows and the YAML node the value was decoded from. Both are built one
// segment at a time (a key, a list item), never parsed back from the
// dotted form, because a channel or header name may itself contain a dot
// (B-001).
//
// A key left out of the file, so that its value is a default, has no node
// of its own: the location keeps the deepest node that exists on the way,
// and a problem with that value is reported at its parent's line.
type loc struct {
	path string
	node *yaml.Node // the value, aliases resolved, or its deepest existing parent; nil: no line known
	// at is the line where the value is written: an alias's own line, not
	// its anchor's.
	at int
	// exact reports that node is the value itself; once a segment is
	// missing, the location stays at that parent.
	exact bool
}

// rootLoc is the location of a file's root mapping.
func rootLoc(root *yaml.Node) loc {
	if root == nil {
		return loc{}
	}
	return loc{node: root, at: root.Line, exact: true}
}

// key is the location of key in the mapping at l.
func (l loc) key(k string) loc {
	next := l
	next.path = k
	if l.path != "" {
		next.path = l.path + "." + k
	}
	if l.exact && l.node.Kind == yaml.MappingNode {
		v, ok := lookupKey(l.node, k)
		next.exact = ok
		if ok {
			next.node, next.at = resolveAlias(v), v.Line
		}
	}
	return next
}

// list is the list at l as yaml.v3 decodes it: its items in order with
// null entries left out, or every later value would point at the line
// before. It is built once per list, so that locating every value stays
// linear in the list's length.
type list struct {
	parent loc
	items  []loc
}

// list returns the list at l; a location that is not a list has none.
func (l loc) list() list {
	out := list{parent: l}
	if !l.exact || l.node.Kind != yaml.SequenceNode {
		return out
	}
	for _, v := range l.node.Content {
		if n := resolveAlias(v); !isNull(n) {
			out.items = append(out.items, loc{node: n, at: v.Line, exact: true})
		}
	}
	return out
}

// item is the location of the i-th decoded value, shown as path
// (itemPath names an item by its name when it has a valid one), or the
// list's own location when it has no such item.
func (s list) item(i int, path string) loc {
	next := s.parent
	next.exact = false
	if i < len(s.items) {
		next = s.items[i]
	}
	next.path = path
	return next
}

// keyLine returns the line of key k itself in the mapping at l (where a
// name is written, as for an unknown header), or l's line when absent.
func (l loc) keyLine(k string) int {
	if l.exact && l.node.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(l.node.Content); i += 2 {
			if l.node.Content[i].Value == k {
				return l.node.Content[i].Line
			}
		}
	}
	return l.line()
}

// line returns the line of l, or 0 when no node is known.
func (l loc) line() int { return l.at }

// atLine prefixes message with "line N: " as every located problem does
// (Section.problem), unless line is 0.
func atLine(line int, message string) string {
	if line <= 0 {
		return message
	}
	return fmt.Sprintf("line %d: %s", line, message)
}
