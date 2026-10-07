package config

import (
	"errors"
	"fmt"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Problem is one configuration finding, located by file and/or a dotted
// path such as "notifications.channels[ops].url". YAML-level findings
// carry "line N:" at the start of Message.
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

// ProblemsOf turns err into problems: the problems of a *ValidationError,
// or a single problem at file and path for any other error.
func ProblemsOf(err error, file, path string) []Problem {
	if err == nil {
		return nil
	}
	var ve *ValidationError
	if errors.As(err, &ve) {
		return ve.Problems
	}
	return []Problem{{File: file, Path: path, Message: err.Error()}}
}

// problemsFromYAML splits yaml.TypeError and joined errors into one
// Problem per message.
func problemsFromYAML(file string, err error) []Problem {
	var msgs []string
	var te *yaml.TypeError
	if errors.As(err, &te) {
		msgs = te.Errors
	} else {
		for _, e := range unwrapJoined(err) {
			msgs = append(msgs, e.Error())
		}
	}
	out := make([]Problem, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, Problem{File: file, Message: strings.TrimPrefix(m, "yaml: ")})
	}
	return out
}

func unwrapJoined(err error) []error {
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		return j.Unwrap()
	}
	return []error{err}
}
