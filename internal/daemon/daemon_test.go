package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/authz"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/config"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/events"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/module"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/notify"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/model"
)

const secret = "s3cret-in-a-panic"

// recorder collects the calls of every test module, in order.
type recorder struct {
	mu    sync.Mutex
	calls []string
}

func (r *recorder) add(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, s)
}

func (r *recorder) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls)
}

// hostile is a panic value whose methods must never run: formatting a
// panic value could put a secret in a log or an event.
type hostile struct{ formatted *atomic.Bool }

func (h hostile) Error() string  { h.formatted.Store(true); return secret }
func (h hostile) String() string { h.formatted.Store(true); return secret }

// testModule is a configured module whose behaviour each test chooses.
type testModule struct {
	name              string
	rec               *recorder
	startErr, stopErr error
	startPanic        any
	stopPanic         any
	stopBlock         chan struct{} // Stop waits for it, ignoring ctx
	startBlock        chan struct{} // Start waits for it, ignoring ctx
	onStart           func()        // called at the beginning of Start
	route             []string
	routePanics       bool
	rt                module.Runtime
}

func (m *testModule) Start(_ context.Context, rt module.Runtime) error {
	m.rec.add("start " + m.name)
	m.rt = rt
	if m.onStart != nil {
		m.onStart()
	}
	if m.startBlock != nil {
		<-m.startBlock
	}
	if m.startPanic != nil {
		panic(m.startPanic)
	}
	return m.startErr
}

func (m *testModule) Stop(context.Context) error {
	m.rec.add("stop " + m.name)
	if m.stopBlock != nil {
		<-m.stopBlock
	}
	if m.stopPanic != nil {
		panic(m.stopPanic)
	}
	return m.stopErr
}

func (m *testModule) NotificationChannels(model.Event) []string {
	if m.routePanics {
		panic("route")
	}
	return m.route
}

// sender records the payloads delivered to one channel.
type sender struct {
	mu       sync.Mutex
	payloads []notify.Payload
	bodies   []string
}

func (s *sender) Send(_ context.Context, body []byte) error {
	var p notify.Payload
	if err := json.Unmarshal(body, &p); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.payloads = append(s.payloads, p)
	s.bodies = append(s.bodies, string(body))
	return nil
}

func (s *sender) delivered() []notify.Payload {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.payloads)
}

// syncBuffer is a log destination safe for concurrent writers.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type harness struct {
	peer atomic.Pointer[authz.Peer] // who the control socket's clients are; nil: root
	d    *Daemon
	cfg  *config.Config
	ops  *sender
	logs *syncBuffer
	rec  *recorder
}

// newHarness builds a daemon over a temporary state directory with one
// webhook channel "ops", which receives the core's events.
func newHarness(t *testing.T, mods ...*testModule) *harness {
	t.Helper()
	h := &harness{ops: &sender{}, logs: &syncBuffer{}, rec: &recorder{}}
	h.cfg = &config.Config{
		Daemon: config.Daemon{
			Socket:          socketPath(t),
			SocketMode:      0o660,
			StateDir:        filepath.Join(t.TempDir(), "state"),
			ShutdownTimeout: config.Duration(8 * time.Second),
		},
		Notifications: config.Notifications{
			Channels: []config.Channel{{Name: "ops", Type: config.ChannelWebhook, Retry: config.RetryPolicy{Attempts: 1}}},
			Core:     config.Route{Channels: []string{"ops"}},
		},
	}
	var instances []module.Instance
	for _, m := range mods {
		m.rec = h.rec
		instances = append(instances, module.Instance{Name: m.name, Module: m})
	}
	h.d = New(h.cfg, instances, Options{
		Logger:    slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Hostname:  "web1",
		Version:   "test",
		NewSender: func(config.Channel) (notify.Sender, error) { return h.ops, nil },
		PeerCredentials: func(*net.UnixConn) (authz.Peer, error) {
			if p := h.peer.Load(); p != nil {
				return *p, nil
			}
			return authz.Peer{UID: 0}, nil
		},
		LookupGroup: func(name string) (uint32, error) {
			if gid, ok := map[string]uint32{"ops": 100, "fw": 200}[name]; ok {
				return gid, nil
			}
			return 0, errors.New("unknown group")
		},
	})
	return h
}

