package daemon

import (
	"context"
	"encoding/json/v2"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/audit"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/authz"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/config"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/module"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/notify"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/transport"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/modules/supervisor"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/api"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/model"
)

// The supervisor through the real daemon (Phase 3a-3): the module is
// registered by a test registry, since production keeps it planned until
// the HTTP checker (3a-4).

// switchable is a checker whose outcome the test sets.
type switchable struct{ down atomic.Bool }

func (s *switchable) Check(context.Context) supervisor.Result {
	if s.down.Load() {
		return supervisor.Result{Failure: supervisor.FailureCheck}
	}
	return supervisor.Result{}
}

type supervisorHarness struct {
	d       *Daemon
	dir     string
	main    string
	sock    string
	peer    atomic.Pointer[authz.Peer]
	checker *switchable
	mu      sync.Mutex
	senders map[string]*sender // by channel name
}

func newSupervisorHarness(t *testing.T, services string) *supervisorHarness {
	t.Helper()
	h := &supervisorHarness{dir: t.TempDir(), sock: socketPath(t), checker: &switchable{}, senders: map[string]*sender{}}
	h.main = filepath.Join(h.dir, "sentinel.yaml")
	h.write(t, "sentinel.yaml", "version: 1\ndaemon:\n  socket: "+h.sock+"\n  state_dir: "+filepath.Join(h.dir, "state")+
		"\n  shutdown_timeout: 2s\nnotifications:\n  channels:\n"+
		"    - {name: ops, type: webhook, url: \"https://hooks.example.com/1\"}\n"+
		"    - {name: pager, type: webhook, url: \"https://hooks.example.com/2\"}\n"+
		"modules:\n  supervisor: {enabled: true}\n")
	h.write(t, "supervisor/web.yaml", services)
	reg := module.NewRegistry()
	if err := reg.Register(func() module.Module {
		return supervisor.New(supervisor.Options{NewChecker: func(supervisor.Service) (supervisor.Checker, error) { return h.checker, nil }})
	}); err != nil {
		t.Fatal(err)
	}
	load := func() (*config.Config, []module.Instance, error) { return Load(reg, h.main) }
	cfg, mods, err := load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	h.d = New(cfg, mods, Options{
		Logger: slog.New(slog.DiscardHandler), Hostname: "web1", Version: "test",
		Commands: reg.Commands(), Modules: reg.Availability(), Load: load,
		NewSender: func(ch config.Channel) (notify.Sender, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			s := &sender{}
			h.senders[ch.Name] = s // a reload replaces the channels' senders
			return s, nil
		},
		PeerCredentials: func(*net.UnixConn) (authz.Peer, error) {
			if p := h.peer.Load(); p != nil {
				return *p, nil
			}
			return authz.Peer{UID: 0}, nil
		},
	})
	if err := h.d.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.d.Stop(context.Background()) })
	return h
}

