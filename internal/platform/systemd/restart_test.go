package systemd

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

// restarting answers RestartUnit with reply (a body or an error name) and
// records the arguments of the call.
func restarting(body []any, errName string, args *[]any, mu *sync.Mutex) func(string, []any) ([]any, string) {
	return func(m string, a []any) ([]any, string) {
		if m != "RestartUnit" {
			return healthy(m, a)
		}
		mu.Lock()
		*args = a
		mu.Unlock()
		return body, errName
	}
}

func TestBusRestart(t *testing.T) {
	var args []any
	var mu sync.Mutex
	b := newTestBus(&peer{t: t, answer: restarting([]any{dbus.ObjectPath("/org/freedesktop/systemd1/job/4021")}, "", &args, &mu)})
	defer b.Close()
	id, err := b.Restart(context.Background(), nginx)
	if err != nil {
		t.Fatal(err)
	}
	if id != 4021 {
		t.Errorf("job = %d, want 4021", id)
	}
	mu.Lock()
	defer mu.Unlock()
	// Mode fail: a job already queued is never replaced (D-093 (3)).
	if len(args) != 2 || args[0] != "nginx.service" || args[1] != "fail" {
		t.Errorf("RestartUnit%v, want (nginx.service, fail)", args)
	}
}

// Every error systemd answers means that nothing was queued; its name
// gives a fixed code, never its text.
func TestBusRestartRefused(t *testing.T) {
	tests := []struct{ name, code string }{
		{"org.freedesktop.systemd1.TransactionIsDestructive", RefusedConflict},
		{"org.freedesktop.systemd1.TransactionJobsConflicting", RefusedConflict},
		{"org.freedesktop.systemd1.NoSuchUnit", RefusedNotFound},
		{"org.freedesktop.systemd1.UnitMasked", RefusedMasked},
		{"org.freedesktop.systemd1.LoadFailed", RefusedLoadFailed},
		{"org.freedesktop.systemd1.BadUnitSetting", RefusedLoadFailed},
		{"org.freedesktop.systemd1.OnlyByDependency", RefusedManual},
		{"org.freedesktop.systemd1.JobTypeNotApplicable", RefusedNotApplicable},
		{"org.freedesktop.DBus.Error.AccessDenied", RefusedDenied},
		{"org.freedesktop.DBus.Error.InvalidArgs", RefusedOther},
		{"System.Error.EIO", RefusedOther},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var args []any
			var mu sync.Mutex
			b := newTestBus(&peer{t: t, answer: restarting(nil, tt.name, &args, &mu)})
			defer b.Close()
			_, err := b.Restart(context.Background(), nginx)
			var re *RefusedError
			if !errors.As(err, &re) || re.Code != tt.code {
				t.Fatalf("error = %v, want refused %s", err, tt.code)
			}
			if errors.Is(err, ErrUnknownOutcome) || errors.Is(err, ErrUnavailable) {
				t.Errorf("a refusal is %v", err)
			}
		})
	}
}

// A reply that cannot be read, or none, leaves the outcome unknown: a job
// may be queued (review 3c-2 C8).
func TestBusRestartUnknownOutcome(t *testing.T) {
	tests := []struct {
		name    string
		body    []any
		invalid bool
	}{
		{"a path outside the jobs", []any{dbus.ObjectPath("/org/freedesktop/systemd1/unit/x")}, true},
		{"job 0", []any{dbus.ObjectPath("/org/freedesktop/systemd1/job/0")}, true},
		{"a number, not a path", []any{uint32(1)}, true},
		{"no reply", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var args []any
			var mu sync.Mutex
			b := newTestBus(&peer{t: t, answer: restarting(tt.body, "", &args, &mu)})
			defer b.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			bounded(t, 2*time.Second, func() {
				_, err := b.Restart(ctx, nginx)
				if !errors.Is(err, ErrUnknownOutcome) {
					t.Fatalf("error = %v, want ErrUnknownOutcome", err)
				}
				if errors.Is(err, ErrInvalid) != tt.invalid {
					t.Errorf("error = %v, want ErrInvalid %v", err, tt.invalid)
				}
			})
		})
	}
}

// A peer that stops reading: the write deadline ends the call, whose
// outcome is unknown (it may have been read in part).
func TestBusRestartBlockedWrite(t *testing.T) {
	b := newTestBus(&peer{t: t, stopAfterAuth: true})
	defer b.Close()
	bounded(t, 2*time.Second, func() {
		if _, err := b.Restart(context.Background(), nginx); !errors.Is(err, ErrUnknownOutcome) {
			t.Errorf("error = %v, want ErrUnknownOutcome", err)
		}
	})
}

// Nothing is sent when systemd is not booted, the bus is closed or the
// context has ended already.
func TestBusRestartNothingSent(t *testing.T) {
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name   string
		booted bool
		closed bool
		ctx    context.Context
	}{
		{"not booted", false, false, context.Background()},
		{"closed", true, true, context.Background()},
		{"context ended", true, false, ended},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &peer{t: t}
			b := newTestBus(p)
			b.booted = func() bool { return tt.booted }
			if tt.closed {
				b.Close()
			}
			_, err := b.Restart(tt.ctx, nginx)
			if err == nil || errors.Is(err, ErrUnknownOutcome) {
				t.Fatalf("error = %v, want nothing sent", err)
			}
			if p.calls.Load() != 0 {
				t.Error("a call reached systemd")
			}
		})
	}
}

func TestJobID(t *testing.T) {
	tests := []struct {
		path string
		id   uint32
		ok   bool
	}{
		{"/org/freedesktop/systemd1/job/1", 1, true},
		{"/org/freedesktop/systemd1/job/4294967295", 4294967295, true},
		{"/org/freedesktop/systemd1/job/4294967296", 0, false},
		{"/org/freedesktop/systemd1/job/0", 0, false},
		{"/org/freedesktop/systemd1/job/01", 0, false},
		{"/org/freedesktop/systemd1/job/+1", 0, false},
		{"/org/freedesktop/systemd1/job/", 0, false},
		{"/org/freedesktop/systemd1/job/1/x", 0, false},
		{"/org/freedesktop/systemd1/unit/1", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			id, ok := jobID(dbus.ObjectPath(tt.path))
			if id != tt.id || ok != tt.ok {
				t.Errorf("jobID = %d, %v; want %d, %v", id, ok, tt.id, tt.ok)
			}
		})
	}
}

func FuzzJobID(f *testing.F) {
	f.Add("/org/freedesktop/systemd1/job/7")
	f.Add("/org/freedesktop/systemd1/job/007")
	f.Fuzz(func(t *testing.T, p string) {
		id, ok := jobID(dbus.ObjectPath(p))
		if !ok {
			return
		}
		if id == 0 || p != jobPrefix+itoa(id) {
			t.Fatalf("accepted %q as %d", p, id)
		}
	})
}

func itoa(n uint32) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

func TestBootID(t *testing.T) {
	id, err := BootID()
	if _, statErr := os.Stat(bootIDPath); statErr != nil {
		if err == nil {
			t.Fatalf("boot id %q without %s", id, bootIDPath)
		}
		return // not Linux
	}
	if err != nil || !bootRe.MatchString(id) {
		t.Fatalf("BootID = %q, %v", id, err)
	}
}
