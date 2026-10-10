package api

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestReadRequest(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		command string
		err     string // substring; "" means valid
	}{
		{"valid", `{"version":1,"command":"core.status"}` + "\n", "core.status", ""},
		{"with args", `{"version":1,"command":"supervisor.restart","args":{"name":"nginx"}}` + "\n", "supervisor.restart", ""},
		{"only the first line is read", `{"version":1,"command":"core.status"}` + "\nnot read\n", "core.status", ""},
		{"no newline", `{"version":1,"command":"core.status"}`, "", "newline"},
		{"empty", "", "", "newline"},
		{"other version", `{"version":2,"command":"core.status"}` + "\n", "", "unsupported protocol version"},
		{"no version", `{"command":"core.status"}` + "\n", "", "unsupported protocol version"},
		{"null", "null\n", "", "unsupported protocol version"},
		{"unknown member", `{"version":1,"command":"core.status","tier":"admin"}` + "\n", "", "unknown object member"},
		{"duplicate member", `{"version":1,"command":"core.status","command":"core.modules"}` + "\n", "", "duplicate"},
		{"trailing data", `{"version":1,"command":"core.status"} {}` + "\n", "", "after top-level value"},
		{"invalid UTF-8", "{\"version\":1,\"command\":\"core.\xff\"}\n", "", "UTF-8"},
		{"bad command", `{"version":1,"command":"status"}` + "\n", "", "invalid command name"},
		{"uppercase command", `{"version":1,"command":"Core.status"}` + "\n", "", "invalid command name"},
		{"args not an object", `{"version":1,"command":"core.status","args":[1]}` + "\n", "", "args must be a JSON object"},
		{"too large", `{"version":1,"command":"core.status","args":{"x":"` + strings.Repeat("a", MaxRequestBytes) + `"}}` + "\n", "", "exceeds"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := ReadRequest(strings.NewReader(tt.in))
			if tt.err == "" {
				if err != nil || req.Command != tt.command {
					t.Fatalf("ReadRequest = %+v, %v", req, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.err) {
				t.Errorf("err = %v, want %q", err, tt.err)
			}
		})
	}
}

func TestVersionErrorIsDistinguishable(t *testing.T) {
	_, err := ReadRequest(strings.NewReader(`{"version":9,"command":"core.status"}` + "\n"))
	if !errors.Is(err, ErrUnsupportedVersion) {
		t.Errorf("err = %v", err)
	}
}

func TestResponseRoundTrip(t *testing.T) {
	ok, err := Result(Status{Version: "1.0.0", Modules: []ModuleInfo{{Name: "supervisor", Availability: "planned"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, resp := range []Response{ok, Fail(CodePermissionDenied, "needs operate")} {
		var b bytes.Buffer
		if err := WriteResponse(&b, resp); err != nil {
			t.Fatal(err)
		}
		got, err := ReadResponse(&b)
		if err != nil {
			t.Fatal(err)
		}
		if (got.Error == nil) != (resp.Error == nil) || string(got.Result) != string(resp.Result) {
			t.Errorf("round trip %+v -> %+v", resp, got)
		}
	}
}

func TestReadResponseRejectsAmbiguousResponses(t *testing.T) {
	for _, in := range []string{
		`{"version":1}`,
		`{"version":1,"result":{},"error":{"code":"internal","message":"x"}}`,
		`{"version":2,"result":{}}`,
	} {
		if _, err := ReadResponse(strings.NewReader(in + "\n")); err == nil {
			t.Errorf("accepted %s", in)
		}
	}
}

func TestWriteRefusesOversizeMessages(t *testing.T) {
	big, err := Result(strings.Repeat("x", MaxResponseBytes))
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteResponse(&bytes.Buffer{}, big); err == nil {
		t.Error("oversize response written")
	}
}

func TestTierAllows(t *testing.T) {
	tests := []struct {
		have, need Tier
		want       bool
	}{
		{TierAdmin, TierRead, true}, {TierAdmin, TierAdmin, true},
		{TierOperate, TierRead, true}, {TierOperate, TierAdmin, false},
		{TierRead, TierRead, true}, {TierRead, TierOperate, false},
		{"", TierRead, false}, {TierAdmin, "", false}, {"root", TierRead, false}, {TierAdmin, "superuser", false},
	}
	for _, tt := range tests {
		if got := tt.have.Allows(tt.need); got != tt.want {
			t.Errorf("%q.Allows(%q) = %v", tt.have, tt.need, got)
		}
	}
}

// FuzzReadRequest: whatever a local user writes to the socket, the decoder
// returns a request that passes every rule, or an error, never a panic.
func FuzzReadRequest(f *testing.F) {
	f.Add(`{"version":1,"command":"core.status"}` + "\n")
	f.Add(`{"version":1,"command":"supervisor.restart","args":{"name":"nginx"}}` + "\n")
	f.Add(`{"version":1,"command":"core.status","command":"x.y"}` + "\n")
	f.Add("{\"version\":1,\"args\":{\"a\":[[[[[[1]]]]]]}}\n")
	f.Fuzz(func(t *testing.T, in string) {
		req, err := ReadRequest(strings.NewReader(in))
		if err != nil {
			return
		}
		if req.Version != Version || !ValidCommandName(req.Command) || (len(req.Args) > 0 && req.Args.Kind() != '{') {
			t.Fatalf("invalid request accepted: %+v", req)
		}
	})
}
