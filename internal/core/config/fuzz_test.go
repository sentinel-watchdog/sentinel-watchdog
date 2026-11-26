package config

import (
	"bytes"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
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

// FuzzLoad loads any central file from disk, through the defaults and the
// semantic checks: no panic, no problem or warning quotes an expanded
// secret, and an accepted file has usable channels and paths.
func FuzzLoad(f *testing.F) {
	for _, seed := range []string{
		"version: 1\n",
		"version: 1\ndaemon: {socket: /run/s.sock, socket_mode: \"0660\", socket_group: \"50\", timezone: Europe/Rome}\n",
		"version: 1\ndaemon: {state_dir: /, shutdown_timeout: 0s, access: {operator_group: \"-1\", admin_group: \"4294967296\"}}\n",
		"version: 1\nnotifications:\n  channels:\n    - {name: ops, type: webhook, url: \"https://h/${TOKEN}\", headers: {X-A: \"${TOKEN}\"}}\n  core: [ops]\n",
		"version: 1\nnotifications:\n  channels:\n    - {name: \"${TOKEN}\", type: slack, url: \"${TOKEN}://\"}\n    - {name: a, type: webhook, url: \"http://h:99999/\", success_status_codes: [301, 1]}\n",
		"version: 1\nmodules: {alpha: {enabled: true}, \"${TOKEN}\": {enabled: 1}}\n",
	} {
		f.Add([]byte(seed))
	}
	// The same text, not from the environment: not a secret.
	f.Add([]byte("version: 1\nmodules: {\"tok-\\x39f8e7d\": {}}\n"))
	// The secret's text as a name in the file, beside the secret.
	f.Add([]byte("version: 1\ndaemon: {socket: \"${TOKEN}${tok-9f8e7d}\"}\n"))
	// A warning beside an error.
	f.Add([]byte("version: 1\ndaemon: {socket: relative, access: {admin_group: \"${TOKEN}\"}}\n"))
	// B-027: the root directory, control bytes, headers that differ only
	// in case, ports at the edges; B-001: names with dots.
	f.Add([]byte("version: 1\ndaemon: {socket: /, state_dir: \"${TOKEN}\"}\n"))
	f.Add([]byte("version: 1\nnotifications:\n  channels:\n    - name: a.b\n      type: webhook\n      url: \"https://h:65535/\"\n" +
		"      headers: {X.A: \"a\\tb\", x.a: \"\\x7f\", Host: \"${TOKEN}\\x1f\"}\n      tls: {ca_file: /}\n"))
	f.Add([]byte("version: 1\nnotifications:\n  channels:\n    - {name: a, type: webhook, url: \"https://h:0/${TOKEN}\"}\n    - {name: a, type: webhook, url: \"https://h:1/\"}\n"))
	const token = "tok-9f8e7d"
	f.Fuzz(func(t *testing.T, data []byte) {
		file := filepath.Join(t.TempDir(), "sentinel.yaml")
		if err := os.WriteFile(file, data, 0o600); err != nil {
			t.Fatal(err)
		}
		expanded := false
		cfg, err := Load(LoadOptions{MainFile: file,
			Modules: map[string]ModuleAvailability{"alpha": ModuleAvailable},
			Lookup: func(name string) (string, bool) {
				expanded = expanded || name == "TOKEN"
				return token, name == "TOKEN"
			}})
		problems := ProblemsOf(err, "", "")
		var ve *ValidationError
		switch {
		case errors.As(err, &ve):
			problems = append(problems, ve.Warnings...)
		case err == nil:
			problems = append(problems, cfg.Warnings...)
		}
		for _, p := range problems {
			if expanded && !bytes.Contains(data, []byte(token)) && strings.Contains(p.String(), token) {
				t.Fatalf("a problem or warning quotes the secret: %s", p)
			}
		}
		if err != nil {
			return
		}
		if !filepath.IsAbs(cfg.Daemon.Socket) || !filepath.IsAbs(cfg.Daemon.StateDir) || cfg.Daemon.Socket == "/" || cfg.Daemon.StateDir == "/" {
			t.Fatalf("accepted socket %q, state directory %q", cfg.Daemon.Socket, cfg.Daemon.StateDir)
		}
		names := map[string]bool{}
		for _, ch := range cfg.Notifications.Channels {
			if ch.Name == "" || names[ch.Name] || ch.Type != ChannelWebhook || ch.URL == "" || ch.TLS.CAFile == "/" {
				t.Fatalf("accepted channel %q, type %q", ch.Name, ch.Type)
			}
			names[ch.Name] = true
			if u, err := url.Parse(ch.URL); err != nil || !usablePort(u.Port()) {
				t.Fatalf("accepted channel %q with an unusable URL", ch.Name)
			}
			canonical := map[string]bool{}
			for k, v := range ch.Headers {
				if controlByte(v) || canonical[http.CanonicalHeaderKey(k)] {
					t.Fatalf("accepted channel %q with header %q", ch.Name, k)
				}
				canonical[http.CanonicalHeaderKey(k)] = true
			}
		}
	})
}

// usablePort reports whether port is empty or a number from 1 to 65535.
func usablePort(port string) bool {
	if port == "" {
		return true
	}
	n, err := strconv.Atoi(port)
	return err == nil && n >= 1 && n <= 65535
}

// controlByte reports whether s holds a byte below 0x20 other than TAB,
// or DEL: written apart from the validator, so the fuzz oracle does not
// share its mistakes.
func controlByte(s string) bool {
	for i := 0; i < len(s); i++ {
		if b := s[i]; b == 0x7f || b < 0x20 && b != '\t' {
			return true
		}
	}
	return false
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
