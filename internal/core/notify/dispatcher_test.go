package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/clock"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/config"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/model"
)

// fakeSender records payloads and fails the first failures attempts.
type fakeSender struct {
	mu       sync.Mutex
	failures int
	attempts int
	payloads []Payload
	sent     chan struct{} // receives one value per attempt
	block    chan struct{} // if set, Send waits on it (or ctx)
}

func newFakeSender(failures int) *fakeSender {
	return &fakeSender{failures: failures, sent: make(chan struct{}, 100)}
}

func (f *fakeSender) Send(ctx context.Context, body []byte) error {
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.mu.Lock()
	defer func() { f.mu.Unlock(); f.sent <- struct{}{} }()
	f.attempts++
	if f.attempts <= f.failures {
		return errors.New("unavailable")
	}
	var p Payload
	if err := json.Unmarshal(body, &p); err != nil {
		return err
	}
	f.payloads = append(f.payloads, p)
	return nil
}

func (f *fakeSender) delivered() []Payload {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Payload(nil), f.payloads...)
}

func testChannel(name string) config.Channel {
	return config.Channel{
		Name: name, Type: config.ChannelWebhook,
		Retry: config.RetryPolicy{Attempts: 3, Delay: config.Duration(5 * time.Second),
			Backoff: config.BackoffExponential, MaxDelay: config.Duration(8 * time.Second)},
	}
}

func testEvent(source string) model.Event {
	return model.Event{ID: source, Module: "test", Source: source, SourceType: "systemd", Type: "thing_failed"}
}

// start runs a dispatcher over the given channels; every event goes to
// every channel named in route.
type harness struct {
	d       *Dispatcher
	clock   *clock.Fake
	events  chan model.Event
	senders map[string]*fakeSender
	cancel  context.CancelFunc
	done    chan struct{}
}

func start(t *testing.T, channels []config.Channel, senders map[string]*fakeSender, route Route, queue int) *harness {
	t.Helper()
	h := &harness{clock: clock.NewFake(time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)),
		events: make(chan model.Event), senders: senders, done: make(chan struct{})}
	d, err := NewDispatcher(channels, route, Options{Clock: h.clock, QueueSize: queue,
		NewSender: func(ch config.Channel) (Sender, error) { return senders[ch.Name], nil }})
	if err != nil {
		t.Fatal(err)
	}
	h.d = d
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() { _ = d.Run(ctx, h.events); close(h.done) }() // runs once: cannot fail
	t.Cleanup(func() { cancel(); <-h.done })
	return h
}

// wait blocks until sender has made n attempts.
func wait(t *testing.T, s *fakeSender, n int) {
	t.Helper()
	for range n {
		select {
		case <-s.sent:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for a delivery attempt")
		}
	}
}

// waitStats blocks until the channel's counters satisfy ok.
func waitStats(t *testing.T, d *Dispatcher, name string, ok func(Stats) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok(d.Stats()[name]) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for channel %s: %+v", name, d.Stats()[name])
		}
		time.Sleep(time.Millisecond)
	}
}

func all(names ...string) Route { return func(model.Event) []string { return names } }

func TestRoutesEventsToTheirChannels(t *testing.T) {
	ops, pager := newFakeSender(0), newFakeSender(0)
	disabled := testChannel("off")
	off := false
	disabled.Enabled = &off
	route := func(e model.Event) []string {
		if e.Source == "db" {
			return []string{"ops", "pager", "off", "unknown"}
		}
		return []string{"ops"}
	}
	h := start(t, []config.Channel{testChannel("ops"), testChannel("pager"), disabled},
		map[string]*fakeSender{"ops": ops, "pager": pager}, route, 8)
	h.events <- testEvent("web")
	h.events <- testEvent("db")
	wait(t, ops, 2)
	wait(t, pager, 1)
	if got := ops.delivered(); len(got) != 2 || got[0].Event.Source != "web" || got[0].PayloadVersion != PayloadVersion {
		t.Errorf("ops received %+v", got)
	}
	if got := pager.delivered(); len(got) != 1 || got[0].Event.Source != "db" {
		t.Errorf("pager received %+v", got)
	}
}

// Failed attempts are retried after 5s, then 8s (10s capped by max_delay).
func TestRetriesWithBackoff(t *testing.T) {
	ops := newFakeSender(2)
	h := start(t, []config.Channel{testChannel("ops")}, map[string]*fakeSender{"ops": ops}, all("ops"), 8)
	h.events <- testEvent("web")
	wait(t, ops, 1)
	h.clock.BlockUntilTimers(1)
	h.clock.Advance(4 * time.Second)
	select {
	case <-ops.sent:
		t.Fatal("retried before the 5s delay")
	case <-time.After(50 * time.Millisecond):
	}
	h.clock.Advance(time.Second)
	wait(t, ops, 1)
	h.clock.BlockUntilTimers(1)
	h.clock.Advance(8 * time.Second)
	wait(t, ops, 1)
	if got := ops.delivered(); len(got) != 1 {
		t.Fatalf("delivered %d payloads after two failures, want 1", len(got))
	}
	if s := h.d.Stats()["ops"]; s.Delivered != 1 || s.Failed != 0 {
		t.Errorf("stats %+v", s)
	}
}