// socketPath returns a control socket path short enough for the Unix limit
// (t.TempDir paths can be too long on macOS).
func socketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "swd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "run", "s.sock")
}

func (h *harness) run(t *testing.T) error {
	t.Helper()
	if err := h.d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return h.d.Stop(context.Background())
}

// daemonErrors returns the daemon_error events delivered to ops.
func (h *harness) daemonErrors() []model.Event {
	var out []model.Event
	for _, p := range h.ops.delivered() {
		if p.Event.Type == model.EventDaemonError {
			out = append(out, p.Event)
		}
	}
	return out
}

// With no module, the daemon starts and stops its core services and
// leaves no goroutine behind (synctest fails the test otherwise).
func TestStartStopWithoutModules(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		if err := h.run(t); err != nil {
			t.Fatal(err)
		}
		if info, err := os.Stat(h.cfg.Daemon.StateDir); err != nil || !info.IsDir() {
			t.Errorf("state directory not created: %v", err)
		}
		if !strings.Contains(h.logs.String(), "sentineld ready") {
			t.Error("readiness not logged")
		}
	})
}

func TestModulesStopInReverseOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, &testModule{name: "a"}, &testModule{name: "b"}, &testModule{name: "c"})
		if err := h.run(t); err != nil {
			t.Fatal(err)
		}
		want := []string{"start a", "start b", "start c", "stop c", "stop b", "stop a"}
		if got := h.rec.list(); !slices.Equal(got, want) {
			t.Errorf("calls %v, want %v", got, want)
		}
		for name, s := range h.d.Snapshot().Modules {
			if s != ModuleStopped {
				t.Errorf("module %s: %s", name, s)
			}
		}
	})
}

// A module that fails or panics is reported (log and daemon_error) without
// the panic value; the other modules still start and stop.
func TestModuleFailuresAreIsolated(t *testing.T) {
	tests := []struct {
		name      string
		broken    func(*testModule, hostile)
		op        string
		failure   string
		wantState ModuleState
		wantCalls []string
	}{
		{"start error", func(m *testModule, _ hostile) { m.startErr = errors.New("port in use") }, "start", "error", ModuleFailed,
			[]string{"start a", "start broken", "stop broken", "start c", "stop c", "stop a"}},
		{"start panic", func(m *testModule, h hostile) { m.startPanic = h }, "start", "panic", ModuleFailed,
			[]string{"start a", "start broken", "stop broken", "start c", "stop c", "stop a"}},
		{"stop error", func(m *testModule, _ hostile) { m.stopErr = errors.New("busy") }, "stop", "error", ModuleFailed,
			[]string{"start a", "start broken", "start c", "stop c", "stop broken", "stop a"}},
		{"stop panic", func(m *testModule, h hostile) { m.stopPanic = h }, "stop", "panic", ModuleFailed,
			[]string{"start a", "start broken", "start c", "stop c", "stop broken", "stop a"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				payload := hostile{formatted: &atomic.Bool{}}
				broken := &testModule{name: "broken"}
				tt.broken(broken, payload)
				h := newHarness(t, &testModule{name: "a"}, broken, &testModule{name: "c"})
				err := h.run(t)
				if tt.op == "stop" && err == nil {
					t.Error("Stop did not report the failed module")
				}
				if got := h.rec.list(); !slices.Equal(got, tt.wantCalls) {
					t.Errorf("calls %v, want %v", got, tt.wantCalls)
				}
				if s := h.d.Snapshot().Modules["broken"]; s != tt.wantState {
					t.Errorf("state %s, want %s", s, tt.wantState)
				}
				errs := h.daemonErrors()
				if len(errs) != 1 || errs[0].Source != "broken" || errs[0].SourceType != model.SourceTypeModule ||
					errs[0].Attributes["operation"] != tt.op || errs[0].Attributes["failure"] != tt.failure {
					t.Errorf("daemon_error events: %+v", errs)
				}
				if payload.formatted.Load() {
					t.Error("the panic value was formatted")
				}
				for _, body := range h.ops.bodies {
					if strings.Contains(body, secret) || strings.Contains(body, "port in use") {
						t.Errorf("delivered event carries the module's error or panic: %s", body)
					}
				}
				if strings.Contains(h.logs.String(), secret) {
					t.Error("the panic value reached the log")
				}
				if tt.failure == "panic" && !strings.Contains(h.logs.String(), "daemon_test.go") {
					t.Error("the panic site is not logged")
				}
			})
		})
	}
}

