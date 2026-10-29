package supervisor

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/clock"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/state"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/platform/systemd"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/api"
)

// unitAt is a loaded service unit in state active/sub, invocation inv
// (one hex digit, repeated) and restart count n.
func unitAt(active systemd.ActiveState, sub string, inv byte, n uint32) systemd.UnitState {
	return systemd.UnitState{ID: "nginx.service", Load: systemd.LoadLoaded, Active: active, Sub: sub,
		InvocationID: strings.Repeat(string(inv), 32), NRestarts: n, Result: "success"}
}

var (
	runningUnit = func(inv byte, n uint32) systemd.UnitState { return unitAt(systemd.ActiveActive, "running", inv, n) }
	dead        = unitAt(systemd.ActiveFailed, "failed", 'a', 0)
)

func TestJudgeUnit(t *testing.T) {
	t0 := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	const grace = 90 * time.Second
	type obs struct {
		st   systemd.UnitState
		at   time.Duration // after t0
		want string        // a failure code, "neutral", or "" for a success
	}
	activating := func(sub string, inv byte, n uint32) systemd.UnitState {
		return unitAt(systemd.ActiveActivating, sub, inv, n)
	}
	withJob := runningUnit('a', 0)
	withJob.Job = true
	notFound := systemd.UnitState{ID: "nginx.service", Load: systemd.LoadNotFound, Active: systemd.ActiveInactive, Sub: "dead"}
	masked := notFound
	masked.Load = systemd.LoadMasked
	badSetting := notFound
	badSetting.Load = systemd.LoadBadSetting
	inactive := unitAt(systemd.ActiveInactive, "dead", 'a', 0)
	other := func(inv byte, n uint32) systemd.UnitState {
		st := runningUnit(inv, n)
		st.ID = "nginx-real.service"
		return st
	}
	loadedAs := func(l systemd.LoadState) systemd.UnitState {
		st := runningUnit('a', 0)
		st.Load = l
		return st
	}
	tests := []struct {
		name string
		seq  []obs
		// restarts is the systemd restarts counted at the end.
		restarts uint64
	}{
		{"active", []obs{{runningUnit('a', 0), 0, ""}, {runningUnit('a', 0), 30 * time.Second, ""}}, 0},
		{"failed", []obs{{dead, 0, string(FailureUnitFailed)}}, 0},
		{"inactive", []obs{{inactive, 0, string(FailureUnitInactive)}}, 0},
		{"not found", []obs{{notFound, 0, string(FailureUnitNotFound)}}, 0},
		{"masked", []obs{{masked, 0, string(FailureUnitMasked)}}, 0},
		{"bad setting", []obs{{badSetting, 0, string(FailureUnitLoadFailed)}}, 0},
		{"transition within grace, then stuck", []obs{
			{activating("start", 'a', 0), 0, "neutral"},
			{activating("start", 'a', 0), grace - time.Second, "neutral"},
			{activating("start", 'a', 0), grace, string(FailureUnitStuck)},
		}, 0},
		{"a changing substate does not renew the grace", []obs{
			{activating("start-pre", 'a', 0), 0, "neutral"},
			{activating("start", 'a', 0), 60 * time.Second, "neutral"},
			{unitAt(systemd.ActiveReloading, "reload", 'a', 0), grace, string(FailureUnitStuck)},
		}, 0},
		{"a job queued is a transition", []obs{{withJob, 0, "neutral"}}, 0},
		{"auto-restart is a transition", []obs{{activating("auto-restart", 'a', 1), 0, "neutral"}}, 0},
		{"settling ends the unsettled period", []obs{
			{activating("start", 'a', 0), 0, "neutral"},
			{runningUnit('a', 0), 60 * time.Second, ""},
			{activating("start", 'a', 0), 70 * time.Second, "neutral"},
			{activating("start", 'a', 0), 70*time.Second + grace - time.Second, "neutral"},
		}, 0},
		// Review 3c-1 C3: a restart seen only through a transition is not lost.
		{"restart across a transition", []obs{
			{runningUnit('a', 0), 0, ""},
			{activating("auto-restart", 'b', 1), 30 * time.Second, "neutral"},
			{runningUnit('b', 1), 60 * time.Second, string(FailureUnitRestarted)},
			{runningUnit('b', 1), 90 * time.Second, ""},
		}, 1},
		{"two restarts in one interval count two", []obs{
			{runningUnit('a', 0), 0, ""}, {runningUnit('c', 2), 30 * time.Second, string(FailureUnitRestarted)},
		}, 2},
		{"a new invocation without a counter is one restart", []obs{
			{runningUnit('a', 3), 0, ""}, {runningUnit('b', 3), 30 * time.Second, string(FailureUnitRestarted)},
		}, 1},
		{"a counter reset by a manual restart counts one", []obs{
			{runningUnit('a', 3), 0, ""}, {runningUnit('b', 0), 30 * time.Second, string(FailureUnitRestarted)},
		}, 1},
		{"a counter that goes down alone re-baselines", []obs{
			{runningUnit('a', 3), 0, ""}, {runningUnit('a', 0), 30 * time.Second, ""}, {runningUnit('a', 0), 60 * time.Second, ""},
		}, 0},
		// Review 3c-1 F2: the baseline survives failed observations (C3).
		{"a restart through a failure is seen", []obs{
			{runningUnit('a', 0), 0, ""}, {dead, 30 * time.Second, string(FailureUnitFailed)},
			{runningUnit('b', 2), 60 * time.Second, string(FailureUnitRestarted)},
			{runningUnit('b', 2), 90 * time.Second, ""},
		}, 2},
		{"a restart through an inactive unit is seen", []obs{
			{runningUnit('a', 0), 0, ""}, {inactive, 30 * time.Second, string(FailureUnitInactive)},
			{runningUnit('b', 0), 60 * time.Second, string(FailureUnitRestarted)},
		}, 1},
		{"a unit that crashes and is restarted every interval never succeeds", []obs{
			{runningUnit('a', 0), 0, ""},
			{dead, 30 * time.Second, string(FailureUnitFailed)},
			{runningUnit('b', 1), 60 * time.Second, string(FailureUnitRestarted)},
			{dead, 90 * time.Second, string(FailureUnitFailed)},
			{runningUnit('c', 2), 120 * time.Second, string(FailureUnitRestarted)},
		}, 2},
		{"another unit behind the name starts over", []obs{
			{runningUnit('a', 0), 0, ""},
			{other('b', 5), 30 * time.Second, ""},
			{other('b', 5), 60 * time.Second, ""},
		}, 0},
		{"deactivating is a transition", []obs{{unitAt(systemd.ActiveDeactivating, "stop", 'a', 0), 0, "neutral"}}, 0},
		{"refreshing is a transition", []obs{{unitAt(systemd.ActiveRefreshing, "refresh", 'a', 0), 0, "neutral"}}, 0},
		{"maintenance is a transition", []obs{{unitAt(systemd.ActiveMaintenance, "maintenance", 'a', 0), 0, "neutral"}}, 0},
		{"a stub unit is a transition", []obs{{loadedAs(systemd.LoadStub), 0, "neutral"}}, 0},
		{"a merged unit is a transition", []obs{{loadedAs(systemd.LoadMerged), 0, "neutral"}}, 0},
		{"zero grace fails a transition at once", nil, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := grace
			seq := tt.seq
			if seq == nil {
				g = 0
				seq = []obs{{activating("start", 'a', 0), 0, string(FailureUnitStuck)}}
			}
			var m unitMemo
			for i, o := range seq {
				var v verdict
				v, m = judgeUnit(m, o.st, t0.Add(o.at), g)
				got := string(v.failure)
				if v.undecided != "" {
					got = "neutral"
				}
				if got != o.want {
					t.Fatalf("observation %d (%s/%s): got %q, want %q", i, o.st.Active, o.st.Sub, got, o.want)
				}
			}
			if m.restarts != tt.restarts {
				t.Errorf("restarts = %d, want %d", m.restarts, tt.restarts)
			}
		})
	}
}

