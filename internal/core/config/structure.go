package config

import (
	"errors"
	"fmt"

	"go.yaml.in/yaml/v3"
)

// checkStructure rejects YAML constructs that would let a document say two
// things at once, before any key is looked up or removed:
//
//   - duplicate keys in a mapping (`enabled: true` then `enabled: false`):
//     yaml.v3 does not report them when parsing into yaml.Node, and code
//     that reads the first occurrence would disagree with code that reads
//     the last;
//   - merge keys (`<<: *defaults`): they add keys that are not literally
//     present, so rules about which keys a file contains (such as one
//     `settings` block per module directory) could be bypassed.
//
// Anchors and aliases stay allowed. Alias nodes are not followed: the
// anchored node is checked where it is defined, which keeps the walk
// linear in the size of the file.
func checkStructure(n *yaml.Node) error {
	var errs []error
	checkStructureWalk(n, &errs)
	return errors.Join(errs...)
}

func checkStructureWalk(n *yaml.Node, errs *[]error) {
	switch n.Kind {
	case yaml.MappingNode:
		first := map[string]int{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			key := n.Content[i]
			if key.ShortTag() == "!!merge" {
				*errs = append(*errs, fmt.Errorf("line %d: YAML merge keys (<<) are not supported; write the keys explicitly", key.Line))
			} else if line, dup := first[key.Value]; dup {
				*errs = append(*errs, fmt.Errorf("line %d: duplicate key %q (first defined on line %d)", key.Line, key.Value, line))
			} else {
				first[key.Value] = key.Line
			}
			checkStructureWalk(n.Content[i+1], errs)
		}
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, c := range n.Content {
			checkStructureWalk(c, errs)
		}
	}
}
