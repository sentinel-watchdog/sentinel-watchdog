package config

import (
	"fmt"
	"strings"
)

// Problem is one configuration finding, located by file and/or a dotted
// path such as "supervisors[worker].recovery.max_attempts".
type Problem struct {
	File    string `json:"file,omitempty"`
	Path    string `json:"path,omitempty"`
	Message string `json:"message"`
}

func (p Problem) String() string {
	var b strings.Builder
	if p.File != "" {
		b.WriteString(p.File)
		b.WriteString(": ")
	}
	if p.Path != "" {
		b.WriteString(p.Path)
		b.WriteString(": ")
	}
	b.WriteString(p.Message)
	return b.String()
}

// ValidationError reports every problem that made a configuration invalid.
type ValidationError struct {
	Problems []Problem
	// Warnings found before validation failed, for complete reporting.
	Warnings []Problem
}

func (e *ValidationError) Error() string {
	lines := make([]string, 0, len(e.Problems)+1)
	lines = append(lines, fmt.Sprintf("invalid configuration (%d problem(s)):", len(e.Problems)))
	for _, p := range e.Problems {
		lines = append(lines, "  - "+p.String())
	}
	return strings.Join(lines, "\n")
}