// fakeUnits is a programmable unitReader. read decides each answer; nil
// answers a running nginx.
type fakeUnits struct {
	mu     sync.Mutex
	read   func(ctx context.Context) (systemd.UnitState, error)
	calls  int
	closed bool
}

func (f *fakeUnits) Unit(ctx context.Context, n systemd.UnitName) (systemd.UnitState, error) {
	f.mu.Lock()
	f.calls++
	read := f.read
	f.mu.Unlock()
	if read == nil {
		return runningUnit('a', 0), nil
	}
	return read(ctx)
}

func (f *fakeUnits) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeUnits) set(read func(ctx context.Context) (systemd.UnitState, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.read = read
}

func (f *fakeUnits) state(st systemd.UnitState, err error) {
	f.set(func(context.Context) (systemd.UnitState, error) { return st, err })
}

func (f *fakeUnits) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// configureUnits configures the module over files with fu as its reader.
func configureUnits(t *testing.T, files map[string]string, fu *fakeUnits) *instance {
	t.Helper()
	mc, err := moduleConfig(t, files, nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(Options{NewUnitReader: func() unitReader { return fu }}).Configure(mc)
	if err != nil {
		t.Fatal(err)
	}
	return c.(*instance)
}

const unitService = "version: 1\nservices:\n  - name: web\n    unit: nginx.service\n    check_interval: 10s\n" +
	"    failure_policy: {consecutive_failures: 2, consecutive_successes: 1}\n    notifications: [ops]\n"

func TestUnitFailAndRecover(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fu := &fakeUnits{}
		in := configureUnits(t, map[string]string{"supervisor/a.yaml": unitService}, fu)
		r := start(t, in, "")
		synctest.Wait()
		if s := statusOf(t, in, "web"); s.State != "healthy" || s.Unit != "nginx.service" || s.ActiveState != "active" ||
			s.SubState != "running" || s.Endpoint != "" {
			t.Fatalf("status: %+v", s)
		}
		fu.state(dead, nil)
		tick(10 * time.Second)
		tick(10 * time.Second) // failure 2: failed
		evs := r.drain()
		if len(evs) != 1 || evs[0].Type != EventServiceFailed || evs[0].SourceType != "systemd" ||
			evs[0].Attributes["unit"] != "nginx.service" || evs[0].Attributes["active_state"] != "failed" ||
			evs[0].Attributes["sub_state"] != "failed" || evs[0].Attributes["failure"] != string(FailureUnitFailed) {
			t.Fatalf("failed events: %+v", evs)
		}
		if _, ok := evs[0].Attributes["endpoint"]; ok {
			t.Error("a systemd event carries an endpoint")
		}
		fu.state(runningUnit('b', 0), nil)
		tick(10 * time.Second) // started again: a restart, one more failed check (F2)
		if evs := r.drain(); len(evs) != 0 {
			t.Fatalf("events at the restart: %+v", evs)
		}
		tick(10 * time.Second) // the same invocation: a success, recovered
		evs = r.drain()
		if len(evs) != 1 || evs[0].Type != EventServiceRecovered || evs[0].Attributes["unit"] != "nginx.service" ||
			evs[0].Attributes["active_state"] != "active" {
			t.Fatalf("recovered events: %+v", evs)
		}
		r.stop(t)
		if !fu.closed {
			t.Error("Stop did not close the reader")
		}
	})
}

