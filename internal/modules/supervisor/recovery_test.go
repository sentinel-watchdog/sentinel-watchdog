package supervisor

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/config"
)

var (
	t0          = time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	defaultRecv = Recovery{MaxAttempts: 3, Window: 10 * time.Minute, Delay: time.Second,
		Backoff: config.BackoffExponential, MaxDelay: time.Minute, StableAfter: time.Minute, Cooldown: 10 * time.Minute}
)

func at(d time.Duration) time.Time { return t0.Add(d) }

func TestAdmit(t *testing.T) {
	tests := []struct {
		name string
		b    budget
		now  time.Time
		want admission
		left budget
	}{
		{"empty", budget{}, t0, admission{OK: true, Attempt: 1}, budget{}},
		{"two in window", budget{Attempts: []time.Time{at(-5 * time.Minute), at(-time.Minute)}}, t0,
			admission{OK: true, Attempt: 3}, budget{Attempts: []time.Time{at(-5 * time.Minute), at(-time.Minute)}}},
		{"exactly window ago is out", budget{Attempts: []time.Time{at(-10 * time.Minute), at(-time.Minute), at(-time.Second)}}, t0,
			admission{OK: true, Attempt: 3}, budget{Attempts: []time.Time{at(-time.Minute), at(-time.Second)}}},
		{"full window exhausts, without a recorded exhaustion (C3)",
			budget{Attempts: []time.Time{at(-3 * time.Minute), at(-2 * time.Minute), at(-time.Minute)}}, t0,
			admission{Exhausted: true, ResumeAt: at(10 * time.Minute)},
			budget{Attempts: []time.Time{at(-3 * time.Minute), at(-2 * time.Minute), at(-time.Minute)}, ExhaustedAt: t0}},
		{"more than max after a policy change",
			budget{Attempts: []time.Time{at(-4 * time.Minute), at(-3 * time.Minute), at(-2 * time.Minute), at(-time.Minute)}}, t0,
			admission{Exhausted: true, ResumeAt: at(10 * time.Minute)},
			budget{Attempts: []time.Time{at(-4 * time.Minute), at(-3 * time.Minute), at(-2 * time.Minute), at(-time.Minute)}, ExhaustedAt: t0}},
		{"exhausted, cooling down", budget{ExhaustedAt: at(-time.Minute)}, t0,
			admission{ResumeAt: at(9 * time.Minute)}, budget{ExhaustedAt: at(-time.Minute)}},
		{"cooldown over: renewed", budget{Attempts: []time.Time{at(-11 * time.Minute)}, ExhaustedAt: at(-10 * time.Minute)}, t0,
			admission{OK: true, Attempt: 1, Renewed: true}, budget{}},
		{"clock went back: future attempts count as now (C7)",
			budget{Attempts: []time.Time{at(-time.Minute), at(time.Hour), at(2 * time.Hour)}}, t0,
			admission{Exhausted: true, ResumeAt: at(10 * time.Minute)},
			budget{Attempts: []time.Time{at(-time.Minute), t0, t0}, ExhaustedAt: t0}},
		{"clock went back: a future exhaustion is clamped to now (C7)", budget{ExhaustedAt: at(time.Hour)}, t0,
			admission{ResumeAt: at(10 * time.Minute)}, budget{ExhaustedAt: t0}},
		{"clock went forward: old attempts expire", budget{Attempts: []time.Time{at(-2 * time.Hour)}}, t0,
			admission{OK: true, Attempt: 1}, budget{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := budget{Attempts: slices.Clone(tt.b.Attempts), ExhaustedAt: tt.b.ExhaustedAt}
			left, got := admit(tt.b, tt.now, defaultRecv)
			if got != tt.want {
				t.Errorf("admission = %+v, want %+v", got, tt.want)
			}
			if !slices.Equal(left.Attempts, tt.left.Attempts) || !left.ExhaustedAt.Equal(tt.left.ExhaustedAt) {
				t.Errorf("budget = %+v, want %+v", left, tt.left)
			}
			if !slices.Equal(in.Attempts, tt.b.Attempts) {
				t.Error("admit changed its input (C6)")
			}
		})
	}
}

