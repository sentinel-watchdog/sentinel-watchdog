package supervisor

import (
	"context"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/module"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/platform/systemd"
)

// aliasUnits answers each unit by its name: every name reads the same
// unit (Id nginx.service), failed; a name in hold waits until released.
type aliasUnits struct {
	mu       sync.Mutex
	hold     map[string]chan struct{}
	restarts int
}

func (a *aliasUnits) Unit(ctx context.Context, n systemd.UnitName) (systemd.UnitState, error) {
	a.mu.Lock()
	wait := a.hold[n.String()]
	a.mu.Unlock()
	if wait != nil {
		select {
		case <-wait:
		case <-ctx.Done():
			return systemd.UnitState{}, ctx.Err()
		}
	}
	return dead, nil
}

func (a *aliasUnits) Restart(context.Context, systemd.UnitName) (uint32, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.restarts++
	return 9, nil
}

func (a *aliasUnits) Close() error { return nil }

func (a *aliasUnits) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.restarts
}

const aliasServices = "version: 1\nservices:\n" +
	"  - name: a\n    unit: a.service\n    check_interval: 10s\n    failure_policy: {consecutive_failures: 1}\n    recovery: {delay: 1s}\n" +
	"  - name: b\n    unit: b.service\n    check_interval: 10s\n    failure_policy: {consecutive_failures: 1}\n    recovery: {delay: 1s}\n"

func configureAlias(t *testing.T, body string, au *aliasUnits) *instance {
	t.Helper()
	mc, err := moduleConfig(t, map[string]string{"sentinel.yaml": armedUnits, "supervisor/a.yaml": body}, nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(Options{NewUnitReader: func() unitReader { return au },
		BootID: func() (string, error) { return testBoot, nil }}).Configure(mc)
	if err != nil {
		t.Fatal(err)
	}
	return c.(*instance)
}

// Two names of one unit: both decide nothing and neither restarts
// (D-095, Q2); a restart waits for the other service's first read, so a
// slow alias cannot let the first one act (review 3c-2 C4).
func TestUnitAlias(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		au := &aliasUnits{hold: map[string]chan struct{}{"b.service": release}}
		in := configureAlias(t, aliasServices, au)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		tick(10 * time.Second) // a is failed, b has not read yet: a waits
		if n := au.count(); n != 0 {
			t.Fatalf("%d restarts before b was read", n)
		}
		if s := statusOf(t, in, "a"); s.BlockedReason != blockedAliasUnresolved {
			t.Errorf("status of a %+v", s)
		}
		close(release)
		for range 4 {
			tick(10 * time.Second)
		}
		if n := au.count(); n != 0 {
			t.Fatalf("%d restarts of an alias", n)
		}
		for _, name := range []string{"a", "b"} {
			if s := statusOf(t, in, name); s.Undecided != undecidedAlias || s.Phase != phaseUnavailable {
				t.Errorf("status of %s %+v", name, s)
			}
		}
		if logs := r.logs.String(); strings.Count(logs, "same systemd unit under two names") != 2 {
			t.Errorf("logs: %s", logs)
		}
	})
}

// Removing one of the two by a reload lets the other decide and restart.
func TestUnitAliasRemoved(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		au := &aliasUnits{}
		in := configureAlias(t, aliasServices, au)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		tick(10 * time.Second)
		one := strings.SplitAfter(aliasServices, "recovery: {delay: 1s}\n")[0]
		nx := configureAlias(t, one, au)
		if err := in.Reconfigure(context.Background(), nx, module.Runtime{}); err != nil {
			t.Fatal(err)
		}
		for range 3 {
			tick(10 * time.Second)
		}
		if n := au.count(); n == 0 {
			t.Error("the remaining service never restarted its unit")
		}
	})
}