func (h *supervisorHarness) write(t *testing.T, name, body string) {
	t.Helper()
	path := filepath.Join(h.dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// delivered returns the events of type typ the channel received.
func (h *supervisorHarness) delivered(channel string, typ model.EventType) []model.Event {
	h.mu.Lock()
	s := h.senders[channel]
	h.mu.Unlock()
	var out []model.Event
	if s == nil {
		return nil
	}
	for _, p := range s.delivered() {
		if p.Event.Type == typ {
			out = append(out, p.Event)
		}
	}
	return out
}

// await polls cond for up to five seconds: the loops run on real time.
func await(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func (h *supervisorHarness) status(t *testing.T, args string) (api.SupervisorStatusResult, *api.Error) {
	t.Helper()
	req := api.Request{Version: api.Version, Command: api.SupervisorStatus.Name}
	if args != "" {
		req.Args = []byte(args)
	}
	resp := callSocket(t, h.sock, req)
	var st api.SupervisorStatusResult
	if resp.Error == nil {
		if err := json.Unmarshal(resp.Result, &st); err != nil {
			t.Fatal(err)
		}
	}
	return st, resp.Error
}

const supervisedWeb = "version: 1\nservices:\n  - name: web\n    url: \"https://web.example.org/health?token=SECRET\"\n" +
	"    check_interval: 1s\n    failure_policy: {consecutive_failures: 1}\n    notifications: [ops]\n"

// AC6, AC7, AC9: a failure reaches the service's own channel only, and
// the commands answer a read-tier peer over the socket.
func TestSupervisorThroughTheDaemon(t *testing.T) {
	h := newSupervisorHarness(t, supervisedWeb)
	h.peer.Store(&authz.Peer{UID: 1000, GID: 1000}) // read tier
	await(t, "the first check", func() bool {
		st, _ := h.status(t, "")
		return len(st.Services) == 1 && st.Services[0].State == "healthy"
	})
	h.checker.down.Store(true)
	await(t, "service_failed on ops", func() bool { return len(h.delivered("ops", supervisor.EventServiceFailed)) == 1 })
	if evs := h.delivered("pager", supervisor.EventServiceFailed); len(evs) != 0 {
		t.Errorf("pager received %d events", len(evs))
	}
	ev := h.delivered("ops", supervisor.EventServiceFailed)[0]
	if ev.Module != "supervisor" || ev.Source != "web" || ev.Attributes["endpoint"] != "https://web.example.org" {
		t.Errorf("event %+v", ev)
	}
	st, apiErr := h.status(t, `{"name":"web"}`)
	if apiErr != nil || st.Services[0].State != "failed" || st.Services[0].IncidentID != ev.ID {
		t.Fatalf("status %+v %v", st, apiErr)
	}
	resp := callSocket(t, h.sock, api.Request{Version: api.Version, Command: api.SupervisorList.Name})
	var list api.SupervisorListResult
	if resp.Error != nil || json.Unmarshal(resp.Result, &list) != nil || len(list.Services) != 1 {
		t.Errorf("list %+v", resp)
	}
	if _, apiErr := h.status(t, `{"name":"nope"}`); apiErr == nil || apiErr.Code != api.CodeBadRequest {
		t.Errorf("unknown service: %+v", apiErr)
	}
}

// AC8: a reload that changes only the routes restarts the module and
// keeps its incident (state carried by digest); disabling the module
// leaves its commands unavailable while the daemon serves.
func TestSupervisorReload(t *testing.T) {
	h := newSupervisorHarness(t, supervisedWeb)
	h.checker.down.Store(true)
	await(t, "service_failed", func() bool { return len(h.delivered("ops", supervisor.EventServiceFailed)) == 1 })
	incident := h.delivered("ops", supervisor.EventServiceFailed)[0].ID

	h.write(t, "supervisor/web.yaml", supervisedWeb[:len(supervisedWeb)-len("[ops]\n")]+"[pager]\n")
	res, apiErr := h.d.Reload(context.Background(), audit.Actor{Kind: audit.ActorSignal})
	if apiErr != nil || !slices.Contains(res.Restarted, "supervisor") {
		t.Fatalf("reload %+v %v", res, apiErr)
	}
	st, apiErr := h.status(t, "")
	if apiErr != nil || st.Services[0].IncidentID != incident || st.Services[0].State != "failed" {
		t.Fatalf("after a route change: %+v %v", st, apiErr)
	}
	h.checker.down.Store(false)
	await(t, "service_recovered on pager", func() bool { return len(h.delivered("pager", supervisor.EventServiceRecovered)) == 1 })
	if ev := h.delivered("pager", supervisor.EventServiceRecovered)[0]; ev.CorrelationID != incident {
		t.Errorf("recovery correlation %s, want %s", ev.CorrelationID, incident)
	}

	h.write(t, "sentinel.yaml", "version: 1\ndaemon:\n  socket: "+h.sock+"\n  state_dir: "+filepath.Join(h.dir, "state")+
		"\n  shutdown_timeout: 2s\nmodules:\n  supervisor: {enabled: false}\n")
	if _, apiErr := h.d.Reload(context.Background(), audit.Actor{Kind: audit.ActorSignal}); apiErr != nil {
		t.Fatalf("reload disabling the module: %v", apiErr)
	}
	if _, apiErr := h.status(t, ""); apiErr == nil || apiErr.Code != api.CodeUnavailable {
		t.Errorf("status of a disabled module: %+v", apiErr)
	}
}

// Nothing looks implemented before 3a-4: production registers the
// supervisor as planned, and enabling it is a configuration error.
func TestProductionKeepsSupervisorPlanned(t *testing.T) {
	reg, err := Modules()
	if err != nil {
		t.Fatal(err)
	}
	if a := reg.Availability()["supervisor"]; a != config.ModulePlanned {
		t.Errorf("supervisor availability %v, want planned", a)
	}
}

// callSocket sends req to the control socket at sock.
func callSocket(t *testing.T, sock string, req api.Request) api.Response {
	t.Helper()
	resp, err := transport.Call(context.Background(), sock, req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}
