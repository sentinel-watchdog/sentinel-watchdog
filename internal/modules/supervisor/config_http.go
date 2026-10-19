package supervisor

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/textproto"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/config"
)

// Limits of an http service's request and expectations (D-082).
const (
	maxHeaders          = 32
	maxHeaderValueBytes = 8 << 10
	maxBodyBytes        = 64 << 10
	maxBodyContains     = 4 << 10
	// maxBodyRead is how much of a response body_contains searches.
	maxBodyRead = 1 << 20
	// maxRedirects bounds the hops of follow_redirects (D-080 (4)).
	maxRedirects = 5
)

// HTTP is what an http service's check sends and expects, defaults
// applied.
type HTTP struct {
	Method  string
	Headers map[string]string // canonical names
	Body    string
	// SuccessCodes are the accepted status codes, sorted; nil accepts any
	// 2xx.
	SuccessCodes []int
	// BodyContains must occur in the first maxBodyRead bytes of the
	// response; "" means the body is not read.
	BodyContains    string
	FollowRedirects bool
	// Address is the "ip:port" to connect to instead of resolving the
	// URL's host; "" resolves it. Host, SNI and certificate verification
	// still follow the URL.
	Address       string
	TLS           config.TLSConfig
	TLSMinVersion uint16
}

type (
	httpYAML struct {
		Method             string            `yaml:"method"`
		Headers            map[string]string `yaml:"headers"`
		Body               string            `yaml:"body"`
		SuccessStatusCodes []int             `yaml:"success_status_codes"`
		BodyContains       string            `yaml:"body_contains"`
		FollowRedirects    bool              `yaml:"follow_redirects"`
		Address            string            `yaml:"address"`
		TLS                tlsYAML           `yaml:"tls"`
	}
	tlsYAML struct {
		CAFile             string `yaml:"ca_file"`
		ServerName         string `yaml:"server_name"`
		MinVersion         string `yaml:"min_version"`
		InsecureSkipVerify bool   `yaml:"insecure_skip_verify"`
	}
)

var (
	// methods are the request methods a check may use.
	methods = []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut}
	// headerNameRe is an RFC 9110 token, as for webhook headers.
	headerNameRe = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")
	// tlsVersions are the values of tls.min_version.
	tlsVersions = map[string]uint16{"1.2": tls.VersionTLS12, "1.3": tls.VersionTLS13}
)

// http checks the request and expectation keys of the service in item.
// Problems name keys and positions, never values: headers and bodies
// often hold tokens. Header names are quoted: keys are never expanded
// (D-006).
func (p *parsed) http(item config.Section, bad map[int]bool, path string, y httpYAML, u *url.URL) HTTP {
	h := HTTP{Method: http.MethodGet, Body: y.Body, FollowRedirects: y.FollowRedirects, TLSMinVersion: tls.VersionTLS12}
	if item.Has("method") && !bad[item.KeyLine("method")] {
		if m := strings.ToUpper(y.Method); slices.Contains(methods, m) {
			h.Method = m
		} else {
			p.addf(item, item.KeyLine("method"), path+".method", "method must be one of %s", strings.Join(methods, ", "))
		}
	}
	h.Headers = p.headers(item, path, y.Headers)
	if item.Has("body") {
		switch {
		case h.Method != http.MethodPost && h.Method != http.MethodPut:
			p.addf(item, item.KeyLine("body"), path+".body", "body needs method POST or PUT")
		case len(y.Body) > maxBodyBytes:
			p.addf(item, item.KeyLine("body"), path+".body", "body is longer than %d bytes", maxBodyBytes)
		}
	}
	h.SuccessCodes = p.successCodes(item, bad, path, y)
	if item.Has("body_contains") && !bad[item.KeyLine("body_contains")] {
		line := item.KeyLine("body_contains")
		switch {
		case y.BodyContains == "":
			p.addf(item, line, path+".body_contains", "body_contains is empty: remove it, or give the text the body must contain")
		case len(y.BodyContains) > maxBodyContains:
			p.addf(item, line, path+".body_contains", "body_contains is longer than %d bytes", maxBodyContains)
		case h.Method == http.MethodHead:
			p.addf(item, line, path+".body_contains", "a HEAD response has no body: use GET")
		default:
			h.BodyContains = y.BodyContains
		}
	}
	if item.Has("address") && !bad[item.KeyLine("address")] {
		addr, why := checkAddress(y.Address, u)
		if why != "" {
			p.addf(item, item.KeyLine("address"), path+".address", "%s", why)
		}
		h.Address = addr
	}
	h.TLS, h.TLSMinVersion = p.tls(item, bad, path, y.TLS, u)
	return h
}

// headers checks the request headers and returns them by canonical name.
func (p *parsed) headers(item config.Section, path string, in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	block, _ := item.Child("headers") // not a mapping: the decode reported it
	if len(in) > maxHeaders {
		p.addf(item, item.KeyLine("headers"), path+".headers", "more than %d headers", maxHeaders)
		return nil
	}
	out := make(map[string]string, len(in))
	names := make([]string, 0, len(in))
	for name := range in {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		line, at := lineOr(block, name), path+".headers"
		canon := textproto.CanonicalMIMEHeaderKey(name)
		switch {
		case !headerNameRe.MatchString(name):
			p.addf(block, line, at, "invalid header name %q", name)
			continue
		case canon == "Host":
			p.addf(block, line, at, "the Host header follows url; to connect to a specific IP, set address")
			continue
		case canon == "Content-Length" || canon == "Transfer-Encoding" || canon == "Connection":
			p.addf(block, line, at, "header %q is set by the HTTP client", name)
			continue
		}
		if _, dup := out[canon]; dup {
			p.addf(block, line, at, "header %q is given twice (header names are case-insensitive)", name)
			continue
		}
		switch v := in[name]; {
		case strings.ContainsFunc(v, isControl):
			p.addf(block, line, at+"."+name, "value contains a control character (only tab is allowed)")
		case len(v) > maxHeaderValueBytes:
			p.addf(block, line, at+"."+name, "value is longer than %d bytes", maxHeaderValueBytes)
		default:
			out[canon] = v
		}
	}
	return out
}

