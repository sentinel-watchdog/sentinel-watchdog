package clock

import (
	"slices"
	"sync"
	"time"
)

// Fake is a Clock for tests. Time stands still until Advance is called;
// timers whose deadline has been reached then fire, earliest first.
//
// It is safe for concurrent use: a test can Advance the clock while the
// code under test waits on a timer in another goroutine. Use
// BlockUntilTimers to wait until that goroutine has created its timer.
type Fake struct {
	mu     sync.Mutex
	cond   *sync.Cond
	now    time.Time
	timers []*fakeTimer // active timers, in creation order
}

// NewFake returns a Fake clock set to start.
func NewFake(start time.Time) *Fake {
	f := &Fake{now: start}
	f.cond = sync.NewCond(&f.mu)
	return f
}

// Now returns the fake current time.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// NewTimer returns a timer that fires when the fake time reaches Now()+d.
// A zero or negative d fires immediately.
func (f *Fake) NewTimer(d time.Duration) Timer {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := &fakeTimer{clock: f, when: f.now.Add(d), c: make(chan time.Time, 1)}
	if d <= 0 {
		t.c <- f.now
		return t
	}
	f.timers = append(f.timers, t)
	f.cond.Broadcast()
	return t
}

// Advance moves the fake time forward by d and fires every timer whose
// deadline is not after the new time, in deadline order. Each timer
// receives its own deadline, as a real timer would deliver the time it
// fired. A negative d panics: time never goes backwards.
func (f *Fake) Advance(d time.Duration) {
	if d < 0 {
		panic("clock: Fake.Advance with a negative duration")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)

	var due, pending []*fakeTimer
	for _, t := range f.timers {
		if t.when.After(f.now) {
			pending = append(pending, t)
		} else {
			due = append(due, t)
		}
	}
	f.timers = pending
	slices.SortStableFunc(due, func(a, b *fakeTimer) int { return a.when.Compare(b.when) })
	for _, t := range due {
		t.c <- t.when // buffered with capacity 1 and each timer fires once: never blocks
	}
}

// PendingTimers returns the number of timers that have not fired or been
// stopped yet.
func (f *Fake) PendingTimers() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.timers)
}

// BlockUntilTimers waits until at least n timers are pending. Call it
// before Advance when the code under test creates its timer in another
// goroutine; otherwise Advance could run before the timer exists.
func (f *Fake) BlockUntilTimers(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for len(f.timers) < n {
		f.cond.Wait()
	}
}

type fakeTimer struct {
	clock *Fake
	when  time.Time
	c     chan time.Time
}

func (t *fakeTimer) C() <-chan time.Time { return t.c }

func (t *fakeTimer) Stop() bool {
	f := t.clock
	f.mu.Lock()
	defer f.mu.Unlock()
	i := slices.Index(f.timers, t)
	if i < 0 {
		return false // already fired or stopped
	}
	f.timers = slices.Delete(f.timers, i, i+1)
	return true
}
