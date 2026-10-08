package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/clock"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/config"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/model"
)

// PayloadVersion is the version of the webhook payload format
// (docs/notifications.md).
const PayloadVersion = 1

// Payload is the JSON body sent to a channel: a versioned envelope around
// the event, with room for delivery information next to it.
type Payload struct {
	PayloadVersion int         `json:"payload_version"`
	Event          model.Event `json:"event"`
	// SuppressedCount is how many deliveries of the same event (module,
	// source and event type) repeat_interval held back since the last one.
	SuppressedCount int `json:"suppressed_count,omitempty"`
}

const (
	// DefaultQueueSize is the number of events each channel can hold
	// while it delivers.
	DefaultQueueSize = 256
	// maxRepeatKeys bounds the memory of repeat_interval per channel.
	maxRepeatKeys = 4096
)

// Sender makes one delivery attempt of an encoded payload. It isolates the
// provider (webhook today; others are planned) and lets tests replace it.
type Sender interface {
	Send(ctx context.Context, body []byte) error
}

// Route returns the names of the channels an event goes to. The daemon
// builds it from notifications.core and the modules' routes.
type Route func(model.Event) []string

// Options configure a Dispatcher.
type Options struct {
	// Clock times retries and repeat_interval; defaults to clock.Real().
	Clock clock.Clock
	// QueueSize is each channel's queue; defaults to DefaultQueueSize.
	QueueSize int
	// NewSender builds a channel's sender; defaults to NewWebhook.
	NewSender func(config.Channel) (Sender, error)
}

// Stats counts what happened to the events routed to one channel.
type Stats struct {
	Delivered  uint64 // delivered within the retry budget
	Failed     uint64 // still failing after every attempt
	Dropped    uint64 // dropped because the queue was full
	Suppressed uint64 // held back by repeat_interval
}

// Dispatcher routes events to channels and delivers them asynchronously:
// each enabled channel has a bounded queue and a worker that retries with
// backoff. A slow or failing channel never delays the others, nor the
// code that publishes events.
type Dispatcher struct {
	route    Route
	clock    clock.Clock
	channels map[string]*channel
}

type channel struct {
	cfg    config.Channel
	sender Sender
	queue  chan model.Event

	delivered, failed, dropped, suppressed atomic.Uint64
}

// NewDispatcher prepares a worker for every enabled channel. Events routed
// to a disabled or unknown channel are not delivered.
func NewDispatcher(channels []config.Channel, route Route, opts Options) (*Dispatcher, error) {
	if opts.Clock == nil {
		opts.Clock = clock.Real()
	}
	if opts.QueueSize <= 0 {
		opts.QueueSize = DefaultQueueSize
	}
	if opts.NewSender == nil {
		opts.NewSender = func(ch config.Channel) (Sender, error) { return NewWebhook(ch) }
	}
	d := &Dispatcher{route: route, clock: opts.Clock, channels: map[string]*channel{}}
	for _, ch := range channels {
		if !ch.IsEnabled() {
			continue
		}
		if ch.Type != config.ChannelWebhook {
			return nil, fmt.Errorf("notify: channel %q: type %q is not implemented", ch.Name, ch.Type)
		}
		sender, err := opts.NewSender(ch)
		if err != nil {
			return nil, err
		}
		d.channels[ch.Name] = &channel{cfg: ch, sender: sender, queue: make(chan model.Event, opts.QueueSize)}
	}
	return d, nil
}

// Run delivers the events received on events until the channel is closed
// (queued deliveries are then completed) or ctx is cancelled (pending
// deliveries are abandoned). It owns the workers and returns after they
// have stopped.
func (d *Dispatcher) Run(ctx context.Context, events <-chan model.Event) {
	var workers sync.WaitGroup
	for _, ch := range d.channels {
		workers.Go(func() { d.work(ctx, ch) })
	}
	defer workers.Wait()
	defer func() {
		for _, ch := range d.channels {
			close(ch.queue)
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case e, ok := <-events:
			if !ok {
				return
			}
			for _, name := range d.route(e) {
				if ch, ok := d.channels[name]; ok {
					ch.enqueue(e)
				}
			}
		}
	}
}

