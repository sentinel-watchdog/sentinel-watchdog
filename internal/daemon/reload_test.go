package daemon

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/audit"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/authz"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/clock"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/config"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/events"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/module"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/notify"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/api"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/model"
)

// reloadHarness runs a daemon over real configuration files, with the
// available test modules alpha and beta: every Configure makes a new
// testModule, so a test sees which instance runs.
type reloadHarness struct {
	*harness
	dir, main, sock string
	level           *slog.LevelVar

	mu        sync.Mutex
	instances map[string][]*testModule
	senders   []*sender         // one per channel built, in order
	tweak     func(*testModule) // applied to every new instance
}

// factory is an available test module whose Configure records the new
// instance in its harness.
type factory struct {
	name string
	h    *reloadHarness
}

func (f factory) Name() string { return f.name }

func (f factory) Commands() []api.Command {
	return []api.Command{{Name: f.name + ".ping", Tier: api.TierRead}, {Name: f.name + ".kick", Tier: api.TierOperate}}
}

func (f factory) Configure(config.ModuleConfig) (module.Configured, error) {
	m := &testModule{name: f.name, rec: f.h.rec}
	f.h.mu.Lock()
	defer f.h.mu.Unlock()
	if f.h.tweak != nil {
		f.h.tweak(m)
	}
	f.h.instances[f.name] = append(f.h.instances[f.name], m)
	return m, nil
}

const baseModules = "modules:\n  alpha: {enabled: true}\n  beta: {enabled: true}\n"

// newReloadHarness writes the central file (daemon and notification
// settings, then central) and the module files, loads them and builds the
// daemon. The daemon is stopped at the end of the test.
func newReloadHarness(t *testing.T, daemonExtra, central string, files map[string]string) *reloadHarness {
	t.Helper()
	return newReloadHarnessWith(t, nil, daemonExtra, central, files)
}

// newReloadHarnessWith is newReloadHarness with tweak applied to every
// module instance, the first ones included.
func newReloadHarnessWith(t *testing.T, tweak func(*testModule), daemonExtra, central string, files map[string]string) *reloadHarness {
	t.Helper()
	h := &reloadHarness{harness: &harness{ops: &sender{}, logs: &syncBuffer{}, rec: &recorder{}},
		dir: t.TempDir(), sock: socketPath(t), level: new(slog.LevelVar), instances: map[string][]*testModule{},
		tweak: tweak}
	h.main = filepath.Join(h.dir, "sentinel.yaml")
	h.rewriteCentral(t, daemonExtra, "https://hooks.example.com/1", central)
	h.write(t, files)
	reg, err := Modules()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alpha", "beta"} {
		if err := reg.Register(func() module.Module { return factory{name: name, h: h} }); err != nil {
			t.Fatal(err)
		}
	}
	cfg, mods, err := Load(reg, h.main)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	h.cfg = cfg
	h.d = New(cfg, mods, Options{
		Logger:   slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: h.level})),
		LogLevel: h.level, Hostname: "web1", Version: "test", Commands: reg.Commands(),
		Load: func() (*config.Config, []module.Instance, error) { return Load(reg, h.main) },
		NewSender: func(config.Channel) (notify.Sender, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			s := h.ops // the first channel built is the harness's ops
			if len(h.senders) > 0 {
				s = &sender{}
			}
			h.senders = append(h.senders, s)
			return s, nil
		},
		PeerCredentials: func(*net.UnixConn) (authz.Peer, error) {
			if p := h.peer.Load(); p != nil {
				return *p, nil
			}
			return authz.Peer{UID: 0}, nil
		},
		LookupGroup: func(name string) (uint32, error) {
			if gid, ok := map[string]uint32{"ops": 100}[name]; ok {
				return gid, nil
			}
			return 0, errors.New("unknown group")
		},
	})
	if err := h.d.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.d.Stop(context.Background()) })
	return h
}

// rewriteCentral writes the central file: the harness's socket and state
// directory, daemonExtra, the channel ops at url, then central.
func (h *reloadHarness) rewriteCentral(t *testing.T, daemonExtra, url, central string) {
	t.Helper()
	h.write(t, map[string]string{"sentinel.yaml": "version: 1\ndaemon:\n  socket: " + h.sock + "\n  state_dir: " +
		filepath.Join(h.dir, "state") + "\n  shutdown_timeout: 2s\n" + daemonExtra +
		"notifications:\n  channels:\n    - {name: ops, type: webhook, url: \"" + url + "\"}\n  core: [ops]\n" + central})
}

// write replaces files atomically (a temporary file renamed over the old
// one, as editors save): a reload running at the same time reads the old
// or the new content, never a file just truncated (a CI flake of
// TestReloadLeavesNoGoroutine: "file is empty").
func (h *reloadHarness) write(t *testing.T, files map[string]string) {
	t.Helper()
	for name, body := range files {
		path := filepath.Join(h.dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		// A hidden name: the loader ignores it in a module directory.
		tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tmp.WriteString(body); err != nil {
			t.Fatal(err)
		}
		if err := tmp.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp.Name(), path); err != nil {
			t.Fatal(err)
		}
	}
}

func (h *reloadHarness) reload(t *testing.T) (api.ReloadResult, *api.Error) {
	t.Helper()
	return h.d.Reload(context.Background(), audit.Actor{Kind: audit.ActorSignal})
}

func (h *reloadHarness) count(name string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.instances[name])
}

func (h *reloadHarness) instance(name string, i int) *testModule {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.instances[name][i]
}

// delivered returns the events of type typ delivered to any channel.
func (h *reloadHarness) delivered(typ model.EventType) []model.Event {
	h.mu.Lock()
	senders := slices.Clone(h.senders)
	h.mu.Unlock()
	var out []model.Event
	for _, s := range senders {
		for _, p := range s.delivered() {
			if p.Event.Type == typ {
				out = append(out, p.Event)
			}
		}
	}
	return out
}