// A clamped exhaustion keeps its deadline: evaluated again later, the
// cooldown ends cooldown after the first evaluation, not after each (C7).
func TestAdmitClampedExhaustionDoesNotMove(t *testing.T) {
	b, a := admit(budget{ExhaustedAt: at(time.Hour)}, t0, defaultRecv)
	if !a.ResumeAt.Equal(at(10 * time.Minute)) {
		t.Fatalf("resume at %v", a.ResumeAt)
	}
	if _, a = admit(b, at(5*time.Minute), defaultRecv); !a.ResumeAt.Equal(at(10 * time.Minute)) {
		t.Errorf("deadline moved to %v", a.ResumeAt)
	}
	if _, a = admit(b, at(10*time.Minute), defaultRecv); !a.OK || !a.Renewed {
		t.Errorf("not renewed at the deadline: %+v", a)
	}
}

func TestDelay(t *testing.T) {
	fixed := defaultRecv
	fixed.Backoff, fixed.Delay, fixed.MaxDelay = config.BackoffFixed, 5*time.Second, 5*time.Second
	long := defaultRecv
	long.MaxDelay = time.Hour
	tests := []struct {
		name string
		n    int
		r    Recovery
		want time.Duration
	}{
		{"exponential 1", 1, defaultRecv, time.Second},
		{"exponential 2", 2, defaultRecv, 2 * time.Second},
		{"exponential 5", 5, defaultRecv, 16 * time.Second},
		{"capped", 7, defaultRecv, time.Minute},
		{"huge n saturates", 1 << 30, long, time.Hour},
		{"fixed", 4, fixed, 5 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := delay(tt.n, tt.r); got != tt.want {
				t.Errorf("delay(%d) = %s, want %s", tt.n, got, tt.want)
			}
		})
	}
}

func TestReserveDoesNotShareItsInput(t *testing.T) {
	b := budget{Attempts: make([]time.Time, 1, 8)}
	b.Attempts[0] = at(-time.Minute)
	r1 := reserve(b, t0)
	r2 := reserve(b, at(time.Second))
	if !r1.Attempts[1].Equal(t0) || !r2.Attempts[1].Equal(at(time.Second)) || len(b.Attempts) != 1 {
		t.Errorf("reservations share a backing array: %v %v", r1.Attempts, r2.Attempts)
	}
}

// Review 3b-3 R2: a clock set back between admission and reservation
// stores the budget in a valid order, as read back after a restart.
func TestReservationAfterClockRollback(t *testing.T) {
	b, a := admit(budget{Attempts: []time.Time{t0}}, t0.Add(time.Second), defaultRecv)
	if !a.OK {
		t.Fatal("not admitted")
	}
	b = reserve(b, t0.Add(-time.Minute))
	data, err := json.Marshal(b.Attempts)
	if err != nil {
		t.Fatal(err)
	}
	var stored []time.Time
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	if !validAttempts(stored) {
		t.Fatalf("reservation stored an invalid order: %v", stored)
	}
}

// Review 3b-3 R2: the stored order is the wall clock's, which is all a
// state file keeps: a reading's monotonic part never decides it.
func TestReducerUsesWallClock(t *testing.T) {
	now := time.Now() // has a monotonic reading
	b := reserve(budget{}, now)
	if b.Attempts[0] != now.Round(0) {
		t.Errorf("stored a monotonic reading: %v", b.Attempts[0])
	}
	b, _ = admit(budget{Attempts: []time.Time{now, now, now}}, now, Recovery{MaxAttempts: 3, Window: time.Hour, Cooldown: time.Minute})
	if b.ExhaustedAt != now.Round(0) {
		t.Errorf("exhaustion with a monotonic reading: %v", b.ExhaustedAt)
	}
}
