package daemon

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/authz"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/transport"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/api"
)

// commandHarness runs a daemon with alpha (running, answers its commands
// with what it received) and broken (failed at start); the binary also
// knows idle, whose module is not enabled.
func commandHarness(t *testing.T, handle func(context.Context, string, jsontext.Value) (any, *api.Error)) *harness {
	t.Helper()
	h := newHarness(t, &testModule{name: "alpha", handle: handle}, &testModule{name: "broken", startErr: errors.New("no")})
	h.d.opts.Commands = map[string][]api.Command{
		"alpha":  {{Name: "alpha.ping", Tier: api.TierRead}, {Name: "alpha.kick", Tier: api.TierOperate}},
		"broken": {{Name: "broken.ping", Tier: api.TierRead}},
		"idle":   {{Name: "idle.ping", Tier: api.TierRead}},
	}
	if err := h.d.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.d.Stop(context.Background()) })
	return h
}

// closing reports whether m takes no more commands.
func (d *Daemon) closing(m *moduleRun) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return m.closing
}

func TestModuleCommandDispatch(t *testing.T) {
	type call struct {
		Command string         `json:"command"`
		Args    map[string]any `json:"args"`
	}
	h := commandHarness(t, func(_ context.Context, command string, args jsontext.Value) (any, *api.Error) {
		c := call{Command: command}
		if len(args) > 0 {
			if err := json.Unmarshal(args, &c.Args); err != nil {
				return nil, &api.Error{Code: api.CodeBadRequest, Message: err.Error()}
			}
		}
		return c, nil
	})
	tests := []struct {
		name, command, args string
		peer                *authz.Peer
		want                string   // result, when no error
		code                api.Code // error, when set
	}{
		{name: "read command", command: "alpha.ping", args: `{"n":1}`, want: `{"command":"alpha.ping","args":{"n":1}}`},
		{name: "operate command by root", command: "alpha.kick", args: `{"force":true}`, want: `{"command":"alpha.kick","args":{"force":true}}`},
		{name: "operate command by a reader", command: "alpha.kick", peer: &authz.Peer{UID: 1000, GID: 1000},
			code: api.CodePermissionDenied},
		{name: "module failed at start", command: "broken.ping", code: api.CodeUnavailable},
		{name: "module not enabled", command: "idle.ping", code: api.CodeUnavailable},
		{name: "undeclared command", command: "alpha.other", code: api.CodeUnknownCommand},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h.peer.Store(tt.peer)
			resp := h.call(t, tt.command, tt.args)
			if tt.code != "" {
				if resp.Error == nil || resp.Error.Code != tt.code {
					t.Fatalf("response %+v, want error %s", resp, tt.code)
				}
				return
			}
			if resp.Error != nil {
				t.Fatal(resp.Error)
			}
			if string(resp.Result) != tt.want {
				t.Errorf("result %s, want %s", resp.Result, tt.want)
			}
		})
	}
	h.peer.Store(nil)
	if resp := h.call(t, "broken.ping", ""); !strings.Contains(resp.Error.Message, "module broken is not running") {
		t.Errorf("message %q", resp.Error.Message)
	}
}

// A command's panic is recovered at the module boundary: the client gets
// a fixed error, the panic value reaches neither the client nor the
// daemon_error event, and the daemon keeps serving.
func TestModuleCommandPanicIsRecovered(t *testing.T) {
	h := commandHarness(t, func(context.Context, string, jsontext.Value) (any, *api.Error) { panic(secret) })
	resp := h.call(t, "alpha.ping", "")
	if resp.Error == nil || resp.Error.Code != api.CodeInternal || strings.Contains(resp.Error.Message, secret) {
		t.Fatalf("response %+v", resp)
	}
	if resp := h.call(t, api.CoreStatus.Name, ""); resp.Error != nil {
		t.Fatalf("daemon stopped serving: %v", resp.Error)
	}
	_ = h.d.Stop(context.Background()) // fails for broken: delivers the events
	errs := h.daemonErrors()
	if len(errs) != 2 { // broken's start, alpha's command
		t.Fatalf("daemon_error events: %+v", errs)
	}
	e := errs[1]
	if e.Source != "alpha" || e.Attributes["operation"] != "command alpha.ping" || e.Attributes["failure"] != "panic" {
		t.Errorf("event %+v", e)
	}
	if strings.Contains(h.logs.String(), secret) || strings.Contains(e.Message, secret) {
		t.Error("the panic value leaked")
	}
}

