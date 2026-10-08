package events

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/clock"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/model"
)

func newBus(t *testing.T) (*Bus, *clock.Fake) {
	t.Helper()
	fake := clock.NewFake(time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC))
	return NewBus(testRegistry(t), Options{Hostname: "web1", Version: "1.0.0", Clock: fake}), fake
}

func TestPublishCompletesTheEvent(t *testing.T) {
	bus, fake := newBus(t)
	sub := bus.Subscribe(4)
	e := event()
	e.Hostname = "spoofed"
	got, err := bus.Publish(e)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID == "" || !got.Timestamp.Equal(fake.Now()) || got.Hostname != "web1" ||
		got.SentinelVersion != "1.0.0" || got.Severity != model.SeverityError {
		t.Errorf("event not completed: %+v", got)
	}
	if delivered := <-sub.Events(); delivered.ID != got.ID {
		t.Errorf("delivered %q, published %q", delivered.ID, got.ID)
	}
}

func TestPublishRejectsInvalidEvents(t *testing.T) {
	bus, _ := newBus(t)
	sub := bus.Subscribe(4)
	e := event()
	e.Type = "unknown"
	if _, err := bus.Publish(e); err == nil {
		t.Fatal("invalid event accepted")
	}
	select {
	case got := <-sub.Events():
		t.Fatalf("invalid event delivered: %+v", got)
	default:
	}
}

func TestEverySubscriberReceivesEveryEvent(t *testing.T) {
	bus, _ := newBus(t)
	a, b := bus.Subscribe(8), bus.Subscribe(8)
	for i := range 3 {
		e := event()
		e.Message = fmt.Sprint(i)
		if _, err := bus.Publish(e); err != nil {
			t.Fatal(err)
		}
	}
	for _, sub := range []*Subscription{a, b} {
		for i := range 3 {
			if got := <-sub.Events(); got.Message != fmt.Sprint(i) {
				t.Fatalf("event %d: message %q, out of order", i, got.Message)
			}
		}
	}
}

// A full queue drops its oldest events and counts them; the emitter never
// blocks and the newest events survive.
func TestFullQueueDropsTheOldest(t *testing.T) {
	bus, _ := newBus(t)
	sub := bus.Subscribe(2)
	for i := range 5 {
		e := event()
		e.Message = fmt.Sprint(i)
		if _, err := bus.Publish(e); err != nil {
			t.Fatal(err)
		}
	}
	if got := sub.Dropped(); got != 3 {
		t.Errorf("Dropped() = %d, want 3", got)
	}
	for _, want := range []string{"3", "4"} {
		if got := <-sub.Events(); got.Message != want {
			t.Errorf("got %q, want %q", got.Message, want)
		}
	}
}

func TestCloseEndsSubscriptions(t *testing.T) {
	bus, _ := newBus(t)
	sub := bus.Subscribe(1)
	bus.Close()
	bus.Close() // idempotent
	if _, open := <-sub.Events(); open {
		t.Error("subscription channel still open after Close")
	}
	if _, err := bus.Publish(event()); !errors.Is(err, ErrClosed) {
		t.Errorf("Publish after Close: %v, want ErrClosed", err)
	}
	if _, open := <-bus.Subscribe(1).Events(); open {
		t.Error("Subscribe after Close returned an open channel")
	}
}

// Concurrent publishers, a slow consumer and Close: run with -race.
func TestConcurrentPublishAndClose(t *testing.T) {
	bus, _ := newBus(t)
	sub := bus.Subscribe(16)
	var consumed sync.WaitGroup
	consumed.Go(func() {
		for range sub.Events() {
		}
	})
	var publishers sync.WaitGroup
	for range 8 {
		publishers.Go(func() {
			for range 200 {
				if _, err := bus.Publish(event()); err != nil && !errors.Is(err, ErrClosed) {
					t.Error(err)
					return
				}
			}
		})
	}
	publishers.Wait()
	bus.Close()
	consumed.Wait()
}
