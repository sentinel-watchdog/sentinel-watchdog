package config

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/redact"
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

// yamlQuotedValue matches the value yaml.v3 quotes in type errors
// ("cannot unmarshal !!str `abc...` into int"). Even shortened, it may be
// the start of a secret, so it is removed: the line number locates it.
var yamlQuotedValue = regexp.MustCompile("`[^`]*` ")

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
		m = yamlQuotedValue.ReplaceAllString(strings.TrimPrefix(m, "yaml: "), "")
		out = append(out, Problem{File: file, Message: m})
	}
	return out
}

// redactProblems replaces every secret in the problems' messages, both
// as written and as quoted by %q (where a newline becomes \n).
func redactProblems(ps []Problem, secrets []string) []Problem {
	if len(secrets) == 0 {
		return ps
	}
	forms := make([]string, 0, 2*len(secrets))
	for _, s := range secrets {
		q := strconv.Quote(s)
		forms = append(forms, s, q[1:len(q)-1])
	}
	for i := range ps {
		ps[i].Message = redact.Text(ps[i].Message, forms...)
	}
	return ps
}

func unwrapJoined(err error) []error {
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		return j.Unwrap()
	}
	return []error{err}
}