func TestGivesUpAfterTheLastAttempt(t *testing.T) {
	ops := newFakeSender(100)
	ch := testChannel("ops")
	ch.Retry.Delay, ch.Retry.MaxDelay = 0, 0
	h := start(t, []config.Channel{ch}, map[string]*fakeSender{"ops": ops}, all("ops"), 8)
	h.events <- testEvent("web")
	wait(t, ops, 3)
	h.cancel()
	<-h.done
	if s := h.d.Stats()["ops"]; s.Failed != 1 || s.Delivered != 0 {
		t.Errorf("stats %+v", s)
	}
}

// repeat_interval holds back the same event and reports how many were
// held back in the next delivery.
func TestRepeatInterval(t *testing.T) {
	ops := newFakeSender(0)
	ch := testChannel("ops")
	ch.RepeatInterval = config.Duration(10 * time.Minute)
	h := start(t, []config.Channel{ch}, map[string]*fakeSender{"ops": ops}, all("ops"), 8)
	h.events <- testEvent("web")
	wait(t, ops, 1)
	h.events <- testEvent("web") // same event: suppressed
	h.events <- testEvent("web") // suppressed
	h.events <- testEvent("db")  // another source: delivered
	wait(t, ops, 1)
	h.clock.Advance(10 * time.Minute)
	h.events <- testEvent("web")
	wait(t, ops, 1)
	got := ops.delivered()
	if len(got) != 3 || got[1].Event.Source != "db" || got[2].SuppressedCount != 2 || got[0].SuppressedCount != 0 {
		t.Fatalf("deliveries %+v", got)
	}
	if s := h.d.Stats()["ops"]; s.Suppressed != 2 {
		t.Errorf("stats %+v", s)
	}
}

// A blocked channel fills its queue and drops the oldest events; another
// channel keeps delivering.
func TestSlowChannelDoesNotBlockOthers(t *testing.T) {
	slow, fast := newFakeSender(0), newFakeSender(0)
	slow.block = make(chan struct{})
	h := start(t, []config.Channel{testChannel("slow"), testChannel("fast")},
		map[string]*fakeSender{"slow": slow, "fast": fast}, all("slow", "fast"), 2)
	for _, s := range []string{"a", "b", "c", "d", "e"} {
		h.events <- testEvent(s)
		wait(t, fast, 1) // fast delivers each event while slow is stuck
	}
	close(slow.block)
	h.cancel()
	<-h.done
	if s := h.d.Stats()["slow"]; s.Dropped == 0 {
		t.Errorf("slow channel stats %+v: expected drops", s)
	}
}

// Closing the input completes queued deliveries; cancelling the context
// abandons them, even during a retry delay.
func TestRunStops(t *testing.T) {
	t.Run("input closed", func(t *testing.T) {
		ops := newFakeSender(0)
		h := start(t, []config.Channel{testChannel("ops")}, map[string]*fakeSender{"ops": ops}, all("ops"), 8)
		h.events <- testEvent("a")
		h.events <- testEvent("b")
		close(h.events)
		<-h.done
		if got := ops.delivered(); len(got) != 2 {
			t.Errorf("delivered %d of 2 queued events", len(got))
		}
	})
	t.Run("context cancelled during a retry delay", func(t *testing.T) {
		ops := newFakeSender(100)
		h := start(t, []config.Channel{testChannel("ops")}, map[string]*fakeSender{"ops": ops}, all("ops"), 8)
		h.events <- testEvent("a")
		wait(t, ops, 1)
		h.clock.BlockUntilTimers(1)
		h.cancel()
		select {
		case <-h.done:
		case <-time.After(5 * time.Second):
			t.Fatal("Run did not stop after cancel")
		}
	})
}

func TestBackoff(t *testing.T) {
	exp := config.RetryPolicy{Delay: config.Duration(time.Second), Backoff: config.BackoffExponential, MaxDelay: config.Duration(5 * time.Second)}
	fixed := config.RetryPolicy{Delay: config.Duration(time.Second), Backoff: config.BackoffFixed, MaxDelay: config.Duration(5 * time.Second)}
	for attempt, want := range map[int]time.Duration{1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second, 4: 5 * time.Second, 60: 5 * time.Second} {
		if got := backoff(exp, attempt); got != want {
			t.Errorf("exponential attempt %d: %s, want %s", attempt, got, want)
		}
		if got := backoff(fixed, attempt); got != time.Second {
			t.Errorf("fixed attempt %d: %s", attempt, got)
		}
	}
}

