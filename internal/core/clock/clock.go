// Package clock abstracts the passage of time so that code which waits,
// retries or schedules can be tested without sleeping.
//
// Production code receives Real(); tests receive a *Fake and move time
// forward explicitly with Advance. Unlike most interfaces in this
// repository, Clock lives with its implementations rather than with a
// consumer: many packages share it, and real vs fake time is the variation
// it isolates.
package clock

import "time"

// Clock tells the time and creates timers.
type Clock interface {
	// Now returns the current time.
	Now() time.Time
	// NewTimer returns a timer that delivers the time on its channel once,
	// after at least d. A zero or negative d fires as soon as possible.
	NewTimer(d time.Duration) Timer
}

// Timer is a single-shot timer, like *time.Timer.
type Timer interface {
	// C returns the channel on which the firing time is delivered.
	C() <-chan time.Time
	// Stop prevents the timer from firing. It reports whether the call
	// stopped the timer, false if it had already fired or been stopped.
	Stop() bool
}

// Real returns a Clock backed by the time package.
func Real() Clock { return realClock{} }

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) NewTimer(d time.Duration) Timer { return realTimer{time.NewTimer(d)} }

type realTimer struct{ t *time.Timer }

func (r realTimer) C() <-chan time.Time { return r.t.C }

func (r realTimer) Stop() bool { return r.t.Stop() }