// Unavailable is neither a success nor a failure: it moves no streak and
// emits nothing (decision C4, R-007).
func TestUnitUnavailableIsNeutral(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fu := &fakeUnits{}
		fu.state(systemd.UnitState{}, systemd.ErrUnavailable)
		in := configureUnits(t, map[string]string{"supervisor/a.yaml": unitService}, fu)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		tick(10 * time.Second)
		s := statusOf(t, in, "web")
		if s.State != "unknown" || s.Phase != phaseUnavailable || s.Undecided != undecidedUnavailable || s.Checks != 2 ||
			s.Failures != 0 || len(r.drain()) != 0 {
			t.Fatalf("status: %+v", s)
		}
		fu.state(dead, nil)
		tick(10 * time.Second) // failure 1
		fu.state(systemd.UnitState{}, fmt.Errorf("read: %w", systemd.ErrChanging))
		tick(10 * time.Second) // neutral: the streak stays at 1
		if s := statusOf(t, in, "web"); s.ConsecutiveFailures != 1 || s.Undecided != undecidedChanging {
			t.Fatalf("after a neutral read: %+v", s)
		}
		fu.state(dead, nil)
		tick(10 * time.Second) // failure 2: failed
		if evs := r.drain(); len(evs) != 1 || evs[0].Type != EventServiceFailed {
			t.Fatalf("events: %+v", evs)
		}
		if s := statusOf(t, in, "web"); s.Phase != "" || s.Undecided != "" {
			t.Errorf("a decided read left %q/%q", s.Phase, s.Undecided)
		}
	})
}