func TestNewDispatcherRejectsUnimplementedTypes(t *testing.T) {
	ch := testChannel("slack")
	ch.Type = config.ChannelSlack
	if _, err := NewDispatcher([]config.Channel{ch}, all(), Options{}); err == nil {
		t.Error("planned channel type accepted")
	}
}

// R6: past the key limit, active repeat windows survive; new keys are
// delivered without being tracked.
func TestRepeatWindowsSurviveManyKeys(t *testing.T) {
	ch := testChannel("ops")
	ch.RepeatInterval = config.Duration(time.Hour)
	ops := newFakeSender(0)
	h := start(t, []config.Channel{ch}, map[string]*fakeSender{"ops": ops}, all("ops"), 8)
	for i := range maxRepeatKeys + 1 {
		h.events <- testEvent(fmt.Sprint(i))
		wait(t, ops, 1)
	}
	h.events <- testEvent("0") // inside its window: held back
	close(h.events)
	<-h.done
	if got := len(ops.delivered()); got != maxRepeatKeys+1 {
		t.Fatalf("%d deliveries, want %d: a repeat inside its window was delivered", got, maxRepeatKeys+1)
	}
}

// A repeat entry whose interval is over but which still has held-back
// deliveries is kept until its next delivery reports them.
func TestFullRepeatMemoryKeepsPendingCounts(t *testing.T) {
	ch := testChannel("ops")
	ch.RepeatInterval = config.Duration(time.Hour)
	ops := newFakeSender(0)
	h := start(t, []config.Channel{ch}, map[string]*fakeSender{"ops": ops}, all("ops"), 8)
	for i := range maxRepeatKeys {
		h.events <- testEvent(fmt.Sprint(i))
		wait(t, ops, 1)
	}
	h.events <- testEvent("0")        // held back: count 1
	h.events <- testEvent("overflow") // memory full: delivered untracked
	wait(t, ops, 1)
	h.clock.Advance(time.Hour)
	h.events <- testEvent("new") // forgets expired entries
	wait(t, ops, 1)
	h.events <- testEvent("0")
	wait(t, ops, 1)
	close(h.events)
	<-h.done
	got := ops.delivered()
	if last := got[len(got)-1]; last.Event.Source != "0" || last.SuppressedCount != 1 {
		t.Fatalf("last delivery %s with suppressed_count %d, want 0 with 1", last.Event.Source, last.SuppressedCount)
	}
}

// Review 2b R1: pending counts of events that never come back must not
// fill the repeat memory for good. When it is full, the oldest expired
// entry with a pending count is forgotten and its count is reported as
// lost (D-072).
func TestFullRepeatMemoryForgetsStalePendingCounts(t *testing.T) {
	ch := testChannel("ops")
	ch.RepeatInterval = config.Duration(time.Hour)
	ops := newFakeSender(0)
	h := start(t, []config.Channel{ch}, map[string]*fakeSender{"ops": ops}, all("ops"), 2*maxRepeatKeys)
	for i := range maxRepeatKeys {
		h.events <- testEvent(fmt.Sprint(i))
		wait(t, ops, 1)
	}
	for i := range maxRepeatKeys {
		h.events <- testEvent(fmt.Sprint(i)) // held back: pending count 1
	}
	// The worker is done with an event once it is counted: only then may
	// time move, or it would record a delivery after the windows ended.
	waitStats(t, h.d, "ops", func(s Stats) bool { return s.Suppressed == maxRepeatKeys })
	h.clock.Advance(24 * time.Hour) // every window is over
	for range 3 {
		h.events <- testEvent("flapping")
	}
	close(h.events)
	<-h.done
	got := 0
	for _, p := range ops.delivered() {
		if p.Event.Source == "flapping" {
			got++
		}
	}
	if got != 1 {
		t.Errorf("repeat_interval 1h, same event 3 times in an instant: %d deliveries, want 1", got)
	}
	if s := h.d.Stats()["ops"]; s.SuppressedLost != 1 {
		t.Errorf("stats %+v: want one lost suppressed delivery", s)
	}
}

// Review 2b R5: Run is single-use; a second call reports an error instead
// of panicking on the closed queues.
func TestRunTwice(t *testing.T) {
	d, err := NewDispatcher([]config.Channel{testChannel("ops")}, all("ops"), Options{
		NewSender: func(config.Channel) (Sender, error) { return newFakeSender(0), nil }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := d.Run(ctx, nil); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if err := d.Run(ctx, nil); err == nil {
		t.Error("second Run accepted")
	}
}