// Stats returns the counters of every enabled channel.
func (d *Dispatcher) Stats() map[string]Stats {
	out := make(map[string]Stats, len(d.channels))
	for name, ch := range d.channels {
		out[name] = Stats{
			Delivered: ch.delivered.Load(), Failed: ch.failed.Load(),
			Dropped: ch.dropped.Load(), Suppressed: ch.suppressed.Load(),
		}
	}
	return out
}

// enqueue adds e to the queue, dropping the oldest queued event when it
// is full. Only Run calls it: with a single producer, no lock is needed.
func (ch *channel) enqueue(e model.Event) {
	select {
	case ch.queue <- e:
		return
	default:
	}
	select {
	case <-ch.queue:
		ch.dropped.Add(1)
	default: // the worker emptied the queue meanwhile
	}
	ch.queue <- e // room now: the worker only takes events out
}

// repeatKey identifies "the same event" for repeat_interval.
type repeatKey struct {
	module, source string
	sourceType     model.SourceType
	eventType      model.EventType
}

type repeatState struct {
	sent       time.Time
	suppressed int
}

// work delivers the channel's queue until it is closed or ctx is done.
func (d *Dispatcher) work(ctx context.Context, ch *channel) {
	recent := map[repeatKey]*repeatState{} // owned by this goroutine
	interval := ch.cfg.RepeatInterval.Std()
	for e := range ch.queue {
		if ctx.Err() != nil {
			return
		}
		key := repeatKey{e.Module, e.Source, e.SourceType, e.Type}
		st := recent[key]
		now := d.clock.Now()
		if interval > 0 && st != nil && now.Sub(st.sent) < interval {
			st.suppressed++
			ch.suppressed.Add(1)
			continue
		}
		payload := Payload{PayloadVersion: PayloadVersion, Event: e}
		if st != nil {
			payload.SuppressedCount = st.suppressed
		}
		if !d.deliver(ctx, ch, payload) {
			continue
		}
		if interval > 0 {
			now := d.clock.Now()
			if _, tracked := recent[key]; tracked || roomFor(recent, now, interval) {
				recent[key] = &repeatState{sent: now}
			}
		}
	}
}

// deliver sends one payload with the channel's retry policy and reports
// whether it was delivered.
func (d *Dispatcher) deliver(ctx context.Context, ch *channel, p Payload) bool {
	body, err := json.Marshal(p)
	if err != nil { // events are normalised by the bus; this cannot happen
		ch.failed.Add(1)
		return false
	}
	retry := ch.cfg.Retry
	for attempt := 1; ; attempt++ {
		if ch.sender.Send(ctx, body) == nil {
			ch.delivered.Add(1)
			return true
		}
		if attempt >= retry.Attempts || !d.sleep(ctx, backoff(retry, attempt)) {
			ch.failed.Add(1)
			return false
		}
	}
}

// backoff returns the delay after the given failed attempt (1-based).
func backoff(r config.RetryPolicy, attempt int) time.Duration {
	delay := r.Delay.Std()
	if r.Backoff == config.BackoffExponential {
		for range attempt - 1 {
			if delay >= r.MaxDelay.Std() {
				break
			}
			delay *= 2
		}
	}
	return min(delay, r.MaxDelay.Std())
}

// sleep waits d on the dispatcher's clock and reports false if ctx ended
// first.
func (d *Dispatcher) sleep(ctx context.Context, wait time.Duration) bool {
	if wait <= 0 {
		return ctx.Err() == nil
	}
	timer := d.clock.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C():
		return true
	}
}

// roomFor reports whether the repeat memory can track one more event,
// first forgetting entries whose interval is over and that have nothing
// left to report. When the memory is full of open windows and pending
// counts, a new event is delivered without being tracked: no window and
// no suppressed_count is ever dropped (D-072).
func roomFor(recent map[repeatKey]*repeatState, now time.Time, interval time.Duration) bool {
	if len(recent) < maxRepeatKeys {
		return true
	}
	for k, st := range recent {
		if now.Sub(st.sent) >= interval && st.suppressed == 0 {
			delete(recent, k)
		}
	}
	return len(recent) < maxRepeatKeys
}