// A reload that restarts a module first closes its commands, then waits
// for the ones in flight: Stop never runs beside Handle, and a request that
// arrives meanwhile is refused rather than sent to an instance stopping.
func TestReloadWaitsForCommandsInFlight(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	instances := 0
	h := newReloadHarnessWith(t, func(m *testModule) {
		if m.name != "alpha" {
			return
		}
		instances++
		if instances > 1 {
			return
		}
		m.handle = func(_ context.Context, command string, _ jsontext.Value) (any, *api.Error) {
			close(entered)
			<-release // ignores ctx: only the daemon's wait keeps Stop away
			m.rec.add("handled " + command)
			return "late", nil
		}
	}, "", baseModules, map[string]string{"alpha/10.yaml": alphaV1})

	first := make(chan api.Response, 1)
	go func() {
		// Not h.call: t.Fatal must not run outside the test's goroutine.
		resp, err := transport.Call(context.Background(), h.cfg.Daemon.Socket,
			api.Request{Version: api.Version, Command: "alpha.ping"})
		if err != nil {
			resp = api.Fail(api.CodeInternal, err.Error())
		}
		first <- resp
	}()
	<-entered
	h.write(t, map[string]string{"alpha/10.yaml": alphaV2})
	reloaded := make(chan api.ReloadResult, 1)
	go func() { res, _ := h.reload(t); reloaded <- res }()

	run := h.d.runs()[0]
	deadline := time.Now().Add(5 * time.Second)
	for !h.d.closing(run) {
		if time.Now().After(deadline) {
			t.Fatal("the reload never closed alpha's commands")
		}
		time.Sleep(time.Millisecond)
	}
	if resp := h.call(t, "alpha.ping", ""); resp.Error == nil || resp.Error.Code != api.CodeUnavailable {
		t.Errorf("request while stopping: %+v", resp)
	}
	if got := h.rec.list(); strings.Contains(strings.Join(got, ","), "stop alpha") {
		t.Fatalf("alpha stopped with a command in flight: %v", got)
	}
	close(release)
	if resp := <-first; resp.Error != nil || string(resp.Result) != `"late"` {
		t.Errorf("command in flight: %+v", resp)
	}
	if res := <-reloaded; len(res.Restarted) != 1 {
		t.Errorf("reload %+v", res)
	}
	got := strings.Join(h.rec.list(), ",")
	if !strings.Contains(got, "handled alpha.ping,stop alpha") {
		t.Errorf("order %s", got)
	}
	if resp := h.call(t, "alpha.ping", ""); resp.Error != nil || string(resp.Result) != `"alpha.ping"` {
		t.Errorf("new instance: %+v", resp)
	}
}

// panicResult is a command result whose encoding panics.
type panicResult struct{}

func (panicResult) MarshalJSON() ([]byte, error) { panic(secret) }

// Encoding the result runs the module's code too (review R1): it stays
// inside the module boundary, so a panic there does not end sentineld.
func TestModuleCommandResultPanicIsRecovered(t *testing.T) {
	h := commandHarness(t, func(context.Context, string, jsontext.Value) (any, *api.Error) { return panicResult{}, nil })
	resp := h.call(t, "alpha.ping", "")
	if resp.Error == nil || resp.Error.Code != api.CodeInternal || strings.Contains(resp.Error.Message, secret) {
		t.Fatalf("response %+v", resp)
	}
	if resp := h.call(t, api.CoreStatus.Name, ""); resp.Error != nil {
		t.Fatalf("daemon stopped serving: %v", resp.Error)
	}
	_ = h.d.Stop(context.Background()) // fails for broken: delivers the events
	if errs := h.daemonErrors(); len(errs) != 2 || errs[1].Attributes["failure"] != "panic" {
		t.Errorf("daemon_error events: %+v", errs)
	}
	if strings.Contains(h.logs.String(), secret) {
		t.Error("the panic value leaked")
	}
}
