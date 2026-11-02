package supervisor

import (
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// Another boot started every unit again: no restart of this one. An
// unknown boot compares as the same one, the cautious side, and never
// replaces the stored boot (review 3c-2 C11).
func TestJudgeUnitBoot(t *testing.T) {
	const other = "11111111-2222-3333-4444-555555555555"
	t0 := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	base := unitBase{Boot: testBoot, Invocation: strings.Repeat("a", 32), NRestarts: 1}
	tests := []struct {
		name     string
		boot     string
		restarts uint64
		wantBoot string
	}{
		{"same boot", testBoot, 1, testBoot},
		{"another boot", other, 0, other},
		{"unknown boot", "", 1, testBoot},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, _, b, n := judgeUnit(unitMemo{}, base, runningUnit('b', 0), t0, time.Minute, tt.boot)
			if n != tt.restarts || (n > 0) != (v.failure == FailureUnitRestarted) {
				t.Errorf("restarts %d, verdict %+v; want %d", n, v, tt.restarts)
			}
			if b.Boot != tt.wantBoot || b.Invocation != strings.Repeat("b", 32) || b.NRestarts != 0 {
				t.Errorf("baseline %+v", b)
			}
		})
	}
}

// The baseline is persisted: a restart by systemd while sentineld was
// down is seen at the next start, and counted once (D-095).
func TestUnitBaselinePersisted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fu := &fakeUnits{}
		files := map[string]string{"supervisor/a.yaml": unitService}
		in := configureUnits(t, files, fu)
		r := start(t, in, "")
		synctest.Wait()
		r.stop(t)
		if f := readFileState(t, r.stateDir).Services["web"]; f.UnitInvocation != strings.Repeat("a", 32) || f.UnitBoot != testBoot {
			t.Fatalf("baseline not saved: %+v", f)
		}
		fu.state(runningUnit('b', 2), nil) // restarted twice while sentineld was down
		in2 := configureUnits(t, map[string]string{"supervisor/a.yaml": unitService}, fu)
		r2 := start(t, in2, r.stateDir)
		synctest.Wait()
		if s := statusOf(t, in2, "web"); s.UnitRestarts != 2 || s.LastFailure != string(FailureUnitRestarted) {
			t.Fatalf("status %+v", s)
		}
		r2.stop(t)
		in3 := configureUnits(t, map[string]string{"supervisor/a.yaml": unitService}, fu)
		r3 := start(t, in3, r.stateDir)
		defer r3.stop(t)
		synctest.Wait()
		if s := statusOf(t, in3, "web"); s.UnitRestarts != 2 || s.ConsecutiveFailures != 0 {
			t.Fatalf("the same restarts were counted again: %+v", s)
		}
	})
}

// systemd's own restarts are counted apart from sentineld's attempts and
// reported once per window when they reach max_attempts, also for a
// service that is only observed (D-095, Q4).
func TestUnitFlapping(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fu := &fakeUnits{}
		body := unitService + "    recovery: {max_attempts: 3, window: 2m}\n"
		in := configureUnits(t, map[string]string{"supervisor/a.yaml": body}, fu)
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		flaps := func() int {
			n := 0
			for _, e := range r.drain() {
				if e.Type == EventServiceFlapping {
					n++
					if e.Attributes["unit"] != "nginx.service" || e.Attributes["restarts"] != 3 || e.Attributes["window_seconds"] != int64(120) {
						t.Errorf("event %+v", e)
					}
				}
			}
			return n
		}
		for i, inv := range []byte("bcd") { // one restart per interval: the third reaches the threshold
			fu.state(runningUnit(inv, uint32(i+1)), nil)
			tick(10 * time.Second)
		}
		if n := flaps(); n != 1 {
			t.Fatalf("%d service_flapping, want 1", n)
		}
		fu.state(runningUnit('e', 4), nil)
		tick(10 * time.Second)
		if n := flaps(); n != 0 {
			t.Fatalf("%d service_flapping within the same window, want 0", n)
		}
		if s := statusOf(t, in, "web"); s.UnitRestarts != 4 || s.Restarts != 0 || s.Attempts != 0 {
			t.Errorf("systemd's restarts mixed with sentineld's: %+v", s)
		}
		tick(2 * time.Minute) // the window ends
		for i, inv := range []byte("fgh") {
			fu.state(runningUnit(inv, uint32(i+5)), nil)
			tick(10 * time.Second)
		}
		if n := flaps(); n != 1 {
			t.Fatalf("%d service_flapping in the next window, want 1", n)
		}
	})
}

// Many restarts at once keep at most the threshold stored, so the file
// always validates (review 3c-2 C1).
func TestFlappingBounded(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	r := Recovery{MaxAttempts: 5, Window: 10 * time.Minute}
	var times []time.Time
	var at time.Time
	emitted := 0
	for i := range 40 {
		var due bool
		times, at, due = flapping(times, at, 50, now.Add(time.Duration(i)*time.Second), r)
		if due {
			emitted++
		}
		if len(times) > r.MaxAttempts || !validAttempts(times) {
			t.Fatalf("round %d: %d times", i, len(times))
		}
	}
	if emitted != 1 {
		t.Errorf("emitted %d, want 1", emitted)
	}
	// The clock going back clamps the stored times; a lower threshold
	// trims them.
	times, _, _ = flapping(times, at, 1, now.Add(-time.Hour), Recovery{MaxAttempts: 2, Window: 10 * time.Minute})
	if len(times) != 2 || !validAttempts(times) {
		t.Errorf("times %v", times)
	}
}

func TestStateValidatesBaseline(t *testing.T) {
	inv := strings.Repeat("a", 32)
	ok := serviceState{Digest: "sha256:" + strings.Repeat("0", 64), Health: HealthHealthy,
		UnitBoot: testBoot, UnitInvocation: inv, UnitNRestarts: 3}
	tests := []struct {
		name  string
		edit  func(*serviceState)
		valid bool
	}{
		{"valid", func(*serviceState) {}, true},
		{"no baseline", func(s *serviceState) { s.UnitBoot, s.UnitInvocation, s.UnitNRestarts = "", "", 0 }, true},
		{"unknown boot", func(s *serviceState) { s.UnitBoot = "" }, true},
		{"bad boot", func(s *serviceState) { s.UnitBoot = "x" }, false},
		{"bad invocation", func(s *serviceState) { s.UnitInvocation = "A" + inv[1:] }, false},
		{"boot without invocation", func(s *serviceState) { s.UnitInvocation, s.UnitNRestarts = "", 0 }, false},
		{"count without invocation", func(s *serviceState) { s.UnitBoot, s.UnitInvocation = "", "" }, false},
		{"restart times out of order", func(s *serviceState) {
			now := time.Now()
			s.UnitRestartTimes = []time.Time{now, now.Add(-time.Second)}
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := ok
			tt.edit(&st)
			f := fileState{Services: map[string]serviceState{"web": st}}
			if err := f.Validate(); (err == nil) != tt.valid {
				t.Errorf("Validate = %v, want valid %v", err, tt.valid)
			}
		})
	}
}
