package config

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
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

// problems collects the errors and warnings of one load or validation, so
// that every problem is reported, not only the first.
type problems struct {
	errs, warns []Problem
}

func (p *problems) errorf(file, path, format string, args ...any) {
	p.errs = append(p.errs, Problem{File: file, Path: path, Message: fmt.Sprintf(format, args...)})
}

func (p *problems) warnf(file, path, format string, args ...any) {
	p.warns = append(p.warns, Problem{File: file, Path: path, Message: fmt.Sprintf(format, args...)})
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

// yamlTypeError matches yaml.v3 type errors ("cannot unmarshal !!str
// `abc...` into int"). The quoted value, even shortened, may be the start
// of a secret, and yaml.v3 does not escape backticks inside it: the whole
// text between the tag and " into <type>" is removed. The line number
// locates the value.
var yamlTypeError = regexp.MustCompile(`(?s)(cannot unmarshal \S+) .*( into \S+)$`)

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
		m = yamlTypeError.ReplaceAllString(strings.TrimPrefix(m, "yaml: "), "$1$2")
		out = append(out, Problem{File: file, Message: m})
	}
	return out
}

// redactProblems replaces every secret in the problems' messages and paths
// (a channel name can come from the environment), both as written and as
// quoted by %q (where a newline becomes \n). Longer forms go first, so a
// secret that is a prefix of another leaves no tail behind.
func redactProblems(ps []Problem, secrets []string) []Problem {
	if len(secrets) == 0 {
		return ps
	}
	seen := map[string]bool{}
	var forms []string
	for _, s := range secrets {
		q := strconv.Quote(s)
		for _, f := range []string{s, q[1 : len(q)-1]} {
			if !seen[f] {
				seen[f] = true
				forms = append(forms, f)
			}
		}
	}
	slices.SortFunc(forms, func(a, b string) int { return len(b) - len(a) })
	for i := range ps {
		ps[i].Message = redact.Text(ps[i].Message, forms...)
		ps[i].Path = redact.Text(ps[i].Path, forms...)
	}
	return ps
}

func unwrapJoined(err error) []error {
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		return j.Unwrap()
	}
	return []error{err}
}
