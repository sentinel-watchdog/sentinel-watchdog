package events

import (
	"errors"
	"regexp"
	"sync"
	"sync/atomic"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/clock"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/model"
)

// ErrClosed is returned by Publish after Close.
var ErrClosed = errors.New("events: bus is closed")

// eventIDRe is the format of model.NewEventID.
var eventIDRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Options configure a Bus.
type Options struct {
	// Hostname and Version are stamped on every event.
	Hostname, Version string
	// Clock stamps events without a timestamp; defaults to clock.Real().
	Clock clock.Clock
	// MaxEventBytes caps the JSON form of an event; defaults to
	// DefaultMaxEventBytes, and is never below 12 KiB.
	MaxEventBytes int
}

// Bus validates events and hands each one to every subscriber. Publish
// never blocks the emitter: each subscriber has a bounded queue, and when
// it is full the oldest queued event is dropped and counted. Events from
// one goroutine reach each subscriber in the order they were published.
type Bus struct {
	reg  *Registry
	opts Options

	mu     sync.RWMutex // guards subs and closed
	subs   []*Subscription
	closed bool
}

// NewBus returns a bus that accepts the event types of reg.
func NewBus(reg *Registry, opts Options) *Bus {
	if opts.Clock == nil {
		opts.Clock = clock.Real()
	}
	opts.Hostname = clean(opts.Hostname, maxIdentifierBytes)
	opts.Version = clean(opts.Version, maxIdentifierBytes)
	if opts.MaxEventBytes <= 0 {
		opts.MaxEventBytes = DefaultMaxEventBytes
	}
	opts.MaxEventBytes = max(opts.MaxEventBytes, minMaxEventBytes)
	return &Bus{reg: reg, opts: opts}
}

// Subscription receives the events published after it was created.
// Events are shared between subscribers: a subscriber must not modify
// their maps.
type Subscription struct {
	ch      chan model.Event
	mu      sync.Mutex // serialises offers and close
	closed  bool
	dropped atomic.Uint64
}

// Subscribe registers a consumer with a queue of capacity events (at
// least 1).
func (b *Bus) Subscribe(capacity int) *Subscription {
	s := &Subscription{ch: make(chan model.Event, max(capacity, 1))}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		close(s.ch)
		s.closed = true
		return s
	}
	b.subs = append(b.subs, s)
	return s
}

// Publish validates e, completes it (ID, timestamp, hostname, version,
// default severity) and queues it for every subscriber. It returns the
// event's ID, so the emitter can use it as a correlation ID. The delivered
// event is not returned: subscribers share its maps, and an emitter that
// changed them would race with the subscribers.
func (b *Bus) Publish(e model.Event) (string, error) {
	switch {
	case e.ID == "":
		e.ID = model.NewEventID()
	case !eventIDRe.MatchString(e.ID):
		return "", errors.New("events: event_id must be 32 lower-case hex digits")
	}
	if e.Timestamp.IsZero() {
		e.Timestamp = b.opts.Clock.Now()
	}
	e.Timestamp = e.Timestamp.UTC()
	e.Hostname, e.SentinelVersion = b.opts.Hostname, b.opts.Version
	e, err := normalize(e, b.reg, b.opts.MaxEventBytes)
	if err != nil {
		return "", err
	}

	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return "", ErrClosed
	}
	for _, s := range b.subs {
		s.offer(e)
	}
	return e.ID, nil
}

// Close stops delivery and closes every subscription's channel, so
// consumers ranging over Events end. Publish then returns ErrClosed.
func (b *Bus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for _, s := range b.subs {
		s.close()
	}
}

// Events returns the channel the subscription's events arrive on. It is
// closed when the bus closes.
func (s *Subscription) Events() <-chan model.Event { return s.ch }

// Dropped returns how many events were dropped because the queue was full.
func (s *Subscription) Dropped() uint64 { return s.dropped.Load() }

// offer queues e, dropping the oldest queued event when the queue is full.
func (s *Subscription) offer(e model.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	select {
	case s.ch <- e:
		return
	default:
	}
	select {
	case <-s.ch:
		s.dropped.Add(1)
	default: // the consumer emptied the queue meanwhile
	}
	// There is room now: offers are serialised by s.mu, and consumers
	// only take events out.
	s.ch <- e
}

func (s *Subscription) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.ch)
	}
}

// Emitter publishes the events of one module. The daemon gives each module
// its own, so a module can neither register types nor publish events in
// another module's name.
type Emitter struct {
	bus    *Bus
	module string
}

// Emitter returns the emitter of module.
func (b *Bus) Emitter(module string) *Emitter {
	return &Emitter{bus: b, module: module}
}

// Register declares the event types the module emits (Registry.Register).
func (e *Emitter) Register(specs ...TypeSpec) error {
	return e.bus.reg.Register(e.module, specs...)
}

// Publish sets the event's module and publishes it (Bus.Publish).
func (e *Emitter) Publish(ev model.Event) (string, error) {
	ev.Module = e.module
	return e.bus.Publish(ev)
}