func (h *reloadHarness) waitDelivered(t *testing.T, typ model.EventType) []model.Event {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if evs := h.delivered(typ); len(evs) > 0 || time.Now().After(deadline) {
			return evs
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// auditRecords returns the core.config_reload records.
func (h *reloadHarness) auditRecords(t *testing.T) []audit.Record {
	t.Helper()
	recs, _, err := h.d.audit.Tail(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []audit.Record
	for _, r := range recs {
		if r.Action == "core.config_reload" {
			out = append(out, r)
		}
	}
	return out
}

const alphaV1, alphaV2 = "version: 1\nsettings:\n  x: 1\n", "version: 1\nsettings:\n  x: 2\n"

// Only the module whose configuration changed is stopped and started
// again; the other keeps running; the reload is audited with both digests
// and announced (D-075).
func TestReloadRestartsOnlyChangedModules(t *testing.T) {
	h := newReloadHarness(t, "", baseModules, map[string]string{"alpha/10.yaml": alphaV1, "beta/10.yaml": alphaV1})
	oldDigest := h.d.status().ConfigDigest
	h.write(t, map[string]string{"alpha/10.yaml": alphaV2})
	res, apiErr := h.reload(t)
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	if !slices.Equal(res.Restarted, []string{"alpha"}) || len(res.Started)+len(res.Stopped)+len(res.Failed) != 0 ||
		res.OldDigest != oldDigest || res.Digest == oldDigest {
		t.Errorf("result %+v", res)
	}
	// Load configures every enabled module (Configure is pure); only the
	// changed one's new instance is started.
	if h.count("alpha") != 2 {
		t.Errorf("alpha instances: %d", h.count("alpha"))
	}
	if !slices.Equal(h.rec.list(), []string{"start alpha", "start beta", "stop alpha", "start alpha"}) {
		t.Errorf("calls %v", h.rec.list())
	}
	st := h.d.status()
	if st.ConfigDigest != res.Digest || st.LastReload == nil || st.LastReload.Result != "ok" {
		t.Errorf("status %+v %+v", st.ConfigDigest, st.LastReload)
	}
	recs := h.auditRecords(t)
	if len(recs) != 2 || recs[0].Result != audit.ResultIntent || recs[1].Result != audit.ResultOK ||
		recs[1].Detail["old_digest"] != oldDigest || recs[1].Detail["new_digest"] != res.Digest ||
		recs[1].Detail["restarted"] != "alpha" || recs[0].CorrelationID != recs[1].CorrelationID {
		t.Errorf("audit %+v", recs)
	}
	if evs := h.waitDelivered(t, model.EventConfigReloaded); len(evs) != 1 {
		t.Errorf("config_reloaded events: %d", len(evs))
	}
	// Again, unchanged: nothing restarts.
	res, apiErr = h.reload(t)
	if apiErr != nil || len(res.Restarted)+len(res.Started)+len(res.Stopped) != 0 || res.Digest != res.OldDigest {
		t.Errorf("unchanged reload: %+v %v", res, apiErr)
	}
}

// An invalid configuration changes nothing: the modules keep running and
// the problems go back to the caller (D-075).
func TestReloadRejectsAnInvalidConfiguration(t *testing.T) {
	h := newReloadHarness(t, "", baseModules, map[string]string{"alpha/10.yaml": alphaV1})
	digest := h.d.status().ConfigDigest
	h.write(t, map[string]string{"alpha/10.yaml": "version: 1\nsettings: [unclosed\n"})
	_, apiErr := h.reload(t)
	if apiErr == nil || apiErr.Code != api.CodeInvalidConfig || !strings.Contains(apiErr.Message, "10.yaml") {
		t.Fatalf("reload: %+v", apiErr)
	}
	if calls := h.rec.list(); slices.Contains(calls, "stop alpha") || slices.Contains(calls, "stop beta") {
		t.Errorf("a module was stopped: %v", calls)
	}
	st := h.d.status()
	if st.ConfigDigest != digest || st.LastReload == nil || st.LastReload.Result != "rejected" {
		t.Errorf("status %s %+v", st.ConfigDigest, st.LastReload)
	}
	if recs := h.auditRecords(t); len(recs) != 2 || recs[1].Result != audit.ResultFailed || recs[1].Detail["reason"] != "invalid_configuration" {
		t.Errorf("audit %+v", recs)
	}
	if evs := h.waitDelivered(t, model.EventConfigReloadFailed); len(evs) != 1 || strings.Contains(evs[0].Message, "10.yaml") {
		t.Errorf("config_reload_failed events %+v (the problems stay local)", evs)
	}
}

// Enabling and disabling modules starts and stops them.
func TestReloadEnablesAndDisablesModules(t *testing.T) {
	h := newReloadHarness(t, "", "modules:\n  alpha: {enabled: true}\n  beta: {enabled: false}\n", nil)
	h.rewriteCentral(t, "", "https://hooks.example.com/1", "modules:\n  alpha: {enabled: false}\n  beta: {enabled: true}\n")
	res, apiErr := h.reload(t)
	if apiErr != nil || !slices.Equal(res.Stopped, []string{"alpha"}) || !slices.Equal(res.Started, []string{"beta"}) {
		t.Fatalf("result %+v %v", res, apiErr)
	}
	mods := map[string]string{}
	for _, m := range h.d.modules() {
		mods[m.Name] = m.State
	}
	if mods["alpha"] != "" || mods["beta"] != "running" {
		t.Errorf("modules %v", mods)
	}
}

// A module whose old instance does not stop is never started beside it;
// it stays tracked, so Stop keeps the state directory and the lock.
func TestReloadNeverStartsBesideAStuckInstance(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	h := newReloadHarness(t, "", baseModules, map[string]string{"alpha/10.yaml": alphaV1})
	h.instance("alpha", 0).stopBlock = release
	h.write(t, map[string]string{"alpha/10.yaml": alphaV2})
	res, apiErr := h.reload(t)
	if apiErr != nil || !slices.Equal(res.Failed, []string{"alpha"}) || len(res.Restarted) != 0 || len(res.Warnings) == 0 {
		t.Fatalf("result %+v %v", res, apiErr)
	}
	if slices.Contains(h.rec.list()[2:], "start alpha") {
		t.Errorf("a second alpha started: %v", h.rec.list())
	}
	// A later reload does not start it either.
	h.write(t, map[string]string{"alpha/10.yaml": "version: 1\nsettings:\n  x: 3\n"})
	if res, _ := h.reload(t); len(res.Restarted) != 0 || len(res.Warnings) == 0 {
		t.Errorf("second reload %+v", res)
	}
	if st := h.d.Snapshot().Modules["alpha"]; st != ModuleAbandoned {
		t.Errorf("alpha is %s", st)
	}
}

// Keys bound at start-up keep their value with a warning; the log level
// and the access groups apply at once.
func TestReloadDaemonKeys(t *testing.T) {
	h := newReloadHarness(t, "", "", nil)
	operator := authz.Peer{UID: 1000, GID: 1000, Groups: []uint32{100}}
	h.peer.Store(&operator)
	if resp := h.call(t, api.CoreReload.Name, ""); resp.Error == nil || resp.Error.Code != api.CodePermissionDenied {
		t.Fatalf("operator before: %+v", resp.Error)
	}
	h.peer.Store(nil)
	oldState := h.cfg.Daemon.StateDir
	h.rewriteCentral(t, "  socket_mode: \"0600\"\n  log: {level: debug}\n  access: {operator_group: ops}\n",
		"https://hooks.example.com/1", "")
	s := strings.Replace(readFile(t, h.main), "state_dir: "+oldState, "state_dir: "+oldState+"2", 1)
	h.write(t, map[string]string{"sentinel.yaml": s})
	res, apiErr := h.reload(t)
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	joined := strings.Join(res.Warnings, "\n")
	for _, key := range []string{"daemon.socket_mode", "daemon.state_dir"} {
		if !strings.Contains(joined, key) {
			t.Errorf("no warning for %s: %v", key, res.Warnings)
		}
	}
	cfg := h.d.config()
	if cfg.Daemon.StateDir != oldState || cfg.Daemon.SocketMode != h.cfg.Daemon.SocketMode {
		t.Errorf("bound keys changed: %s %v", cfg.Daemon.StateDir, cfg.Daemon.SocketMode)
	}
	if h.level.Level() != slog.LevelDebug {
		t.Errorf("log level %v", h.level.Level())
	}
	h.peer.Store(&operator)
	if resp := h.call(t, api.CoreReload.Name, ""); resp.Error != nil {
		t.Errorf("operator after the reload: %+v", resp.Error)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// A change of the channels replaces the dispatcher: events after the
// reload go to the new channel.
func TestReloadReplacesTheDispatcher(t *testing.T) {
	h := newReloadHarness(t, "", "", nil)
	h.rewriteCentral(t, "", "https://hooks.example.com/2", "")
	if _, apiErr := h.reload(t); apiErr != nil {
		t.Fatal(apiErr)
	}
	h.mu.Lock()
	n := len(h.senders)
	last := h.senders[n-1]
	h.mu.Unlock()
	if n != 2 {
		t.Fatalf("%d channels built, want 2", n)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !slices.ContainsFunc(last.delivered(), func(p notify.Payload) bool { return p.Event.Type == model.EventConfigReloaded }) {
		if time.Now().After(deadline) {
			t.Fatal("config_reloaded not delivered to the new channel")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if slices.ContainsFunc(h.ops.delivered(), func(p notify.Payload) bool { return p.Event.Type == model.EventConfigReloaded }) {
		t.Error("the old channel received an event after the reload")
	}
}

// Deliveries still queued for the replaced channels are abandoned and
// counted, never sent to the old endpoint, even once it answers again
// (D-077); the old dispatcher returns before the reload does.
func TestReloadAbandonsTheOldQueue(t *testing.T) {
	h := newReloadHarness(t, "", "", nil)
	release := make(chan struct{})
	h.ops.block = release
	h.d.publishCore(model.EventDaemonError, "first", nil)         // in flight, blocked
	h.d.publishCore(model.EventConfigurationError, "second", nil) // queued behind it
	h.rewriteCentral(t, "", "https://hooks.example.com/2", "")
	if _, apiErr := h.reload(t); apiErr != nil {
		t.Fatal(apiErr)
	}
	close(release) // the old endpoint answers again
	if err := h.d.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := h.ops.delivered(); len(got) != 0 {
		t.Errorf("the old channel received %d events after the reload", len(got))
	}
	if n := h.d.Snapshot().NotifyAbandoned; n == 0 {
		t.Error("no abandoned delivery counted")
	}
	if strings.Contains(h.logs.String(), "did not return in time") {
		t.Errorf("the replaced dispatcher was waited for:\n%s", h.logs.String())
	}
}

// setShutdownTimeout rewrites the central file's shutdown_timeout.
func (h *reloadHarness) setShutdownTimeout(t *testing.T, from, to string) {
	t.Helper()
	h.write(t, map[string]string{"sentinel.yaml": strings.Replace(readFile(t, h.main),
		"shutdown_timeout: "+from, "shutdown_timeout: "+to, 1)})
}

// Review 2c-3b R1: a router that blocks holds the dispatcher and then the
// forwarder; a reload that replaces the channels still returns within its
// budget instead of waiting for the forwarder forever.
func TestReloadBoundsBlockedRouter(t *testing.T) {
	h := newReloadHarness(t, "", baseModules, nil)
	r := &blockingRouter{entered: make(chan struct{}), release: make(chan struct{})}
	defer close(r.release)
	h.d.viewMu.Lock()
	h.d.view.routers["alpha"] = r
	h.d.viewMu.Unlock()
	m := h.instance("alpha", 0)
	if err := m.rt.Events.Register(events.TypeSpec{Type: "alpha_test", Severity: model.SeverityError}); err != nil {
		t.Fatal(err)
	}
	for i := range 2 { // the first blocks the dispatcher, the second the forwarder
		if _, err := m.rt.Events.Publish(model.Event{Type: "alpha_test", Source: fmt.Sprint(i), SourceType: "test"}); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			<-r.entered
		}
	}
	time.Sleep(50 * time.Millisecond) // let the forwarder take the second event
	h.rewriteCentral(t, "", "https://hooks.example.com/2", baseModules)
	done := make(chan *api.Error, 1)
	go func() {
		_, apiErr := h.reload(t)
		done <- apiErr
	}()
	select {
	case apiErr := <-done:
		if apiErr != nil {
			t.Error(apiErr)
		}
	case <-time.After(reloadBudget(h.cfg) + time.Second):
		t.Fatal("reload exceeded its budget")
	}
}

// Review 2c-3b R2: every module call of a reload is bounded by what is
// left of the reload's budget, not by a full shutdown_timeout, even when
// the reload raises shutdown_timeout.
func TestReloadBoundsModuleCallsByItsBudget(t *testing.T) {
	h := newReloadHarness(t, "", baseModules, map[string]string{"alpha/10.yaml": alphaV1, "beta/10.yaml": alphaV1})
	h.setShutdownTimeout(t, "2s", "1s") // a budget of 2s
	if _, apiErr := h.reload(t); apiErr != nil {
		t.Fatal(apiErr)
	}
	release := make(chan struct{})
	defer close(release)
	h.mu.Lock()
	h.tweak = func(m *testModule) { m.startBlock = release } // new instances hang in Start
	h.mu.Unlock()
	h.setShutdownTimeout(t, "1s", "3s")
	h.write(t, map[string]string{"alpha/10.yaml": alphaV2, "beta/10.yaml": alphaV2})
	begin := time.Now()
	res, apiErr := h.reload(t)
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	if took := time.Since(begin); took > 2*time.Second+500*time.Millisecond {
		t.Errorf("reload took %v, budget 2s", took)
	}
	if len(res.Failed) != 2 {
		t.Errorf("result %+v: both restarts should fail", res)
	}
}

// Review 2c-3b R3: an instance whose Stop failed may still run: no later
// reload starts another beside it, and disabling it keeps it tracked.
func TestReloadRemembersAFailedStop(t *testing.T) {
	h := newReloadHarness(t, "", baseModules, map[string]string{"alpha/10.yaml": alphaV1})
	h.instance("alpha", 0).stopErr = errors.New("still active")
	h.write(t, map[string]string{"alpha/10.yaml": alphaV2})
	if res, apiErr := h.reload(t); apiErr != nil || !slices.Equal(res.Failed, []string{"alpha"}) {
		t.Fatalf("first reload: %+v %v", res, apiErr)
	}
	h.write(t, map[string]string{"alpha/10.yaml": "version: 1\nsettings:\n  x: 3\n"})
	res, apiErr := h.reload(t)
	if apiErr != nil || slices.Contains(res.Restarted, "alpha") || len(res.Warnings) == 0 {
		t.Errorf("second reload: %+v %v", res, apiErr)
	}
	h.rewriteCentral(t, "", "https://hooks.example.com/1", "modules:\n  alpha: {enabled: false}\n  beta: {enabled: true}\n")
	if _, apiErr := h.reload(t); apiErr != nil {
		t.Fatal(apiErr)
	}
	if got := h.d.Snapshot().Modules["alpha"]; got != ModuleFailed {
		t.Errorf("alpha is %q after it was disabled; the instance that did not stop must stay tracked", got)
	}
	if slices.Contains(h.rec.list()[2:], "start alpha") {
		t.Errorf("a second alpha started: %v", h.rec.list())
	}
}

// Review 2c-3b R4: the socket holds a reload's exchange for the longest
// budget any valid configuration can give, since a reload may raise
// shutdown_timeout after the socket was opened.
func TestReloadExchangeCoversTheLongestBudget(t *testing.T) {
	longest := &config.Config{Daemon: config.Daemon{ShutdownTimeout: config.Duration(config.MaxShutdownTimeout)}}
	if reloadExchange <= reloadBudget(longest) {
		t.Errorf("exchange %v, longest reload budget %v", reloadExchange, reloadBudget(longest))
	}
}

// Review 2c-3b, second round, R1: the cleanup Stop after a failed Start
// gets what is left of the budget, not the Start's allowance again.
func TestReloadBoundsFailedStartCleanup(t *testing.T) {
	h := newReloadHarness(t, "", baseModules, map[string]string{"alpha/10.yaml": alphaV1})
	h.setShutdownTimeout(t, "2s", "1s") // a budget of 2s
	if _, apiErr := h.reload(t); apiErr != nil {
		t.Fatal(apiErr)
	}
	release := make(chan struct{})
	defer close(release)
	h.mu.Lock()
	h.tweak = func(m *testModule) {
		m.onStart = func() { time.Sleep(1500 * time.Millisecond) }
		m.startErr = errors.New("start failed")
		m.stopBlock = release
	}
	h.mu.Unlock()
	h.setShutdownTimeout(t, "1s", "3s")
	h.write(t, map[string]string{"alpha/10.yaml": alphaV2})
	begin := time.Now()
	if _, apiErr := h.reload(t); apiErr != nil {
		t.Fatal(apiErr)
	}
	if took := time.Since(begin); took > 2500*time.Millisecond {
		t.Errorf("reload took %v, budget 2s", took)
	}
}

// Review 2c-3b, second round, R2: waiting for the replaced dispatcher is
// part of the budget, even when the reload raises shutdown_timeout.
func TestReloadBoundsDispatcherRetirement(t *testing.T) {
	h := newReloadHarness(t, "", baseModules, nil)
	h.setShutdownTimeout(t, "2s", "1s") // a budget of 2s
	if _, apiErr := h.reload(t); apiErr != nil {
		t.Fatal(apiErr)
	}
	r := &blockingRouter{entered: make(chan struct{}), release: make(chan struct{})}
	defer close(r.release)
	h.d.viewMu.Lock()
	h.d.view.routers["alpha"] = r
	h.d.viewMu.Unlock()
	m := h.instance("alpha", 0)
	if err := m.rt.Events.Register(events.TypeSpec{Type: "alpha_test", Severity: model.SeverityError}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.rt.Events.Publish(model.Event{Type: "alpha_test", Source: "x", SourceType: "test"}); err != nil {
		t.Fatal(err)
	}
	<-r.entered
	h.rewriteCentral(t, "", "https://hooks.example.com/2", baseModules)
	h.setShutdownTimeout(t, "2s", "12s") // a quarter is 3s
	begin := time.Now()
	if _, apiErr := h.reload(t); apiErr != nil {
		t.Fatal(apiErr)
	}
	if took := time.Since(begin); took > 2500*time.Millisecond {
		t.Errorf("reload took %v, budget 2s", took)
	}
}

// Review 2c-3b, second round, R3: an instance whose Stop failed during a
// reload may still run, so Stop keeps the state directory and the socket
// lock, as for an abandoned one.
func TestShutdownKeepsResourcesAfterAFailedReloadStop(t *testing.T) {
	h := newReloadHarness(t, "", baseModules, map[string]string{"alpha/10.yaml": alphaV1})
	defer h.d.server.Release()
	defer func() { _ = h.d.state.Close() }()
	h.instance("alpha", 0).stopErr = errors.New("still active")
	h.write(t, map[string]string{"alpha/10.yaml": alphaV2})
	if _, apiErr := h.reload(t); apiErr != nil {
		t.Fatal(apiErr)
	}
	if err := h.d.Stop(context.Background()); err == nil {
		t.Error("Stop did not report the module that did not stop")
	}
	if root, err := h.d.state.OpenRoot(); err != nil {
		t.Errorf("state directory closed beside an instance that may run: %v", err)
	} else {
		_ = root.Close()
	}
	if server, err := h.d.listen(); err == nil {
		_ = server.Close(context.Background())
		t.Error("socket lock released beside an instance that may run")
	}
}

// Review 2c-3b, second round, R4: a reload whose preparation outlasts a
// Stop that gave up waiting for it changes nothing and returns, instead of
// handing a dispatcher to a forwarder that has ended.
func TestReloadAfterAShutdownThatDidNotWait(t *testing.T) {
	h := newReloadHarness(t, "", "", nil)
	h.rewriteCentral(t, "", "https://hooks.example.com/2", "")
	load := h.d.opts.Load
	entered, release := make(chan struct{}), make(chan struct{})
	h.d.opts.Load = func() (*config.Config, []module.Instance, error) {
		close(entered)
		<-release
		return load()
	}
	done := make(chan *api.Error, 1)
	go func() {
		_, apiErr := h.reload(t)
		done <- apiErr
	}()
	<-entered
	_ = h.d.Stop(context.Background()) // gives up on the gate: a reload is running
	close(release)
	select {
	case apiErr := <-done:
		if apiErr == nil || apiErr.Code != api.CodeUnavailable {
			t.Errorf("reload after shutdown: %+v", apiErr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("reload remained blocked after shutdown")
	}
}

// Review 2c-3b, third round, R1: a preparation that blocks (a stalled
// file system or group lookup) does not hold the reload past its budget;
// while it still runs, a new reload is refused instead of piling up.
func TestReloadBoundsPreparation(t *testing.T) {
	h := newReloadHarness(t, "", "", nil)
	h.setShutdownTimeout(t, "2s", "1s") // a budget of 2s
	if _, apiErr := h.reload(t); apiErr != nil {
		t.Fatal(apiErr)
	}
	load := h.d.opts.Load
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	h.d.opts.Load = func() (*config.Config, []module.Instance, error) {
		once.Do(func() { close(entered) })
		<-release
		return load()
	}
	begin := time.Now()
	_, apiErr := h.reload(t)
	<-entered
	if took := time.Since(begin); took > 2500*time.Millisecond || apiErr == nil || apiErr.Code != api.CodeUnavailable {
		t.Errorf("blocked preparation: %v after %v", apiErr, took)
	}
	if _, apiErr := h.reload(t); apiErr == nil || apiErr.Code != api.CodeUnavailable ||
		!strings.Contains(apiErr.Message, "still") {
		t.Errorf("reload beside a running preparation: %+v", apiErr)
	}
	close(release)
	h.d.opts.Load = load
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, apiErr := h.reload(t)
		if apiErr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("reload after the preparation ended: %+v", apiErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Review 2c-3b, third round, R2: the old dispatcher is cancelled before
// its input closes, so it never drains its queue to the replaced endpoint,
// even when the endpoint answers again during the swap.
func TestReplacementDoesNotDrainTheOldQueue(t *testing.T) {
	h := newReloadHarness(t, "", "", nil)
	h.rewriteCentral(t, "", "https://hooks.example.com/2", "")
	p, err := h.d.prepare(h.d.config())
	if err != nil {
		t.Fatal(err)
	}
	h.d.mu.Lock()
	old := h.d.gen
	h.d.mu.Unlock()
	release := make(chan struct{})
	h.ops.block = release
	h.d.publishCore(model.EventDaemonError, "first", nil)
	h.d.publishCore(model.EventConfigurationError, "second", nil)
	h.d.publishCore(model.EventConfigReloaded, "third", nil)
	deadline := time.Now().Add(5 * time.Second)
	for queued(old) < 2 {
		if time.Now().After(deadline) {
			close(release)
			t.Fatal("the old queue did not fill")
		}
		time.Sleep(time.Millisecond)
	}
	h.d.mu.Lock() // holds the reload after the swap, before anything else it does
	done := make(chan bool, 1)
	go func() { done <- h.d.replaceDispatcher(context.Background(), p.dispatcher, time.Second) }()
	select {
	case <-old.done: // ended by the swap alone
	case <-time.After(5 * time.Second):
		select {
		case <-old.in: // closed, not cancelled: let the endpoint answer
		default:
		}
	}
	close(release)
	select {
	case <-old.done:
	case <-time.After(5 * time.Second):
		t.Error("the old dispatcher did not finish")
	}
	got := h.ops.delivered()
	h.d.mu.Unlock()
	<-done
	if len(got) != 0 {
		t.Errorf("%d queued deliveries reached the replaced endpoint", len(got))
	}
}

// Review 2c-3b, fourth round, R1: deliveries the old dispatcher takes
// from its queue and discards once cancelled are counted as abandoned,
// even when it does so before the queue could be measured.
func TestReplacementCountsDiscardedQueuedDeliveries(t *testing.T) {
	h := newReloadHarness(t, "", "", nil)
	h.rewriteCentral(t, "", "https://hooks.example.com/2", "")
	p, err := h.d.prepare(h.d.config())
	if err != nil {
		t.Fatal(err)
	}
	h.d.mu.Lock()
	old := h.d.gen
	h.d.mu.Unlock()
	release := make(chan struct{})
	defer close(release)
	h.ops.block = release
	h.d.publishCore(model.EventDaemonError, "first", nil)         // in flight
	h.d.publishCore(model.EventConfigurationError, "second", nil) // queued
	h.d.publishCore(model.EventConfigReloaded, "third", nil)      // queued
	deadline := time.Now().Add(5 * time.Second)
	for {
		if s := old.dispatcher.Stats()["ops"]; len(h.ops.delivered()) == 0 && s.Delivered == 0 && queued(old) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the old queue did not fill")
		}
		time.Sleep(time.Millisecond)
	}
	cancel := old.cancel
	old.cancel = func() { // the old dispatcher discards its queue before anything else runs
		cancel()
		<-old.done
	}
	if !h.d.replaceDispatcher(context.Background(), p.dispatcher, time.Second) {
		t.Fatal("replacement failed")
	}
	if got := h.d.Snapshot().NotifyAbandoned; got != 2 {
		t.Errorf("abandoned %d, want 2", got)
	}
}

// delayedCancelSender returns only after its context ends and release
// is closed: a delivery slow to give up.
type delayedCancelSender struct{ release <-chan struct{} }

func (s delayedCancelSender) Send(ctx context.Context, _ []byte) error {
	<-ctx.Done()
	<-s.release
	return ctx.Err()
}

// Review 2c-3b, fifth round, R1: a replaced dispatcher that returns after
// the reload stopped waiting for it still has its abandoned deliveries
// counted, once, when it returns.
func TestReplacementCountsAbandonmentAfterRetirementTimeout(t *testing.T) {
	h := newReloadHarness(t, "", "", nil)
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	oldDispatcher, err := notify.NewDispatcher(h.d.config().Notifications.Channels,
		func(model.Event) []string { return []string{"ops"} },
		notify.Options{NewSender: func(config.Channel) (notify.Sender, error) { return delayedCancelSender{release}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	if !h.d.replaceDispatcher(context.Background(), oldDispatcher, time.Second) {
		t.Fatal("installing the old dispatcher failed")
	}
	h.d.mu.Lock()
	old := h.d.gen
	h.d.mu.Unlock()
	before := h.d.Snapshot().NotifyAbandoned
	for i := range 3 {
		h.d.publishCore(model.EventDaemonError, fmt.Sprint(i), map[string]any{"i": i})
	}
	deadline := time.Now().Add(5 * time.Second)
	for queued(old) != 2 {
		if time.Now().After(deadline) {
			t.Fatal("the old queue did not fill")
		}
		time.Sleep(time.Millisecond)
	}
	next, err := h.d.newDispatcher(h.d.config())
	if err != nil {
		t.Fatal(err)
	}
	if !h.d.replaceDispatcher(context.Background(), next, time.Millisecond) {
		t.Fatal("replacement failed")
	}
	once.Do(func() { close(release) })
	select {
	case <-old.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the old dispatcher did not return")
	}
	if got := h.d.Snapshot().NotifyAbandoned - before; got != 2 {
		t.Errorf("abandoned %d, want 2", got)
	}
}

// queued counts the deliveries waiting in g's channel queues.
func queued(g *generation) int {
	n := 0
	for _, s := range g.dispatcher.Stats() {
		n += s.Queued
	}
	return n
}

// Review 2c-3b, sixth round, R1: a problem found after the files loaded
// (here an access group that does not resolve) is redacted with the new
// configuration, the only one that knows the values it expanded.
func TestReloadRedactsPreparationErrors(t *testing.T) {
	h := newReloadHarness(t, "", "", nil)
	const secret = "reload-private-token"
	t.Setenv("SENTINEL_TEST_RELOAD_SECRET", secret)
	h.rewriteCentral(t, "  access: {operator_group: \"${SENTINEL_TEST_RELOAD_SECRET}\"}\n", "https://hooks.example.com/1", "")
	_, apiErr := h.reload(t)
	if apiErr == nil || apiErr.Code != api.CodeInvalidConfig {
		t.Fatalf("reload: %+v", apiErr)
	}
	if strings.Contains(apiErr.Message, secret) {
		t.Errorf("the answer shows the expanded value: %s", apiErr.Message)
	}
	if strings.Contains(h.logs.String(), secret) {
		t.Error("the log shows the expanded value")
	}
}

// Review 2c-3b, seventh round, R1: a change that an earlier failed Stop
// blocks (here disabling the module) is reported as failed, not as a
// successful reload.
func TestReloadReportsABlockedDisable(t *testing.T) {
	h := newReloadHarness(t, "", baseModules, map[string]string{"alpha/10.yaml": alphaV1})
	h.instance("alpha", 0).stopErr = errors.New("still active")
	h.write(t, map[string]string{"alpha/10.yaml": alphaV2})
	if res, apiErr := h.reload(t); apiErr != nil || !slices.Contains(res.Failed, "alpha") {
		t.Fatalf("first reload: %+v %v", res, apiErr)
	}
	h.rewriteCentral(t, "", "https://hooks.example.com/1", "modules:\n  alpha: {enabled: false}\n  beta: {enabled: true}\n")
	res, apiErr := h.reload(t)
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	if !slices.Contains(res.Failed, "alpha") || len(res.Warnings) == 0 {
		t.Errorf("a blocked disable reported as applied: %+v", res)
	}
	if last := h.d.status().LastReload; last == nil || last.Result != "partial" {
		t.Errorf("last reload %+v", last)
	}
	if recs := h.auditRecords(t); recs[len(recs)-1].Result != audit.ResultFailed {
		t.Errorf("audit outcome %s, want failed", recs[len(recs)-1].Result)
	}
}

// Reloads run one at a time; one asked during or after Stop is refused.
func TestReloadConcurrencyAndStop(t *testing.T) {
	h := newReloadHarness(t, "", baseModules, map[string]string{"alpha/10.yaml": alphaV1})
	var wg sync.WaitGroup
	errs := make(chan *api.Error, 4)
	for i := range 4 {
		wg.Go(func() {
			h.write(t, map[string]string{"alpha/10.yaml": fmt.Sprintf("version: 1\nsettings:\n  x: %d\n", 10+i)})
			_, err := h.reload(t)
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent reload: %v", err)
		}
	}
	if running := h.d.Snapshot().Modules["alpha"]; running != ModuleRunning {
		t.Errorf("alpha is %s", running)
	}
	if err := h.d.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, apiErr := h.reload(t); apiErr == nil || apiErr.Code != api.CodeUnavailable {
		t.Errorf("reload after Stop: %+v", apiErr)
	}
}

// A reload has one time budget whoever asks it (twice
// daemon.shutdown_timeout): a SIGHUP, whose context never ends, does not
// wait for module after module without limit.
func TestReloadHasATimeBudget(t *testing.T) {
	h := newReloadHarness(t, "", baseModules, map[string]string{"alpha/10.yaml": alphaV1})
	h.write(t, map[string]string{"alpha/10.yaml": alphaV2})
	before := time.Now()
	if _, apiErr := h.reload(t); apiErr != nil {
		t.Fatal(apiErr)
	}
	after := time.Now()
	budget := 2 * h.cfg.Daemon.ShutdownTimeout.Std()
	deadline := h.instance("alpha", 1).startDeadline
	if deadline.Before(before.Add(budget)) || deadline.After(after.Add(budget)) {
		t.Errorf("restarted module's deadline %v, want %v after the reload began", deadline, budget)
	}
}

// A replaced dispatcher returns at once, not at Stop; concurrent reloads
// that replace it, then Stop, leave no goroutine behind (synctest fails
// the test otherwise). The bubble's clock moves only while every
// goroutine waits, so a reload that waited for the old dispatcher shows.
func TestReloadLeavesNoGoroutine(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newReloadHarness(t, "", baseModules, map[string]string{"alpha/10.yaml": alphaV1})
		h.rewriteCentral(t, "", "https://hooks.example.com/2", baseModules)
		before := time.Now()
		if _, apiErr := h.reload(t); apiErr != nil {
			t.Fatal(apiErr)
		}
		if waited := time.Since(before); waited > 0 || strings.Contains(h.logs.String(), "did not return in time") {
			t.Errorf("the reload waited %v for the replaced dispatcher:\n%s", waited, h.logs.String())
		}
		var wg sync.WaitGroup
		for i := range 3 {
			wg.Go(func() {
				h.mu.Lock() // the files are shared: one writer at a time
				h.rewriteCentral(t, "", fmt.Sprintf("https://hooks.example.com/%d", 3+i), baseModules)
				h.write(t, map[string]string{"alpha/10.yaml": fmt.Sprintf("version: 1\nsettings:\n  x: %d\n", 10+i)})
				h.mu.Unlock()
				if _, apiErr := h.reload(t); apiErr != nil {
					t.Errorf("reload: %v", apiErr)
				}
			})
		}
		wg.Wait()
		if got := h.d.Snapshot().Modules["alpha"]; got != ModuleRunning {
			t.Errorf("alpha is %s", got)
		}
		if err := h.d.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
}

// Over the socket, core.reload needs the operate tier, is audited at both
// levels (the request and the operation) and answers the result.
func TestReloadOverTheSocket(t *testing.T) {
	h := newReloadHarness(t, "", baseModules, map[string]string{"alpha/10.yaml": alphaV1})
	h.write(t, map[string]string{"alpha/10.yaml": alphaV2})
	resp := h.call(t, api.CoreReload.Name, "")
	if resp.Error != nil {
		t.Fatal(resp.Error)
	}
	var res api.ReloadResult
	if err := json.Unmarshal(resp.Result, &res); err != nil || !slices.Equal(res.Restarted, []string{"alpha"}) {
		t.Fatalf("result %+v %v", res, err)
	}
	recs, _, err := h.d.audit.Tail(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, r := range recs {
		actions = append(actions, r.Action+" "+string(r.Result))
	}
	want := []string{"core.reload intent", "core.config_reload intent", "core.config_reload ok", "core.reload ok"}
	if !slices.Equal(actions, want) {
		t.Errorf("audit %v, want %v", actions, want)
	}
	if recs[1].Actor.Kind != audit.ActorSocket || recs[1].Actor.UID == nil || *recs[1].Actor.UID != 0 {
		t.Errorf("operation actor %+v", recs[1].Actor)
	}
}

// A module that registers its event types in Start can be restarted.
func TestReloadRestartsAModuleThatRegistersEvents(t *testing.T) {
	var regErrs []error
	var mu sync.Mutex
	h := newReloadHarnessWith(t, func(m *testModule) {
		m.onStart = func() {
			err := m.rt.Events.Register(events.TypeSpec{Type: "alpha_failed", Severity: model.SeverityError})
			mu.Lock()
			regErrs = append(regErrs, err)
			mu.Unlock()
		}
	}, "", "modules:\n  alpha: {enabled: true}\n", map[string]string{"alpha/10.yaml": alphaV1})
	h.write(t, map[string]string{"alpha/10.yaml": alphaV2})
	if res, apiErr := h.reload(t); apiErr != nil || len(res.Failed) != 0 {
		t.Fatalf("reload %+v %v", res, apiErr)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, err := range regErrs {
		if err != nil {
			t.Errorf("registration on restart: %v", err)
		}
	}
}

// The audit chain broken: a reload (SIGHUP) is refused before anything
// changes (D-077).
func TestReloadRefusedWhenTheAuditChainIsBroken(t *testing.T) {
	h := newReloadHarness(t, "", baseModules, map[string]string{"alpha/10.yaml": alphaV1})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// A lost outcome breaks the log (audit.Log.Record).
	if _, err := h.d.audit.Record(ctx, audit.Entry{Actor: audit.Actor{Kind: audit.ActorDaemon}, Action: "core.x", Result: audit.ResultOK}); err == nil {
		t.Fatal("recorded with an ended context")
	}
	h.write(t, map[string]string{"alpha/10.yaml": alphaV2})
	if _, apiErr := h.reload(t); apiErr == nil || apiErr.Code != api.CodeUnavailable || !strings.Contains(apiErr.Message, "broken") {
		t.Fatalf("reload: %+v", apiErr)
	}
	if slices.Contains(h.rec.list(), "stop alpha") {
		t.Error("a module was stopped")
	}
}

// Design review 2026-10-16 Q2: the reload's budget is measured on the
// daemon's clock, like the module calls it bounds. With a clock whose
// epoch is far from wall time, the old arithmetic gave every module call
// no time at all, and the changed module was abandoned.
func TestReloadBudgetUsesTheDaemonClock(t *testing.T) {
	h := newHarness(t, &testModule{name: "alpha"})
	fake := clock.NewFake(time.Date(2040, 1, 1, 0, 0, 0, 0, time.UTC))
	h.d.clock, h.d.opts.Clock = fake, fake
	h.cfg.Modules = []config.ModuleConfig{{Name: "alpha", Enabled: true}}
	next := *h.cfg
	next.Modules = []config.ModuleConfig{{Name: "alpha", Enabled: true, Dir: "/etc/sentinel/alpha"}}
	again := &testModule{name: "alpha", rec: h.rec}
	h.d.opts.Load = func() (*config.Config, []module.Instance, error) {
		return &next, []module.Instance{{Name: "alpha", Module: again}}, nil
	}
	if err := h.d.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.d.Stop(context.Background()) })
	res, apiErr := h.d.Reload(context.Background(), audit.Actor{Kind: audit.ActorSignal})
	if apiErr != nil || len(res.Failed) != 0 || !slices.Contains(res.Restarted, "alpha") {
		t.Fatalf("reload: %+v, %+v", res, apiErr)
	}
}

// Review of the design-review fixes R1: a caller's deadline shorter than
// the reload budget still bounds the module calls, which may ignore the
// cancellation. The module call limit here is shutdown_timeout (2 s): a
// reload that ignored the caller's 300 ms would last at least that long.
// The bound below leaves the rest of the reload (audit record with fsync,
// cleanup) room on a slow runner.
func TestReloadPreservesCallerDeadline(t *testing.T) {
	h := newReloadHarness(t, "", baseModules, map[string]string{"alpha/10.yaml": alphaV1})
	entered := make(chan struct{})
	release := make(chan struct{})
	h.mu.Lock()
	h.tweak = func(m *testModule) {
		if m.name == "alpha" {
			m.onStart = func() { close(entered) }
			m.startBlock = release
		}
	}
	h.mu.Unlock()
	h.write(t, map[string]string{"alpha/10.yaml": alphaV2})
	began := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = h.d.Reload(ctx, audit.Actor{Kind: audit.ActorSignal})
	}()
	defer func() {
		close(release)
		<-done
	}()
	select {
	case <-entered:
	case <-done:
		t.Fatal("reload returned before the blocked Start")
	case <-ctx.Done():
		t.Fatal("Start was not reached before the caller's deadline")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second): // far beyond the bug: the module's 2 s
	}
	if took := time.Since(began); took >= 1700*time.Millisecond {
		t.Errorf("the reload took %s: a module call outlasted the caller's 300ms deadline", took)
	}
}
