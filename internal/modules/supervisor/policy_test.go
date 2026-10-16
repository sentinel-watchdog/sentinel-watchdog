package supervisor

import "testing"

// AC3: N consecutive failures fail a service once, M consecutive
// successes recover it once, the first success decides nothing loud.
func TestStep(t *testing.T) {
	p := Policy{Failures: 3, Successes: 2}
	tests := []struct {
		name    string
		outcome string // f = failure, s = success
		want    []transition
		health  Health
	}{
		{"first success", "s", []transition{becameUp}, HealthHealthy},
		{"under the threshold", "ff", []transition{noChange, noChange}, HealthUnknown},
		{"fails once", "fffff", []transition{noChange, noChange, failed, noChange, noChange}, HealthFailed},
		{"a success breaks the run", "sffsff", []transition{becameUp, noChange, noChange, noChange, noChange, noChange}, HealthHealthy},
		{"recovers once", "fffsss", []transition{noChange, noChange, failed, noChange, recovered, noChange}, HealthHealthy},
		{"a failure breaks the recovery", "fffsfs", []transition{noChange, noChange, failed, noChange, noChange, noChange}, HealthFailed},
		{"fails again", "fffssfff", []transition{noChange, noChange, failed, noChange, recovered, noChange, noChange, failed}, HealthFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := streak{Health: HealthUnknown}
			for i, o := range tt.outcome {
				var tr transition
				s, tr = step(s, o == 's', p)
				if tr != tt.want[i] {
					t.Fatalf("check %d (%c): transition %d, want %d", i+1, o, tr, tt.want[i])
				}
			}
			if s.Health != tt.health {
				t.Errorf("health %s, want %s", s.Health, tt.health)
			}
		})
	}
	// One failure is enough with a threshold of one; the runs saturate.
	if s, tr := step(streak{Health: HealthHealthy}, false, Policy{1, 1}); tr != failed || s.Failures != 1 {
		t.Errorf("threshold 1: %+v %d", s, tr)
	}
	if s, _ := step(streak{Health: HealthFailed, Failures: maxStreak}, false, p); s.Failures != maxStreak {
		t.Errorf("failures %d, want saturated", s.Failures)
	}
}
