package daemon

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"sync"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/authz"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/transport"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/api"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/model"
)

// recentEvents keeps the last events the log subscriber received, for
// core.events (D-080 (7), D-083). It holds at most api.MaxEventsLimit
// events, each at most 16 KiB encoded (the bus's limit), in memory only.
//
// Events are shared, never copied: the bus already gives every
// subscriber the same maps on the contract that nobody changes them, and
// core.events only encodes them.
type recentEvents struct {
	mu   sync.Mutex
	ring []model.Event // capacity api.MaxEventsLimit
	next int           // where the next event goes once the ring is full
}

func newRecentEvents() *recentEvents {
	return &recentEvents{ring: make([]model.Event, 0, api.MaxEventsLimit)}
}

// add keeps e, forgetting the oldest event when full.
func (r *recentEvents) add(e model.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.ring) < cap(r.ring) {
		r.ring = append(r.ring, e)
		return
	}
	r.ring[r.next] = e
	r.next = (r.next + 1) % len(r.ring)
}

// last returns up to n events, newest first.
func (r *recentEvents) last(n int) []model.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	size := len(r.ring)
	newest := size - 1
	if size == cap(r.ring) {
		newest = (r.next - 1 + size) % size // the slot before next
	}
	n = min(n, size)
	out := make([]model.Event, 0, n)
	for i := range n {
		out = append(out, r.ring[(newest-i+size)%size])
	}
	return out
}

// eventsCommand serves core.events from recent; dropped counts the events
// the subscription feeding it lost to a full queue (Subscription.Dropped).
// It never waits for the bus.
func eventsCommand(recent *recentEvents, dropped func() uint64) transport.Run {
	return func(_ context.Context, _ authz.Peer, args jsontext.Value) (any, *api.Error) {
		limit, apiErr := eventsLimit(args)
		if apiErr != nil {
			return nil, apiErr
		}
		return api.EventsResult{Events: recent.last(limit), Dropped: dropped()}, nil
	}
}

// eventsLimit decodes the arguments of core.events: they come from the
// socket, so they are checked here, not trusted from sentinelctl.
func eventsLimit(args jsontext.Value) (int, *api.Error) {
	var a api.EventsArgs
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a, json.RejectUnknownMembers(true)); err != nil {
			return 0, &api.Error{Code: api.CodeBadRequest, Message: `core.events takes {"limit": 1-` + fmt.Sprint(api.MaxEventsLimit) + `} or nothing`}
		}
	}
	limit := api.DefaultEventsLimit
	if a.Limit != nil {
		limit = *a.Limit
	}
	if limit < 1 || limit > api.MaxEventsLimit {
		return 0, &api.Error{Code: api.CodeBadRequest, Message: fmt.Sprintf("limit must be 1 to %d", api.MaxEventsLimit)}
	}
	return limit, nil
}
