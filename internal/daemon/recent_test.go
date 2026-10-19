package daemon

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"log/slog"
	"sync"
	"testing"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/authz"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/events"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/api"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/model"
)

// sources returns the Source of each event, in order.
func sources(evs []model.Event) []string {
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.Source)
	}
	return out
}

// D-083: the history keeps the last api.MaxEventsLimit events and returns
// them newest first.
func TestRecentEvents(t *testing.T) {
	tests := []struct {
		name       string
		added, ask int
		first      string // newest returned
		last       string // oldest returned
		count      int
	}{
		{"empty", 0, 10, "", "", 0},
		{"fewer than asked", 3, 10, "s2", "s0", 3},
		{"fewer than stored", 10, 4, "s9", "s6", 4},
		{"exactly full", api.MaxEventsLimit, api.MaxEventsLimit, fmt.Sprintf("s%d", api.MaxEventsLimit-1), "s0", api.MaxEventsLimit},
		{"wrapped", api.MaxEventsLimit + 44, api.MaxEventsLimit, fmt.Sprintf("s%d", api.MaxEventsLimit+43), "s44", api.MaxEventsLimit},
		{"wrapped, a few", 2*api.MaxEventsLimit + 1, 3, fmt.Sprintf("s%d", 2*api.MaxEventsLimit), fmt.Sprintf("s%d", 2*api.MaxEventsLimit-2), 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRecentEvents()
			for i := range tt.added {
				r.add(model.Event{Source: fmt.Sprintf("s%d", i)})
			}
			got := sources(r.last(tt.ask))
			if len(got) != tt.count {
				t.Fatalf("%d events, want %d", len(got), tt.count)
			}
			if tt.count > 0 && (got[0] != tt.first || got[len(got)-1] != tt.last) {
				t.Errorf("newest %s, oldest %s; want %s, %s", got[0], got[len(got)-1], tt.first, tt.last)
			}
		})
	}
}

// Adds and reads from many goroutines: run with -race.
func TestRecentEventsConcurrent(t *testing.T) {
	r := newRecentEvents()
	var wg sync.WaitGroup
	for w := range 4 {
		wg.Go(func() {
			for i := range 500 {
				r.add(model.Event{Source: fmt.Sprintf("w%d-%d", w, i)})
			}
		})
		wg.Go(func() {
			for range 500 {
				if n := len(r.last(api.MaxEventsLimit)); n > api.MaxEventsLimit {
					t.Errorf("%d events", n)
				}
			}
		})
	}
	wg.Wait()
	if n := len(r.last(api.MaxEventsLimit)); n != api.MaxEventsLimit {
		t.Errorf("%d events after 2000 adds", n)
	}
}

// Critique C3: the arguments come from the socket and are checked by the
// daemon.
func TestEventsLimit(t *testing.T) {
	tests := []struct {
		args string
		want int // 0: bad_request
	}{
		{"", api.DefaultEventsLimit},
		{"{}", api.DefaultEventsLimit},
		{`{"limit":1}`, 1},
		{`{"limit":256}`, 256},
		{`{"limit":0}`, 0},
		{`{"limit":257}`, 0},
		{`{"limit":-1}`, 0},
		{`{"limit":"10"}`, 0},
		{`{"limit":1.5}`, 0},
		{`{"limit":1,"limit":2}`, 0},
		{`{"limit":1,"source":"web"}`, 0},
		{`[1]`, 0},
		{`7`, 0},
	}
	for _, tt := range tests {
		got, apiErr := eventsLimit(jsontext.Value(tt.args))
		switch {
		case tt.want == 0 && (apiErr == nil || apiErr.Code != api.CodeBadRequest):
			t.Errorf("%s: limit %d, error %v; want bad_request", tt.args, got, apiErr)
		case tt.want != 0 && (apiErr != nil || got != tt.want):
			t.Errorf("%s: limit %d, error %v; want %d", tt.args, got, apiErr, tt.want)
		}
	}
}

// FuzzEventsLimit: whatever the socket sends, eventsLimit never panics
// and accepts only a limit within bounds.
func FuzzEventsLimit(f *testing.F) {
	for _, s := range []string{"", "{}", `{"limit":10}`, `{"limit":0}`, `{"limit":1e9}`, `{"x":1}`, "null", `{"limit":null}`} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, args string) {
		limit, apiErr := eventsLimit(jsontext.Value(args))
		if apiErr == nil && (limit < 1 || limit > api.MaxEventsLimit) {
			t.Fatalf("accepted limit %d", limit)
		}
	})
}

// Critique C3: events lost by the log subscription's full queue are
// missing from the history and counted in the result: the real bus,
// subscription and logEvents, with the consumer started late.
func TestEventsReportsDropped(t *testing.T) {
	bus := events.NewBus(events.NewRegistry(), events.Options{Hostname: "h", Version: "test"})
	sub := bus.Subscribe(2)
	for i := range 5 {
		if _, err := bus.Publish(model.Event{Module: model.ModuleCore, Type: model.EventConfigReloaded,
			Source: fmt.Sprintf("s%d", i), SourceType: model.SourceTypeDaemon, Message: "m"}); err != nil {
			t.Fatal(err)
		}
	}
	bus.Close() // ends the consumer once it has read the queue
	d := &Daemon{log: slog.New(slog.DiscardHandler), recent: newRecentEvents()}
	d.logEvents(sub.Events())
	got, apiErr := eventsCommand(d.recent, sub.Dropped)(context.Background(), authz.Peer{}, nil)
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	res := got.(api.EventsResult)
	if s := sources(res.Events); res.Dropped != 3 || len(s) != 2 || s[0] != "s4" || s[1] != "s3" {
		t.Errorf("events %v, dropped %d; want [s4 s3], 3", s, res.Dropped)
	}
}
