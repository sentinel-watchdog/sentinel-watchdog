package supervisor

import (
	"slices"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/config"
)

// budget is what recovery keeps of a service (D-014): the attempts made
// inside the window, in non-decreasing order, and when the service became
// exhausted (zero: it is not). An attempt is a launch made while the
// service has an open incident: after a crash, a failed start, or a
// restart or reload of sentineld that found the incident open (D-087).
type budget struct {
	Attempts    []time.Time
	ExhaustedAt time.Time
}

// admission is whether an attempt may be launched now.
type admission struct {
	OK bool
	// Attempt is the 1-based number of the attempt in the window.
	Attempt int
	// Renewed: the cooldown is over and the budget starts again; the
	// attempt is launched without a delay.
	Renewed bool
	// Exhausted: this decision spent the budget (once per exhaustion).
	Exhausted bool
	// ResumeAt is when an exhausted budget is renewed.
	ResumeAt time.Time
}

// trim drops the attempts outside the window ending at now. A time
// after now (the clock went back) becomes now, so that a backward jump
// never frees budget early and the stored order stays valid; equal times
// are valid. The result shares nothing with b.
func trim(b budget, now time.Time, r Recovery) budget {
	now = wall(now)
	out := budget{ExhaustedAt: b.ExhaustedAt}
	if out.ExhaustedAt.After(now) {
		out.ExhaustedAt = now
	}
	cutoff := now.Add(-r.Window)
	for _, t := range b.Attempts {
		if t.After(now) {
			t = now
		}
		if t.After(cutoff) {
			out.Attempts = append(out.Attempts, t)
		}
	}
	slices.SortFunc(out.Attempts, time.Time.Compare)
	return out
}

// admit decides whether an attempt may start at now. A full window
// exhausts the budget even when no exhaustion was recorded (sentineld
// stopped during the last attempt); at the end of the cooldown the budget
// is renewed, never with a cooldown of 0 (ResumeAt is then zero: only an
// operator's reset renews it, D-013). It is a pure function: the clock,
// the state file and the events are the caller's.
func admit(b budget, now time.Time, r Recovery) (budget, admission) {
	now = wall(now)
	b = trim(b, now, r)
	if !b.ExhaustedAt.IsZero() {
		resume := resumeAt(b.ExhaustedAt, r)
		if resume.IsZero() || now.Before(resume) {
			return b, admission{ResumeAt: resume}
		}
		return budget{}, admission{OK: true, Attempt: 1, Renewed: true}
	}
	if len(b.Attempts) >= r.MaxAttempts {
		b.ExhaustedAt = now
		return b, admission{Exhausted: true, ResumeAt: resumeAt(now, r)}
	}
	return b, admission{OK: true, Attempt: len(b.Attempts) + 1}
}

// resumeAt is when a budget exhausted at t is renewed: zero with a
// cooldown of 0, which waits for an operator's reset.
func resumeAt(t time.Time, r Recovery) time.Time {
	if r.Cooldown == 0 {
		return time.Time{}
	}
	return t.Add(r.Cooldown)
}

// delay is the wait before attempt n (1-based): Delay, doubled for each
// earlier attempt with an exponential backoff, never more than MaxDelay.
func delay(n int, r Recovery) time.Duration {
	d := r.Delay
	if r.Backoff == config.BackoffExponential {
		for i := 1; i < n && d < r.MaxDelay; i++ {
			d *= 2
		}
	}
	return min(d, r.MaxDelay)
}

// reserve records an attempt launched at now. An earlier attempt after
// now (the clock went back since the admission) becomes now, so the
// stored order stays valid (review 3b-3 R2). The result shares nothing
// with b.
func reserve(b budget, now time.Time) budget {
	now = wall(now)
	attempts := make([]time.Time, 0, len(b.Attempts)+1)
	for _, t := range b.Attempts {
		attempts = append(attempts, minTime(t, now))
	}
	return budget{Attempts: append(attempts, now), ExhaustedAt: b.ExhaustedAt}
}

// wall drops t's monotonic reading: a budget is ordered by the wall
// clock, the only one a state file keeps, so a comparison in memory
// agrees with one after a restart (review 3b-3 R2).
func wall(t time.Time) time.Time { return t.Round(0) }

func minTime(a, b time.Time) time.Time {
	if a.After(b) {
		return b
	}
	return a
}