// Review 3c-1 C2: a read that outlives its timeout, or never returns, is
// neutral, never a failure, and no second read starts beside it.
func TestUnitStalledRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fu := &fakeUnits{}
		release := make(chan struct{})
		fu.set(func(ctx context.Context) (systemd.UnitState, error) {
			<-release // ignores ctx: a stalled bus
			return systemd.UnitState{}, errors.New("late")
		})
		body := "version: 1\nservices:\n  - name: web\n    unit: nginx.service\n    check_interval: 10s\n    timeout: 1s\n" +
			"    failure_policy: {consecutive_failures: 1}\n"
		in := configureUnits(t, map[string]string{"supervisor/a.yaml": body}, fu)
		r := start(t, in, "")
		synctest.Wait()
		for range 3 {
			tick(10 * time.Second)
		}
		s := statusOf(t, in, "web")
		if s.Failures != 0 || s.Phase != phaseUnavailable || !s.Stalled || len(r.drain()) != 0 || fu.count() != 1 {
			t.Fatalf("status %+v, reads %d", s, fu.count())
		}
		close(release)
		fu.set(nil)
		tick(10 * time.Second) // the stalled read returned: a new one
		tick(10 * time.Second)
		if s := statusOf(t, in, "web"); s.State != "healthy" || s.Stalled {
			t.Fatalf("after the stall: %+v", s)
		}
		r.stop(t)
	})
}

func TestUnitReadHonoursTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fu := &fakeUnits{}
		fu.set(func(ctx context.Context) (systemd.UnitState, error) {
			<-ctx.Done() // a bus that never answers; the read gives up with its context
			return systemd.UnitState{}, fmt.Errorf("%w: %w", systemd.ErrUnavailable, ctx.Err())
		})
		in := configureUnits(t, map[string]string{"supervisor/a.yaml": unitService}, fu)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		tick(10 * time.Second)
		if s := statusOf(t, in, "web"); s.Failures != 0 || s.Phase != phaseUnavailable || len(r.drain()) != 0 {
			t.Fatalf("status %+v", s)
		}
	})
}

// A restart by systemd between two checks is one failed observation and
// is counted (decision Q3).
func TestUnitRestartedBySystemd(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fu := &fakeUnits{}
		body := strings.Replace(unitService, "consecutive_failures: 2", "consecutive_failures: 1", 1)
		in := configureUnits(t, map[string]string{"supervisor/a.yaml": body}, fu)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		fu.state(runningUnit('b', 1), nil)
		tick(10 * time.Second)
		evs := r.drain()
		if len(evs) != 1 || evs[0].Attributes["failure"] != string(FailureUnitRestarted) {
			t.Fatalf("events %+v", evs)
		}
		if s := statusOf(t, in, "web"); s.UnitRestarts != 1 {
			t.Errorf("status %+v", s)
		}
		tick(10 * time.Second) // the same invocation again: a success
		if evs := r.drain(); len(evs) != 1 || evs[0].Type != EventServiceRecovered {
			t.Fatalf("events %+v", evs)
		}
	})
}

