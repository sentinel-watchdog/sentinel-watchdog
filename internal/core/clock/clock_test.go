package clock

import (
	"testing"
	"time"
)

var start = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

// fired reports whether t has delivered a value, and which one.
func fired(t Timer) (time.Time, bool) {
	select {
	case v := <-t.C():
		return v, true
	default:
		return time.Time{}, false
	}
}

func TestFakeNowAndAdvance(t *testing.T) {
	f := NewFake(start)
	if got := f.Now(); !got.Equal(start) {
		t.Fatalf("Now() = %v, want %v", got, start)
	}
	f.Advance(90 * time.Second)
	if got, want := f.Now(), start.Add(90*time.Second); !got.Equal(want) {
		t.Fatalf("Now() after Advance = %v, want %v", got, want)
	}
}

func TestFakeTimerFiresAtDeadline(t *testing.T) {
	tests := []struct {
		name      string
		timer     time.Duration
		advance   []time.Duration
		wantFired bool
	}{
		{name: "before deadline", timer: 10 * time.Second, advance: []time.Duration{9 * time.Second}},
		{name: "exactly at deadline", timer: 10 * time.Second, advance: []time.Duration{10 * time.Second}, wantFired: true},
		{name: "after deadline", timer: 10 * time.Second, advance: []time.Duration{time.Minute}, wantFired: true},
		{name: "in several steps", timer: 10 * time.Second, advance: []time.Duration{4 * time.Second, 6 * time.Second}, wantFired: true},
		{name: "zero duration", timer: 0, wantFired: true},
		{name: "negative duration", timer: -time.Second, wantFired: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := NewFake(start)
			timer := f.NewTimer(tt.timer)
			for _, d := range tt.advance {
				f.Advance(d)
			}
			_, ok := fired(timer)
			if ok != tt.wantFired {
				t.Fatalf("fired = %v, want %v", ok, tt.wantFired)
			}
		})
	}
}

func TestFakeTimerDeliversItsDeadline(t *testing.T) {
	f := NewFake(start)
	timer := f.NewTimer(10 * time.Second)
	f.Advance(time.Hour)
	got, ok := fired(timer)
	if !ok {
		t.Fatal("timer did not fire")
	}
	if want := start.Add(10 * time.Second); !got.Equal(want) {
		t.Errorf("delivered %v, want the deadline %v", got, want)
	}
}

func TestFakeTimerFiresOnce(t *testing.T) {
	f := NewFake(start)
	timer := f.NewTimer(time.Second)
	f.Advance(time.Second)
	if _, ok := fired(timer); !ok {
		t.Fatal("timer did not fire")
	}
	f.Advance(time.Hour)
	if _, ok := fired(timer); ok {
		t.Error("timer fired twice")
	}
}

func TestFakeTimerStop(t *testing.T) {
	f := NewFake(start)
	timer := f.NewTimer(time.Second)
	if f.PendingTimers() != 1 {
		t.Fatalf("PendingTimers() = %d, want 1", f.PendingTimers())
	}
	if !timer.Stop() {
		t.Error("first Stop() = false, want true")
	}
	if timer.Stop() {
		t.Error("second Stop() = true, want false")
	}
	f.Advance(time.Hour)
	if _, ok := fired(timer); ok {
		t.Error("stopped timer fired")
	}
	if f.PendingTimers() != 0 {
		t.Errorf("PendingTimers() = %d, want 0", f.PendingTimers())
	}
}

func TestFakeStopAfterFire(t *testing.T) {
	f := NewFake(start)
	timer := f.NewTimer(time.Second)
	f.Advance(time.Second)
	if timer.Stop() {
		t.Error("Stop() after firing = true, want false")
	}
}

// TestFakeAdvanceFiresAllDueTimers checks that one Advance fires every due
// timer, each with its own deadline. (Advance also sends in deadline order,
// but channel sends to different timers are not observable in order.)
func TestFakeAdvanceFiresAllDueTimers(t *testing.T) {
	f := NewFake(start)
	late := f.NewTimer(30 * time.Second)
	early := f.NewTimer(10 * time.Second)
	f.Advance(time.Minute)
	e, okE := fired(early)
	l, okL := fired(late)
	if !okE || !okL {
		t.Fatalf("fired early=%v late=%v, want both", okE, okL)
	}
	if !e.Before(l) {
		t.Errorf("early delivered %v, late delivered %v", e, l)
	}
}

func TestFakeAdvanceNegativePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("Advance(-1s) did not panic")
		}
	}()
	NewFake(start).Advance(-time.Second)
}

// TestFakeWithGoroutine shows the intended use: code under test waits on a
// timer in its own goroutine while the test moves time forward.
func TestFakeWithGoroutine(t *testing.T) {
	f := NewFake(start)
	done := make(chan time.Time)
	go func() {
		timer := f.NewTimer(5 * time.Minute)
		done <- <-timer.C()
	}()

	f.BlockUntilTimers(1) // the goroutine has created its timer
	f.Advance(5 * time.Minute)

	select {
	case got := <-done:
		if want := start.Add(5 * time.Minute); !got.Equal(want) {
			t.Errorf("goroutine woke at %v, want %v", got, want)
		}
	case <-time.After(5 * time.Second): // real-time safety net only
		t.Fatal("goroutine did not wake up")
	}
}

func TestRealClock(t *testing.T) {
	c := Real()
	before := time.Now()
	if now := c.Now(); now.Before(before) {
		t.Errorf("Now() = %v, before %v", now, before)
	}
	timer := c.NewTimer(time.Millisecond)
	select {
	case <-timer.C():
	case <-time.After(5 * time.Second):
		t.Fatal("real timer did not fire")
	}
	if timer.Stop() {
		t.Error("Stop() after firing = true, want false")
	}
}
