package supervisor

// Health is a service's state.
type Health string

// Service states (D-080 (3)).
const (
	HealthUnknown Health = "unknown" // no check has decided yet
	HealthHealthy Health = "healthy"
	HealthFailed  Health = "failed"
)

// Policy is a service's failure_policy (D-012): how many consecutive
// failures make it failed, and how many consecutive successes make a
// failed service healthy again.
type Policy struct {
	Failures, Successes int
}

// streak is what the policy decides on: the state and the current runs.
type streak struct {
	Health              Health
	Failures, Successes int
}

// transition is what a check changed.
type transition int

const (
	noChange  transition = iota
	becameUp             // unknown → healthy, on the first success: no event
	failed               // → failed: service_failed
	recovered            // failed → healthy: service_recovered
)

// maxStreak saturates the runs, so a service failing for years never
// overflows them.
const maxStreak = 1 << 30

// step applies one check's outcome to s. It is a pure function: the
// clock, the state file and the events are the caller's.
func step(s streak, ok bool, p Policy) (streak, transition) {
	if ok {
		s.Failures = 0
		s.Successes = min(s.Successes+1, maxStreak)
		switch {
		case s.Health == HealthUnknown:
			s.Health = HealthHealthy
			return s, becameUp
		case s.Health == HealthFailed && s.Successes >= p.Successes:
			s.Health = HealthHealthy
			return s, recovered
		}
		return s, noChange
	}
	s.Successes = 0
	s.Failures = min(s.Failures+1, maxStreak)
	if s.Health != HealthFailed && s.Failures >= p.Failures {
		s.Health = HealthFailed
		return s, failed
	}
	return s, noChange
}