// A module that does not stop in its share of shutdown_timeout is
// abandoned: the modules before it in start order still stop, the daemon
// returns an error and leaves the state directory open.
func TestHungStopIsAbandoned(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		h := newHarness(t, &testModule{name: "a"}, &testModule{name: "hung", stopBlock: release})
		if err := h.d.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		begin := time.Now()
		err := h.d.Stop(context.Background())
		if err == nil || !strings.Contains(err.Error(), "hung") {
			t.Errorf("Stop error = %v", err)
		}
		if elapsed := time.Since(begin); elapsed > h.cfg.Daemon.ShutdownTimeout.Std() {
			t.Errorf("Stop took %s, budget %s", elapsed, h.cfg.Daemon.ShutdownTimeout.Std())
		}
		if got := h.rec.list(); !slices.Equal(got, []string{"start a", "start hung", "stop hung", "stop a"}) {
			t.Errorf("calls %v", got)
		}
		snap := h.d.Snapshot()
		if snap.Modules["hung"] != ModuleAbandoned || snap.Modules["a"] != ModuleStopped {
			t.Errorf("states %v", snap.Modules)
		}
		if errs := h.daemonErrors(); len(errs) != 1 || errs[0].Attributes["failure"] != "timeout" {
			t.Errorf("daemon_error events: %+v", errs)
		}
		if !strings.Contains(h.logs.String(), "state directory left open") {
			t.Error("abandoned module did not keep the state directory open")
		}
		close(release) // let the abandoned goroutine end inside the bubble
		synctest.Wait()
		_ = h.d.state.Close() // left open by Stop; the test owns it now
	})
}

// Core events follow notifications.core, filters included; a module's
// events follow its Router; a panicking Router only loses the routing.
func TestRouting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		routed := &testModule{name: "routed", route: []string{"ops"}}
		silent := &testModule{name: "silent", routePanics: true}
		h := newHarness(t, routed, silent)
		h.cfg.Notifications.Core.Events = []model.EventType{model.EventConfigurationError} // daemon_error filtered out
		if err := h.d.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		for _, m := range []*testModule{routed, silent} {
			if err := m.rt.Events.Register(events.TypeSpec{Type: model.EventType(m.name + "_event"), Severity: model.SeverityWarning}); err != nil {
				t.Fatal(err)
			}
			if _, err := m.rt.Events.Publish(model.Event{Type: model.EventType(m.name + "_event"), Source: "x", SourceType: "test"}); err != nil {
				t.Fatal(err)
			}
		}
		h.d.report("routed", "start", time.Second, errors.New("filtered core event"))
		if err := h.d.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
		got := h.ops.delivered()
		if len(got) != 1 || got[0].Event.Type != "routed_event" || got[0].Event.Module != "routed" {
			t.Errorf("delivered %+v, want only routed_event", got)
		}
		if !strings.Contains(h.logs.String(), "notification route panicked") {
			t.Error("router panic not logged")
		}
	})
}

// The runtime is bound to its module: state opens under the module's own
// directory.
func TestRuntimeState(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := &testModule{name: "keeper"}
		h := newHarness(t, m)
		if err := h.d.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		store, err := m.rt.State(1)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Save(map[string]int{"restarts": 1}); err != nil {
			t.Fatal(err)
		}
		if err := h.d.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(h.cfg.Daemon.StateDir, "keeper", "state.json")); err != nil {
			t.Errorf("module state not saved in its directory: %v", err)
		}
	})
}

