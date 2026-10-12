package transport

import (
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/clock"
)

// Admission limits (D-077). A user may open admitBurst connections at once,
// then admitRate per second. Root is not limited.
const (
	admitBurst = 10
	admitRate  = 1.0 // connections per second
	// maxTrackedUsers bounds the table of users; when it is full and no
	// user is idle, a new user is refused until one is.
	maxTrackedUsers = 1024
)

// admission is a token bucket per user. Every denied operate or admin
// request costs a durable audit record, and the audit log keeps a bounded
// number of bytes: without a limit, one local user looping requests could
// push older evidence out of the log and hold every connection slot.
// Limiting connections, before any request is read, keeps every request
// that is read audited. Only the accept loop uses it: no lock.
type admission struct {
	clock clock.Clock
	users map[uint32]*bucket
}

type bucket struct {
	tokens  float64
	at      time.Time
	limited bool // refused since it was last admitted (logged once)
}

func newAdmission(c clock.Clock) *admission {
	return &admission{clock: c, users: map[uint32]*bucket{}}
}

// allow reports whether uid may open a connection now, and whether this
// refusal is the first since uid was last admitted.
func (a *admission) allow(uid uint32) (ok, first bool) {
	if uid == 0 {
		return true, false
	}
	now := a.clock.Now()
	b, found := a.users[uid]
	if !found {
		if len(a.users) >= maxTrackedUsers {
			a.prune(now)
		}
		if len(a.users) >= maxTrackedUsers {
			return false, false
		}
		b = &bucket{tokens: admitBurst, at: now}
		a.users[uid] = b
	}
	b.refill(now)
	if b.tokens < 1 {
		first = !b.limited
		b.limited = true
		return false, first
	}
	b.tokens--
	b.limited = false
	return true, false
}

func (b *bucket) refill(now time.Time) {
	if elapsed := now.Sub(b.at).Seconds(); elapsed > 0 {
		b.tokens = min(admitBurst, b.tokens+elapsed*admitRate)
	}
	b.at = now
}

// prune forgets the users whose bucket has refilled: they are idle.
func (a *admission) prune(now time.Time) {
	for uid, b := range a.users {
		b.refill(now)
		if b.tokens >= admitBurst {
			delete(a.users, uid)
		}
	}
}