// Review 3c-1 C7, F6: changing the interval replaces the loop but keeps
// the state, the open incident and what was observed of the unit: the
// restart baseline (a restart after the reload is still seen) and the
// unsettled period (a reload does not renew transition_grace).
func TestUnitReloadKeepsState(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fu := &fakeUnits{}
		files := map[string]string{"supervisor/a.yaml": unitService}
		in := configureUnits(t, files, fu)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait() // baseline: invocation a, no restart
		fu.state(dead, nil)
		tick(10 * time.Second)
		tick(10 * time.Second)
		incident := statusOf(t, in, "web").IncidentID
		if incident == "" {
			t.Fatal("no incident")
		}
		files["supervisor/a.yaml"] = strings.Replace(unitService, "check_interval: 10s", "check_interval: 20s", 1)
		next := configureUnits(t, files, &fakeUnits{})
		fu.state(runningUnit('b', 1), nil) // systemd restarted it: seen against the kept baseline
		if err := in.Reconfigure(context.Background(), next, runtimeFor(r, r.dir)); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		s := statusOf(t, in, "web")
		if s.IncidentID != incident || s.State != "failed" || s.CheckInterval != "20s" || s.UnitRestarts != 1 ||
			s.LastFailure != string(FailureUnitRestarted) {
			t.Fatalf("after reload: %+v", s)
		}
		tick(20 * time.Second)
		if s := statusOf(t, in, "web"); s.State != "healthy" {
			t.Fatalf("after recovery: %+v", s)
		}
	})
}

func TestUnitReloadKeepsTransition(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fu := &fakeUnits{}
		fu.state(unitAt(systemd.ActiveActivating, "start", 'a', 0), nil)
		body := "version: 1\nservices:\n  - name: web\n    unit: nginx.service\n    check_interval: 10s\n" +
			"    transition_grace: 30s\n    failure_policy: {consecutive_failures: 1}\n"
		files := map[string]string{"supervisor/a.yaml": body}
		in := configureUnits(t, files, fu)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait() // t=0: unsettled from here
		tick(10 * time.Second)
		tick(10 * time.Second) // t=20
		files["supervisor/a.yaml"] = strings.Replace(body, "check_interval: 10s", "check_interval: 15s", 1)
		if err := in.Reconfigure(context.Background(), configureUnits(t, files, &fakeUnits{}), runtimeFor(r, r.dir)); err != nil {
			t.Fatal(err)
		}
		synctest.Wait() // the new loop reads at once: t=20, still within the grace
		if s := statusOf(t, in, "web"); s.Phase != phaseTransition {
			t.Fatalf("after reload: %+v", s)
		}
		tick(15 * time.Second) // t=35: unsettled for 35 s, not 15
		if s := statusOf(t, in, "web"); s.State != "failed" || s.LastFailure != string(FailureUnitStuck) {
			t.Fatalf("the reload renewed the grace: %+v", s)
		}
	})
}

func TestUnitCommands(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fu := &fakeUnits{}
		in := configureUnits(t, map[string]string{"supervisor/a.yaml": unitService}, fu)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		for _, a := range []action{actStart, actStop, actRestart, actReset} {
			_, apiErr := in.act(context.Background(), "web", a)
			if apiErr == nil || apiErr.Code != api.CodeConflict || !strings.Contains(apiErr.Message, "systemctl") {
				t.Errorf("%s: %+v", a, apiErr)
			}
		}
		if _, apiErr := in.act(context.Background(), "web", actDisable); apiErr != nil {
			t.Fatal(apiErr)
		}
		synctest.Wait()
		before := fu.count()
		tick(30 * time.Second)
		if fu.count() != before || statusOf(t, in, "web").Phase != phaseDisabled {
			t.Fatal("a disabled unit is still read")
		}
		if _, apiErr := in.act(context.Background(), "web", actEnable); apiErr != nil {
			t.Fatal(apiErr)
		}
		synctest.Wait()
		if fu.count() == before {
			t.Error("enable did not resume the reads")
		}
	})
}

// Review 3c-1 F4: a reply that cannot be read decides nothing (an unknown
// state never fails a unit, so it can never drive a restart), shows why,
// and is logged once when it starts and once when it ends.
func TestUnitInvalidReplyIsNeutralAndLogged(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fu := &fakeUnits{}
		fu.state(systemd.UnitState{}, fmt.Errorf("%w: property ActiveState: unknown value", systemd.ErrInvalid))
		in := configureUnits(t, map[string]string{"supervisor/a.yaml": unitService}, fu)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		tick(10 * time.Second)
		tick(10 * time.Second)
		s := statusOf(t, in, "web")
		if s.Failures != 0 || s.Phase != phaseUnavailable || s.Undecided != undecidedInvalid || len(r.drain()) != 0 {
			t.Fatalf("status %+v", s)
		}
		if n := strings.Count(r.logs.String(), "cannot read a systemd unit"); n != 1 {
			t.Errorf("logged %d times, want once per streak:\n%s", n, r.logs.String())
		}
		if strings.Contains(r.logs.String(), "ActiveState") {
			t.Error("the log carries the error text")
		}
		fu.set(nil)
		tick(10 * time.Second)
		if !strings.Contains(r.logs.String(), "reads the systemd unit again") {
			t.Errorf("no log at the end of the streak:\n%s", r.logs.String())
		}
	})
}

