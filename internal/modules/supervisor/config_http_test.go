package supervisor

import (
	"crypto/tls"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/config"
)

// httpService configures one service from the YAML of its keys after name
// and returns its HTTP spec.
func httpService(t *testing.T, keys string) (HTTP, error) {
	t.Helper()
	in, err := configure(t, map[string]string{"supervisor/a.yaml": "version: 1\nservices:\n  - name: web\n" + keys},
		newFakes().build)
	if err != nil {
		return HTTP{}, err
	}
	return in.services[0].def.HTTP, nil
}

// D-082: the request and expectation keys, defaults applied.
func TestHTTPKeys(t *testing.T) {
	tests := []struct {
		name, keys string
		want       HTTP
	}{
		{"defaults", "    url: https://web/\n",
			HTTP{Method: "GET", TLSMinVersion: tls.VersionTLS12}},
		{"post with headers and body", "    url: https://web/\n    method: post\n" +
			"    headers: {authorization: Bearer x, X-Probe: \"1\\t2\"}\n    body: '{}'\n    success_status_codes: [204, 200, 204]\n",
			HTTP{Method: "POST", Headers: map[string]string{"Authorization": "Bearer x", "X-Probe": "1\t2"}, Body: "{}",
				SuccessCodes: []int{200, 204}, TLSMinVersion: tls.VersionTLS12}},
		{"expected redirect", "    url: http://web/\n    method: HEAD\n    success_status_codes: [301]\n",
			HTTP{Method: "HEAD", SuccessCodes: []int{301}, TLSMinVersion: tls.VersionTLS12}},
		{"body and redirects", "    url: https://web/\n    body_contains: ok\n    follow_redirects: true\n",
			HTTP{Method: "GET", BodyContains: "ok", FollowRedirects: true, TLSMinVersion: tls.VersionTLS12}},
		{"address takes the url port", "    url: https://app.example.com:8443/\n    address: 10.0.0.5\n",
			HTTP{Method: "GET", Address: "10.0.0.5:8443", TLSMinVersion: tls.VersionTLS12}},
		{"address default port", "    url: http://app.example.com/\n    address: 10.0.0.5\n",
			HTTP{Method: "GET", Address: "10.0.0.5:80", TLSMinVersion: tls.VersionTLS12}},
		{"address with port", "    url: https://app/\n    address: 10.0.0.5:9443\n",
			HTTP{Method: "GET", Address: "10.0.0.5:9443", TLSMinVersion: tls.VersionTLS12}},
		{"IPv6 address", "    url: https://app/\n    address: \"[2001:db8::1]\"\n",
			HTTP{Method: "GET", Address: "[2001:db8::1]:443", TLSMinVersion: tls.VersionTLS12}},
		{"bare IPv6 address", "    url: https://app/\n    address: \"2001:db8::1\"\n",
			HTTP{Method: "GET", Address: "[2001:db8::1]:443", TLSMinVersion: tls.VersionTLS12}},
		{"IPv6 address with port", "    url: https://app/\n    address: \"[2001:db8::1]:8443\"\n",
			HTTP{Method: "GET", Address: "[2001:db8::1]:8443", TLSMinVersion: tls.VersionTLS12}},
		{"tls", "    url: https://web/\n    tls: {server_name: web.internal, min_version: 1.3}\n",
			HTTP{Method: "GET", TLS: config.TLSConfig{ServerName: "web.internal"}, TLSMinVersion: tls.VersionTLS13}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := httpService(t, tt.keys)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got  %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

// D-082, Codex critique C3: every rule of the HTTP keys, located, without
// quoting a value.
func TestHTTPKeyProblems(t *testing.T) {
	tests := []struct {
		name, keys string
		want       []string
	}{
		{"method", "    url: https://web/\n    method: TRACE\n", []string{"services[0].method", "line 5"}},
		{"Host header", "    url: https://web/\n    headers: {host: other}\n", []string{"services[0].headers", "line 5", "set address"}},
		{"client header", "    url: https://web/\n    headers:\n      transfer-encoding: chunked\n", []string{"line 6", `"transfer-encoding" is set by the HTTP client`}},
		{"header twice", "    url: https://web/\n    headers: {X-A: \"1\", x-a: \"2\"}\n", []string{"line 5", "given twice"}},
		{"header name", "    url: https://web/\n    headers: {\"a b\": x}\n", []string{"line 5", "invalid header name"}},
		{"header value LF", "    url: https://web/\n    headers: {X-A: \"a\\nb\"}\n", []string{"services[0].headers.X-A", "control character"}},
		{"header value control byte", "    url: https://web/\n    headers: {X-A: \"a\\x01b\"}\n", []string{"services[0].headers.X-A", "control character"}},
		{"header value DEL", "    url: https://web/\n    headers: {X-A: \"a\\x7fb\"}\n", []string{"services[0].headers.X-A", "control character"}},
		{"header value too long", "    url: https://web/\n    headers: {X-A: " + strings.Repeat("a", maxHeaderValueBytes+1) + "}\n", []string{"longer than 8192"}},
		{"body with GET", "    url: https://web/\n    body: x\n", []string{"services[0].body", "line 5", "POST or PUT"}},
		{"body too long", "    url: https://web/\n    method: POST\n    body: " + strings.Repeat("a", maxBodyBytes+1) + "\n", []string{"services[0].body", "longer than"}},
		{"empty codes", "    url: https://web/\n    success_status_codes: []\n", []string{"line 5", "at least one"}},
		{"1xx code", "    url: https://web/\n    success_status_codes: [200, 101]\n", []string{"entry 2", "200-599"}},
		{"redirect followed", "    url: https://web/\n    follow_redirects: true\n    success_status_codes: [302]\n", []string{"entry 1", "never ends on one"}},
		{"empty body_contains", "    url: https://web/\n    body_contains: \"\"\n", []string{"services[0].body_contains", "is empty"}},
		{"body_contains on HEAD", "    url: https://web/\n    method: HEAD\n    body_contains: ok\n", []string{"line 6", "HEAD response has no body"}},
		{"body_contains too long", "    url: https://web/\n    body_contains: " + strings.Repeat("a", maxBodyContains+1) + "\n", []string{"body_contains is longer"}},
		{"empty address", "    url: https://web/\n    address: \"\"\n", []string{"services[0].address", "is empty"}},
		{"hostname address", "    url: https://web/\n    address: db.internal\n", []string{"line 5", "must be an IP address"}},
		{"address port zero", "    url: https://web/\n    address: 10.0.0.5:0\n", []string{"must be an IP address"}},
		{"bracketed IPv4", "    url: https://web/\n    address: \"[10.0.0.5]\"\n", []string{"must be an IP address"}},
		{"unbalanced bracket", "    url: https://web/\n    address: \"[2001:db8::1\"\n", []string{"must be an IP address"}},
		{"unspecified address", "    url: https://web/\n    address: 0.0.0.0\n", []string{"unicast"}},
		{"tls on http", "    url: http://web/\n    tls: {server_name: x}\n", []string{"services[0].tls", "only to https"}},
		{"relative ca_file", "    url: https://web/\n    tls:\n      ca_file: ca.pem\n", []string{"services[0].tls.ca_file", "line 6", "absolute"}},
		{"min_version", "    url: https://web/\n    tls: {min_version: \"1.1\"}\n", []string{"services[0].tls.min_version", `"1.2" or "1.3"`}},
		{"unknown tls key", "    url: https://web/\n    tls: {verify: false}\n", []string{"line 5", `unknown field "verify"`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := httpService(t, tt.keys)
			requireProblems(t, err, append([]string{"a.yaml"}, tt.want...))
		})
	}
}

// No problem of the HTTP keys quotes a value expanded into it, even one
// too short for redaction to mask.
func TestHTTPProblemsDoNotQuoteValues(t *testing.T) {
	mc, err := moduleConfig(t, map[string]string{"supervisor/a.yaml": "version: 1\nservices:\n" +
		"  - name: web\n    url: https://web/\n    method: \"${T}\"\n    headers: {X-A: \"${T}\\n\"}\n" +
		"    body: \"${T}\"\n    body_contains: \"${T}\"\n    address: \"${T}\"\n" +
		"    tls: {ca_file: \"${T}\", min_version: \"${T}\"}\n"}, map[string]string{"T": "Zq"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = New(Options{NewChecker: newFakes().build}).Configure(mc)
	requireProblems(t, err, []string{"method"}, []string{"headers.X-A"}, []string{"body"}, []string{"address"},
		[]string{"ca_file"}, []string{"min_version"})
	if strings.Contains(err.Error(), "Zq") {
		t.Errorf("a problem quotes a value:\n%v", err)
	}
}

// D-019: disabled verification is legal and warned about.
func TestInsecureSkipVerifyWarns(t *testing.T) {
	in, err := configure(t, map[string]string{"supervisor/a.yaml": "version: 1\nservices:\n" +
		"  - name: web\n    url: https://web/\n    tls:\n      insecure_skip_verify: true\n"}, newFakes().build)
	if err != nil {
		t.Fatal(err)
	}
	w := in.Warnings()
	if len(w) != 1 || !strings.Contains(w[0].String(), "line 6") || !strings.Contains(w[0].String(), "tls.insecure_skip_verify") {
		t.Errorf("warnings %v", w)
	}
}

// Codex critique C2: a CA bundle that cannot be used is reported at
// Configure (validate) with a fixed reason, never with the paths the
// underlying error names (the bundle, its directory, a link's target).
func TestCAFileProblemsDoNotQuotePaths(t *testing.T) {
	base := filepath.Join(t.TempDir(), "secretdir")
	if err := os.Mkdir(base, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(name string, data []byte) string {
		path := filepath.Join(base, name)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	notPEM := write("notpem.pem", []byte("x"))
	big := write("big.pem", make([]byte, 1<<20+1))
	outside := filepath.Join(t.TempDir(), "targetdir")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "ca.pem"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link.pem")
	if err := os.Symlink(filepath.Join("..", "..", filepath.Base(filepath.Dir(outside)), "targetdir", "ca.pem"), link); err != nil {
		t.Fatal(err)
	}
	unsafeDir := filepath.Join(base, "open")
	if err := os.Mkdir(unsafeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	unsafeCA := filepath.Join(unsafeDir, "ca.pem")
	if err := os.WriteFile(unsafeCA, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unsafeDir, 0o777); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unsafeDir, 0o700) })
	tests := []struct{ name, path, want string }{
		{"missing", filepath.Join(base, "missing.pem"), "does not exist"},
		{"missing directory", filepath.Join(base, "nodir", "ca.pem"), "does not exist"},
		{"not PEM", notPEM, "PEM certificates"},
		{"too large", big, "PEM certificates"},
		{"link out of its directory", link, "unsafe path"},
		{"writable directory", unsafeCA, "unsafe path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := configure(t, map[string]string{"supervisor/a.yaml": "version: 1\nservices:\n" +
				"  - name: web\n    url: https://web/\n    tls: {ca_file: \"" + tt.path + "\"}\n"}, nil)
			requireProblems(t, err, []string{"a.yaml", "line 3", "tls.ca_file", tt.want})
			for _, leak := range []string{"secretdir", "targetdir", ".pem"} {
				if strings.Contains(err.Error(), leak) {
					t.Errorf("the problem quotes %q:\n%v", leak, err)
				}
			}
		})
	}
}

// Without Options.NewChecker the module checks over HTTP: nothing is
// left looking implemented without a real check.
func TestDefaultCheckerIsHTTP(t *testing.T) {
	in, err := configure(t, map[string]string{"supervisor/a.yaml": "version: 1\nservices:\n  - {name: web, url: \"https://web/\", timeout: 1s}\n"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := in.services[0].checker.(*httpChecker); !ok || in.services[0].def.Timeout != time.Second {
		t.Errorf("checker %T", in.services[0].checker)
	}
}

// The shipped example stays valid (its CA bundle is not read: the
// checkers are fakes, as on a host without that file it is a reference).
func TestExampleFile(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "configs", "supervisor", "http.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	mc, err := moduleConfig(t, map[string]string{"supervisor/http.yaml": string(data)}, map[string]string{"BILLING_TOKEN": "t"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(Options{NewChecker: newFakes().build}).Configure(mc)
	if err != nil {
		t.Fatalf("configs/supervisor/http.yaml: %v", err)
	}
	if in := c.(*instance); len(in.services) != 5 || len(in.Warnings()) != 0 {
		t.Errorf("%d services, warnings %v", len(in.services), in.Warnings())
	}
}