// isControl reports the characters net/http refuses in a header value:
// every ASCII control except horizontal tab (review R2).
func isControl(r rune) bool { return (r < 0x20 && r != '\t') || r == 0x7f }

// successCodes checks success_status_codes and returns them sorted and
// without repeats; nil when absent (any 2xx).
func (p *parsed) successCodes(item config.Section, bad map[int]bool, path string, y httpYAML) []int {
	line := item.KeyLine("success_status_codes")
	if !item.Has("success_status_codes") || bad[line] {
		return nil
	}
	at := path + ".success_status_codes"
	if len(y.SuccessStatusCodes) == 0 {
		p.addf(item, line, at, "list at least one status code, or remove the key to accept any 2xx")
		return nil
	}
	ok := true
	for i, c := range y.SuccessStatusCodes {
		switch {
		case c < 200 || c > 599:
			p.addf(item, line, at, "entry %d is not a final HTTP status code (200-599)", i+1)
			ok = false
		case c >= 300 && c < 400 && y.FollowRedirects:
			p.addf(item, line, at, "entry %d is a redirect: with follow_redirects the check never ends on one", i+1)
			ok = false
		}
	}
	if !ok {
		return nil
	}
	codes := slices.Clone(y.SuccessStatusCodes)
	slices.Sort(codes)
	return slices.Compact(codes)
}

// tls checks the tls block. The CA bundle itself is read when the checker
// is built, so `sentinelctl validate` reports it (D-080 (4)).
func (p *parsed) tls(item config.Section, bad map[int]bool, path string, y tlsYAML, u *url.URL) (config.TLSConfig, uint16) {
	block, _ := item.Child("tls") // not a mapping: the decode reported it
	out, version := config.TLSConfig{ServerName: y.ServerName, InsecureSkipVerify: y.InsecureSkipVerify}, uint16(tls.VersionTLS12)
	if block.IsZero() {
		return out, version
	}
	at := path + ".tls"
	if u != nil && u.Scheme != "https" {
		p.addf(item, item.KeyLine("tls"), at, "tls applies only to https urls")
		return config.TLSConfig{}, version
	}
	if block.Has("ca_file") && !bad[block.KeyLine("ca_file")] {
		if !filepath.IsAbs(y.CAFile) {
			p.addf(block, block.KeyLine("ca_file"), at+".ca_file", "ca_file must be an absolute path")
		} else {
			out.CAFile = filepath.Clean(y.CAFile)
		}
	}
	if block.Has("min_version") && !bad[block.KeyLine("min_version")] {
		if v, ok := tlsVersions[y.MinVersion]; ok {
			version = v
		} else {
			p.addf(block, block.KeyLine("min_version"), at+".min_version", `min_version must be "1.2" or "1.3"`)
		}
	}
	if y.InsecureSkipVerify {
		msg := "TLS certificate verification is disabled (D-019)"
		if line := block.KeyLine("insecure_skip_verify"); line > 0 {
			msg = fmt.Sprintf("line %d: %s", line, msg)
		}
		p.warnings = append(p.warnings, config.Problem{File: item.File, Path: at + ".insecure_skip_verify", Message: msg})
	}
	return out, version
}

// checkAddress parses address: an IP address with an optional port
// ("10.0.0.5", "10.0.0.5:8443", "2001:db8::1", "[2001:db8::1]:8443");
// without one, the URL's port. It returns "ip:port". The reason it gives
// never quotes the value.
func checkAddress(raw string, u *url.URL) (string, string) {
	const want = "address must be an IP address, optionally with a port (10.0.0.5, 10.0.0.5:8443, [2001:db8::1]:8443)"
	if raw == "" {
		return "", "address is empty: remove it, or give the IP address to connect to"
	}
	bare := raw
	if inner, ok := strings.CutPrefix(raw, "["); ok {
		bare, _ = strings.CutSuffix(inner, "]")
		if bare == inner { // "[" without "]": maybe "[v6]:port"
			bare = ""
		}
	}
	var ip netip.Addr
	port := ""
	if a, err := netip.ParseAddr(bare); err == nil && (bare == raw || a.Is6()) {
		ip = a
	} else if ap, err := netip.ParseAddrPort(raw); err == nil && ap.Port() != 0 {
		ip, port = ap.Addr(), fmt.Sprint(ap.Port())
	} else {
		return "", want
	}
	if ip.IsUnspecified() || ip.IsMulticast() {
		return "", "address must be a unicast IP address"
	}
	if port == "" {
		if u == nil {
			return "", "" // the url's own problem is reported
		}
		port = portOf(u)
	}
	return net.JoinHostPort(ip.String(), port), ""
}

// portOf returns u's port, or the default port of its scheme.
func portOf(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}
	if u.Scheme == "https" {
		return "443"
	}
	return "80"
}
