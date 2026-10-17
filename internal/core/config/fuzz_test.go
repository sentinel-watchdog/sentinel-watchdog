package config

import (
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// Fuzz targets for every parser of untrusted configuration input
// (AGENTS.md, "Tests"). Run them with `task fuzz`.

// FuzzParseDocument feeds arbitrary bytes through parsing, structure
// checks, expansion and strict decoding of the central file shape. It
// must never panic, and a document that parses decodes or fails cleanly.
func FuzzParseDocument(f *testing.F) {
	for _, seed := range []string{
		"version: 1\n",
		"version: 1\ndaemon: {log: {level: debug}}\n",
		"version: 1\nmodules:\n  alpha: &b {enabled: true}\n  beta: *b\n",
		"version: 1\nx: &a [*a]\n",
		"a: &a [*a, *a]\nb: &b [*a, *a]\nc: [*b, *b]\n",
		"version: 1\nversion: 2\n",
		"<<: {a: 1}\n",
		"version: 1\nnotifications:\n  channels:\n    - {name: w, url: '${URL}'}\n",
		"--- a\n--- b\n",
	} {
		f.Add([]byte(seed))
	}
	lookup := env(map[string]string{"URL": "https://example.org/hook"})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > maxFileSize {
			t.Skip()
		}
		root, secrets, err := parseDocument(data, lookup)
		if err != nil {
			return
		}
		s := Section{File: "f.yaml", node: without(root, "version"), secrets: secrets}
		var doc centralDoc
		_ = s.Decode(&doc)
		// How a module walks its file (Phase 3a-3): every key as a block
		// and as a list; whatever is returned is a mapping of the same file.
		for _, key := range s.Keys() {
			if c, err := s.Child(key); err == nil && !c.IsZero() && (c.File != s.File || c.node.Kind != yaml.MappingNode) {
				t.Fatalf("Child(%q) is not a mapping of the file", key)
			}
			items, _ := s.Items(key)
			for _, item := range items {
				if item.File != s.File || item.node.Kind != yaml.MappingNode {
					t.Fatalf("an item of %q is not a mapping of the file", key)
				}
				var v map[string]any
				_ = item.Decode(&v)
			}
		}
	})
}

// FuzzExpandString checks the expansion rules and its budget: no panic,
// a value without '$' is unchanged, and a result never grows by more than
// the budget.
func FuzzExpandString(f *testing.F) {
	for _, seed := range []string{"plain", "Bearer ${TOKEN}", "$${X}", "${", "${}", "${1A}", "cost $5", "${TOKEN}${TOKEN}"} {
		f.Add(seed, "s3cret", 64)
	}
	f.Fuzz(func(t *testing.T, s, value string, budget int) {
		if budget < 0 || budget > maxFileSize {
			t.Skip()
		}
		got, err := expandString(s, env(map[string]string{"TOKEN": value}), budget)
		if err != nil {
			return
		}
		if !strings.Contains(s, "$") && got != s {
			t.Fatalf("expandString(%q) = %q without any '$'", s, got)
		}
		if len(got) > len(s)+budget {
			t.Fatalf("expandString grew %d bytes with a budget of %d", len(got)-len(s), budget)
		}
	})
}

// FuzzParseByteSize: no panic, and a plain decimal number parses to itself.
func FuzzParseByteSize(f *testing.F) {
	for _, seed := range []string{"0", "1024", "512MiB", "1GB", " 2 KiB ", "18446744073709551615", "16EiB", "-1", "1.5GB"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got, err := ParseByteSize(s)
		if err != nil {
			return
		}
		if n, perr := strconv.ParseUint(strings.TrimSpace(s), 10, 64); perr == nil && uint64(got) != n {
			t.Fatalf("ParseByteSize(%q) = %d, want %d", s, got, n)
		}
	})
}