// Review 3c-1 C6, F8: a Start that its context ended closes the reader;
// a reload that adds the first systemd service reads it with the
// instance's reader.
func TestUnitReaderLifecycle(t *testing.T) {
	t.Run("canceled start", func(t *testing.T) {
		fu := &fakeUnits{}
		in := configureUnits(t, map[string]string{"supervisor/a.yaml": unitService}, fu)
		dir, err := state.OpenDir(filepath.Join(t.TempDir(), "state"), clock.Real())
		if err != nil {
			t.Fatal(err)
		}
		defer dir.Close()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := in.Start(ctx, runtimeFor(&rig{logs: &syncBuffer{}}, dir)); err == nil {
			t.Fatal("Start succeeded with a canceled context")
		}
		if !fu.closed || fu.count() != 0 {
			t.Errorf("closed %v, reads %d", fu.closed, fu.count())
		}
	})
	t.Run("first systemd service added by a reload", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			fu := &fakeUnits{}
			files := map[string]string{"supervisor/a.yaml": "version: 1\nservices:\n  - name: web\n    url: https://web.example.org/\n"}
			mc, err := moduleConfig(t, files, nil)
			if err != nil {
				t.Fatal(err)
			}
			c, err := New(Options{NewChecker: newFakes().build, NewUnitReader: func() unitReader { return fu }}).Configure(mc)
			if err != nil {
				t.Fatal(err)
			}
			in := c.(*instance)
			r := start(t, in, "")
			defer r.stop(t)
			synctest.Wait()
			files["supervisor/a.yaml"] += "  - name: proxy\n    unit: nginx.service\n"
			next, err := moduleConfig(t, files, nil)
			if err != nil {
				t.Fatal(err)
			}
			nc, err := New(Options{NewChecker: newFakes().build, NewUnitReader: func() unitReader {
				t.Error("a reload built a reader")
				return &fakeUnits{}
			}}).Configure(next)
			if err != nil {
				t.Fatal(err)
			}
			if err := in.Reconfigure(context.Background(), nc, runtimeFor(r, r.dir)); err != nil {
				t.Fatal(err)
			}
			synctest.Wait()
			if s := statusOf(t, in, "proxy"); s.State != "healthy" || fu.count() == 0 {
				t.Fatalf("status %+v, reads %d", s, fu.count())
			}
		})
	})
}

// Review 3c-1 C5, F10: status read while observations are recorded, from
// other goroutines, agrees with itself (run with -race).
func TestUnitStatusDuringTransitions(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fu := &fakeUnits{}
		var flip atomic.Int64
		fu.set(func(context.Context) (systemd.UnitState, error) {
			switch flip.Add(1) % 3 {
			case 0:
				return dead, nil
			case 1:
				return systemd.UnitState{}, systemd.ErrUnavailable
			}
			return runningUnit('a', 0), nil
		})
		in := configureUnits(t, map[string]string{"supervisor/a.yaml": unitService}, fu)
		r := start(t, in, "")
		defer r.stop(t)
		var wg sync.WaitGroup
		stop := make(chan struct{})
		for range 4 {
			wg.Go(func() {
				for {
					st, apiErr := in.status("web")
					if apiErr != nil {
						t.Error(apiErr)
						return
					}
					if s := st.Services[0]; (s.Phase == phaseUnavailable) != (s.Undecided != "") {
						t.Errorf("phase %q with undecided %q", s.Phase, s.Undecided)
						return
					}
					select {
					case <-stop:
						return
					case <-time.After(time.Second):
					}
				}
			})
		}
		for range 9 {
			tick(10 * time.Second)
		}
		close(stop)
		wg.Wait()
		if fu.count() < 9 {
			t.Errorf("reads %d", fu.count())
		}
	})
}
