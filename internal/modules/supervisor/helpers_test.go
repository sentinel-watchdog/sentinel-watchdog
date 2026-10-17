package supervisor

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/clock"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/config"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/events"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/module"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/state"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/model"
)

// TestMain runs the tests with umask 022: they write configuration and
// state under t.TempDir(), which the checks refuse when group-writable.
func TestMain(m *testing.M) {
	syscall.Umask(0o022)
	m.Run()
}

// writeTree writes files under a fresh directory and returns it.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// centralFile enables the supervisor and declares the channels ops and
// pager; extra goes under modules.supervisor.
const centralFile = "version: 1\nnotifications:\n  channels:\n" +
	"    - {name: ops, type: webhook, url: \"https://hooks.example.com/1\"}\n" +
	"    - {name: pager, type: webhook, url: \"https://hooks.example.com/2\"}\n" +
	"modules:\n  supervisor:\n    enabled: true\n"

// moduleConfig loads files (paths relative to the configuration
// directory; "sentinel.yaml" defaults to centralFile) through config.Load,
// the only way to build Sections, and returns the supervisor's part.
func moduleConfig(t *testing.T, files map[string]string, env map[string]string) (config.ModuleConfig, error) {
	t.Helper()
	if _, ok := files["sentinel.yaml"]; !ok {
		files["sentinel.yaml"] = centralFile
	}
	dir := writeTree(t, files)
	cfg, err := config.Load(config.LoadOptions{MainFile: filepath.Join(dir, "sentinel.yaml"),
		Modules: map[string]config.ModuleAvailability{moduleName: config.ModuleAvailable},
		Lookup:  func(k string) (string, bool) { v, ok := env[k]; return v, ok }})
	if err != nil {
		return config.ModuleConfig{}, err
	}
	mc, ok := cfg.Module(moduleName)
	if !ok {
		t.Fatal("no supervisor in the configuration")
	}
	return mc, nil
}

// configure loads files and configures the module with checkers.
func configure(t *testing.T, files map[string]string, checkers func(Service) (Checker, error)) (*instance, error) {
	t.Helper()
	mc, err := moduleConfig(t, files, nil)
	if err != nil {
		return nil, err
	}
	c, err := New(Options{NewChecker: checkers}).Configure(mc)
	if err != nil {
		return nil, err
	}
	return c.(*instance), nil
}

// fake is a programmable checker. check decides each result; nil is a
// success.
type fake struct {
	mu    sync.Mutex
	check func(ctx context.Context) Result
	calls int
}

func (f *fake) Check(ctx context.Context) Result {
	f.mu.Lock()
	f.calls++
	check := f.check
	f.mu.Unlock()
	if check == nil {
		return Result{}
	}
	return check(ctx)
}

func (f *fake) set(check func(ctx context.Context) Result) {
	f.mu.Lock()
	f.check = check
	f.mu.Unlock()
}

func (f *fake) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func failing(context.Context) Result { return Result{Failure: FailureCheck} }

// fakes builds one fake per service, by name.
type fakes struct {
	mu  sync.Mutex
	all map[string]*fake
}

func newFakes() *fakes { return &fakes{all: map[string]*fake{}} }

func (fs *fakes) build(s Service) (Checker, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	f, ok := fs.all[s.Name]
	if !ok {
		f = &fake{}
		fs.all[s.Name] = f
	}
	return f, nil
}

func (fs *fakes) get(name string) *fake {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.all[name]
}

// rig runs a configured instance with a real bus and state directory.
type rig struct {
	in       *instance
	stateDir string
	bus      *events.Bus
	sub      *events.Subscription
	logs     *syncBuffer
}

// start starts in over stateDir ("" for a fresh one).
func start(t *testing.T, in *instance, stateDir string) *rig {
	t.Helper()
	if stateDir == "" {
		stateDir = filepath.Join(t.TempDir(), "state")
	}
	dir, err := state.OpenDir(stateDir, clock.Real())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dir.Close() })
	r := &rig{in: in, stateDir: stateDir, logs: &syncBuffer{}}
	rt := runtimeFor(r, dir)
	if err := in.Start(context.Background(), rt); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return r
}

// runtimeFor is the runtime of r's instance over dir, on the real clock
// (the fake time of a synctest bubble when run in one).
func runtimeFor(r *rig, dir *state.Dir) module.Runtime {
	if r.bus == nil {
		r.bus = events.NewBus(events.NewRegistry(), events.Options{Hostname: "web1", Version: "test"})
		r.sub = r.bus.Subscribe(64)
	}
	return module.Runtime{Logger: slog.New(slog.NewTextHandler(r.logs, nil)), Clock: clock.Real(),
		Events: r.bus.Emitter(moduleName),
		State:  func(v int) (*state.Store, error) { return dir.Store(moduleName, v) }}
}

// stop stops the instance; it must stop cleanly.
func (r *rig) stop(t *testing.T) {
	t.Helper()
	if err := r.in.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// drain returns the events published so far.
func (r *rig) drain() []model.Event {
	var out []model.Event
	for {
		select {
		case e := <-r.sub.Events():
			out = append(out, e)
		default:
			return out
		}
	}
}

// syncBuffer is a bytes.Buffer safe for the logger's concurrent writes.
type syncBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

func eventFor(source, typ string) model.Event {
	return model.Event{Module: moduleName, Source: source, Type: model.EventType(typ)}
}