func TestFailedStartAndSingleUse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		h.cfg.Daemon.StateDir = "relative/state"
		if err := h.d.Start(context.Background()); err == nil {
			t.Fatal("relative state directory accepted")
		}
		if err := h.d.Stop(context.Background()); err != nil {
			t.Errorf("Stop after a failed Start: %v", err)
		}
		if err := h.d.Start(context.Background()); err == nil {
			t.Error("second Start accepted")
		}
	})
}

// Review 2c-1 R1: a module's error text is logged with the values the
// configuration knows to be secret masked.
func TestReturnedErrorDoesNotLeakSecrets(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const token = "regression-secret-12345"
		h := newHarness(t, &testModule{name: "broken", startErr: errors.New("request failed: Bearer " + token)})
		h.cfg.Notifications.Channels[0].Headers = map[string]string{"Authorization": "Bearer " + token}
		_ = h.run(t)
		if strings.Contains(h.logs.String(), token) {
			t.Error("a module error leaked a configured credential into the log")
		}
		if !strings.Contains(h.logs.String(), "request failed") {
			t.Error("the module error is not logged at all")
		}
	})
}

type blockingRouter struct {
	entered chan struct{}
	release chan struct{}
}

func (r *blockingRouter) NotificationChannels(model.Event) []string {
	close(r.entered)
	<-r.release
	return nil
}

// Review 2c-1 R2: a router (or a logger) that blocks cannot hold Stop past
// its budget: delivery is abandoned and Stop reports it.
func TestShutdownBoundsBlockedRouter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := &testModule{name: "hung"}
		h := newHarness(t, m)
		r := &blockingRouter{entered: make(chan struct{}), release: make(chan struct{})}
		h.d.routers[m.name] = r
		if err := h.d.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := m.rt.Events.Register(events.TypeSpec{Type: "hung_event", Severity: model.SeverityError}); err != nil {
			t.Fatal(err)
		}
		if _, err := m.rt.Events.Publish(model.Event{Type: "hung_event", Source: "x", SourceType: "test"}); err != nil {
			t.Fatal(err)
		}
		<-r.entered
		begin := time.Now()
		done := make(chan error, 1)
		go func() { done <- h.d.Stop(context.Background()) }()
		time.Sleep(h.cfg.Daemon.ShutdownTimeout.Std() + time.Second)
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), "abandoned") {
				t.Errorf("Stop error = %v", err)
			}
			if elapsed := time.Since(begin); elapsed > h.cfg.Daemon.ShutdownTimeout.Std()+time.Second {
				t.Errorf("Stop took %s", elapsed)
			}
		default:
			t.Error("Stop exceeded its shutdown budget")
		}
		close(r.release) // let the abandoned dispatcher end inside the bubble
		synctest.Wait()
		select {
		case <-done:
		default:
		}
	})
}

// Review 2c-1 R3: a module abandoned or failed during Start makes the
// shutdown unclean, so sentineld exits 1.
func TestStartFailuresMakeStopUnclean(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		h := newHarness(t, &testModule{name: "hung", startBlock: release}, &testModule{name: "bad", startErr: errors.New("no")})
		err := h.run(t)
		snap := h.d.Snapshot()
		if snap.Modules["hung"] != ModuleAbandoned || snap.Modules["bad"] != ModuleFailed {
			t.Fatalf("states %v", snap.Modules)
		}
		if err == nil || !strings.Contains(err.Error(), "hung") || !strings.Contains(err.Error(), "bad") {
			t.Errorf("Stop error = %v", err)
		}
		close(release)
		synctest.Wait()
		_ = h.d.state.Close() // left open by Stop; the test owns it now
	})
}

// Review 2c-1 R4: a cancelled start (a signal) lets the current module's
// Start return and starts no further module.
func TestCancelledStartStartsNoMoreModules(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		h := newHarness(t, &testModule{name: "a", onStart: cancel}, &testModule{name: "b"})
		if err := h.d.Start(ctx); err != nil {
			t.Fatal(err)
		}
		_ = h.d.Stop(context.Background())
		if got := h.rec.list(); slices.Contains(got, "start b") {
			t.Errorf("calls %v: b started after the cancellation", got)
		}
		if s := h.d.Snapshot().Modules["b"]; s != ModuleConfigured {
			t.Errorf("b is %s", s)
		}
	})
}
