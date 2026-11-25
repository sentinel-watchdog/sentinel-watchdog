//go:build ignore

// Command lint checks the repository's text files (task lint-docs): no em
// or en dash, and every relative link and #anchor of the Markdown files
// resolves to a file of the repository. It reads the file names, separated
// by NUL, from stdin, and runs in their root. One file run with go run,
// outside ./...: the package layers (ADR-0015) hold the agent only.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// As numbers, so that this file passes its own check.
const enDash, emDash = rune(0x2013), rune(0x2014)

var (
	// A destination ends at a space (a title follows) or the parenthesis.
	inlineLink = regexp.MustCompile(`\]\(<?([^)\s>]+)`)
	refLink    = regexp.MustCompile(`^ {0,3}\[[^\]]+\]:\s+<?([^\s>]+)`)
	htmlLink   = regexp.MustCompile(`(?:href|src)="([^"]+)"`)
	htmlAnchor = regexp.MustCompile(`(?:id|name)="([^"]+)"`)
	heading    = regexp.MustCompile(`^ {0,3}#{1,6}\s+(.+?)(?:\s+#+)?\s*$`)
	headLink   = regexp.MustCompile(`!?\[([^\]]*)\]\([^)]*\)`)
	backticks  = regexp.MustCompile("`+")
	fence      = regexp.MustCompile("^ {0,3}(```+|~~~+)(.*)$")
	scheme     = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:|^//`)
)

type link struct {
	line   int
	target string
}

type page struct {
	links   []link
	anchors map[string]bool
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "lint-docs:", err)
		os.Exit(1)
	}
}

func run() error {
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}
	names := strings.FieldsFunc(string(input), func(r rune) bool { return r == 0 })
	slices.Sort(names)
	t := tree{exists: map[string]bool{".": true}, pages: map[string]page{}}
	for _, name := range names {
		if err := t.read(name); err != nil {
			return err
		}
	}
	if len(t.exists) == 1 {
		return errors.New("no files on stdin")
	}
	for _, name := range names {
		for _, l := range t.pages[name].links {
			if problem := t.check(name, l.target); problem != "" {
				t.report(name, l.line, "link "+l.target+": "+problem)
			}
		}
	}
	if t.findings > 0 {
		return fmt.Errorf("%d findings", t.findings)
	}
	return nil
}

// tree is what the files of the repository are checked against.
type tree struct {
	exists   map[string]bool // files and their directories
	pages    map[string]page // Markdown files
	findings int
}

func (t *tree) report(name string, line int, problem string) {
	fmt.Printf("%s:%d: %s\n", name, line, problem)
	t.findings++
}

// read adds a file to t and reports its dashes.
func (t *tree) read(name string) error {
	data, err := os.ReadFile(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil // tracked, and deleted in the working tree
	}
	if err != nil {
		return err // names the file
	}
	for p := name; !t.exists[p]; p = path.Dir(p) {
		t.exists[p] = true
	}
	// Binary files, and inputs a fuzzer wrote, are not prose.
	if bytes.IndexByte(data, 0) >= 0 || strings.Contains("/"+name, "/testdata/fuzz/") {
		return nil
	}
	for i, line := range strings.Split(string(data), "\n") {
		if strings.ContainsRune(line, enDash) || strings.ContainsRune(line, emDash) {
			t.report(name, i+1, "em or en dash")
		}
	}
	if strings.HasSuffix(name, ".md") {
		t.pages[name] = parse(string(data))
	}
	return nil
}

// check says what is wrong with a link of the page name, or "".
func (t *tree) check(name, target string) string {
	if scheme.MatchString(target) {
		return ""
	}
	file, anchor, _ := strings.Cut(target, "#")
	file, err := url.PathUnescape(file)
	if err != nil {
		return err.Error()
	}
	dest := name
	switch {
	case strings.HasPrefix(file, "/"): // from the repository's root
		dest = path.Join(".", file[1:])
	case file != "":
		dest = path.Join(path.Dir(name), file)
	}
	if !t.exists[dest] {
		return "no such file in the repository"
	}
	if to, ok := t.pages[dest]; ok && anchor != "" && !to.anchors[anchor] {
		return "no such heading"
	}
	return ""
}

// parse collects a Markdown page's links and the anchors GitHub gives it,
// outside code.
func parse(text string) page {
	p := page{anchors: map[string]bool{}}
	seen := map[string]int{}
	closing := "" // the fence that ends the open code block
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if m := fence.FindStringSubmatch(line); m != nil &&
			(closing == "" || strings.HasPrefix(m[1], closing) && strings.TrimSpace(m[2]) == "") {
			if closing == "" {
				closing = m[1]
			} else {
				closing = ""
			}
			lines[i] = ""
			continue
		}
		if closing != "" {
			lines[i] = ""
			continue
		}
		if m := heading.FindStringSubmatch(line); m != nil { // code in a heading is part of its anchor
			s := slug(m[1])
			if n := seen[s]; n > 0 {
				p.anchors[fmt.Sprintf("%s-%d", s, n)] = true
			} else {
				p.anchors[s] = true
			}
			seen[s]++
		}
	}
	prose := withoutCode(strings.Join(lines, "\n"))
	for i, line := range strings.Split(prose, "\n") {
		for _, m := range htmlAnchor.FindAllStringSubmatch(line, -1) {
			p.anchors[m[1]] = true
		}
		for _, re := range []*regexp.Regexp{inlineLink, refLink, htmlLink} {
			for _, m := range re.FindAllStringSubmatch(line, -1) {
				p.links = append(p.links, link{i + 1, m[1]})
			}
		}
	}
	return p
}

// withoutCode removes the code spans, keeping their line breaks so that
// line numbers stay. A span ends with as many backticks as it starts with,
// in the same paragraph; other backticks are text.
func withoutCode(text string) string {
	paragraphs := strings.Split(text, "\n\n")
	for i, p := range paragraphs {
		paragraphs[i] = withoutSpans(p)
	}
	return strings.Join(paragraphs, "\n\n")
}

func withoutSpans(p string) string {
	runs := backticks.FindAllStringIndex(p, -1)
	// next[i] is the run after i as long as i, or 0: found from the end, once.
	next, last := make([]int, len(runs)), map[int]int{}
	for i, r := range slices.Backward(runs) {
		next[i] = last[r[1]-r[0]]
		last[r[1]-r[0]] = i
	}
	var b strings.Builder
	from := 0
	for i := 0; i < len(runs); i++ {
		start := runs[i][0]
		escaped := strings.HasSuffix(strings.ReplaceAll(p[from:start], `\\`, ""), `\`)
		if next[i] == 0 || escaped {
			continue
		}
		end := runs[next[i]][1]
		b.WriteString(p[from:start])
		b.WriteString(strings.Repeat("\n", strings.Count(p[start:end], "\n")))
		from, i = end, next[i]
	}
	return b.String() + p[from:]
}

// slug is the anchor GitHub gives a heading: lower case, letters, digits,
// hyphens and underscores kept, spaces as hyphens.
func slug(text string) string {
	text = headLink.ReplaceAllString(text, "$1")
	var b strings.Builder
	for _, r := range strings.ToLower(text) {
		switch {
		case r == ' ':
			b.WriteByte('-')
		case r == '-', r == '_', unicode.IsLetter(r), unicode.IsDigit(r):
			b.WriteRune(r)
		}
	}
	return b.String()
}
