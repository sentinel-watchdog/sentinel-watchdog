package supervisor

import (
	"context"
	"encoding/json/v2"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/api"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/model"
)

const webService = "version: 1\nservices:\n  - name: web\n    url: \"https://admin:pw-SECRET@web.example.org:8443/hook/TOKEN?key=QSECRET\"\n" +
	"    check_interval: 10s\n    failure_policy: {consecutive_failures: 2, consecutive_successes: 2}\n    notifications: [ops]\n"

// tick lets one check_interval pass and waits until the loops are idle.
func tick(d time.Duration) {
	time.Sleep(d)
	synctest.Wait()
}

func statusOf(t *testing.T, in *instance, name string) api.ServiceStatus {
	t.Helper()
	st, apiErr := in.status(name)
	if apiErr != nil || len(st.Services) != 1 {
		t.Fatalf("status %s: %+v %v", name, st, apiErr)
	}
	return st.Services[0]
}

// AC3: service_failed once after N failures, service_recovered once after
// M successes, correlated by the incident; nothing on the first success.
func TestFailAndRecover(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fs := newFakes()
		in, err := configure(t, map[string]string{"supervisor/a.yaml": webService}, fs.build)
		if err != nil {
			t.Fatal(err)
		}
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait() // first check: a success
		if s := statusOf(t, in, "web"); s.State != "healthy" || s.Checks != 1 || len(r.drain()) != 0 {
			t.Fatalf("after the first success: %+v", s)
		}
		fs.get("web").set(failing)
		tick(10 * time.Second) // failure 1
		if len(r.drain()) != 0 {
			t.Fatal("an event before the threshold")
		}
		tick(10 * time.Second) // failure 2: failed
		tick(10 * time.Second) // failure 3: still failed, no event
		evs := r.drain()
		if len(evs) != 1 || evs[0].Type != EventServiceFailed || evs[0].Severity != model.SeverityError ||
			evs[0].ID != evs[0].CorrelationID || evs[0].Attributes["failure"] != string(FailureCheck) ||
			evs[0].Attributes["endpoint"] != "https://web.example.org:8443" || evs[0].Source != "web" ||
			evs[0].PreviousState != "healthy" {
			t.Fatalf("failed events: %+v", evs)
		}
		incident := evs[0].ID
		if s := statusOf(t, in, "web"); s.State != "failed" || s.IncidentID != incident || s.ConsecutiveFailures != 3 {
			t.Errorf("status while failed: %+v", s)
		}
		fs.get("web").set(nil)
		tick(10 * time.Second) // success 1
		tick(10 * time.Second) // success 2: recovered
		tick(10 * time.Second)
		evs = r.drain()
		if len(evs) != 1 || evs[0].Type != EventServiceRecovered || evs[0].CorrelationID != incident ||
			evs[0].Attributes["downtime_seconds"] != int64(30) {
			t.Fatalf("recovered events: %+v", evs)
		}
		if s := statusOf(t, in, "web"); s.State != "healthy" || s.IncidentID != "" || s.Failures != 3 || s.Checks != 7 {
			t.Errorf("status after recovery: %+v", s)
		}
	})
}

// AC3: a check cancelled by Stop is not a failure.
func TestShutdownIsNotAFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fs := newFakes()
		in, err := configure(t, map[string]string{"supervisor/a.yaml": webService}, fs.build)
		if err != nil {
			t.Fatal(err)
		}
		fs.get("web").set(func(ctx context.Context) Result { <-ctx.Done(); return Result{Failure: FailureCheck} })
		r := start(t, in, "")
		synctest.Wait()
		r.stop(t)
		if s := statusOf(t, in, "web"); s.Checks != 0 || s.State != "unknown" || len(r.drain()) != 0 {
			t.Errorf("after stop: %+v", s)
		}
	})
}

// AC4: at most max_concurrent_checks checks run at once; the others wait
// for a slot, which status shows and which is not counted in a check's
// duration.
func TestConcurrencyLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var b strings.Builder
		b.WriteString("version: 1\nsettings: {max_concurrent_checks: 4}\nservices:\n")
		for i := range 20 {
			b.WriteString("  - {name: s" + string(rune('a'+i)) + ", url: \"https://h/\"}\n")
		}
		release := make(chan struct{})
		var active, peak atomic.Int64
		check := func(ctx context.Context) Result {
			n := active.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			defer active.Add(-1)
			select {
			case <-release:
				time.Sleep(time.Second) // a check that takes a second once it runs
			case <-ctx.Done():
			}
			return Result{}
		}
		in, err := configure(t, map[string]string{"supervisor/a.yaml": b.String()},
			func(Service) (Checker, error) { return &fake{check: check}, nil })
		if err != nil {
			t.Fatal(err)
		}
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		st, _ := in.status("")
		waiting := 0
		for _, s := range st.Services {
			if !s.WaitingSince.IsZero() {
				waiting++
			}
		}
		if st.ChecksInFlight != 4 || waiting != 16 {
			t.Fatalf("in flight %d, waiting %d", st.ChecksInFlight, waiting)
		}
		time.Sleep(3 * time.Second)
		close(release)
		synctest.Wait()
		tick(5 * time.Second) // every service has had its first check
		st, _ = in.status("")
		for _, s := range st.Services {
			// The first four ran from the start (held 3 s, then 1 s); the
			// others waited for a slot, which their duration leaves out.
			first := s.LastSlotWaitMS == 0 && s.LastDurationMS == 4000
			queued := s.LastSlotWaitMS >= 4000 && s.LastDurationMS == 1000
			if s.Checks < 1 || (!first && !queued) {
				t.Errorf("%s: checks %d, last duration %d ms, slot wait %d ms", s.Name, s.Checks, s.LastDurationMS, s.LastSlotWaitMS)
			}
		}
		if peak.Load() != 4 {
			t.Errorf("peak %d checks at once, want 4", peak.Load())
		}
	})
}

// AC4, design C: a check that ignores its cancellation times out, counts
// as failed every interval without a new call, keeps its slot, and keeps
// Stop waiting: Stop never claims a clean stop while it runs.
func TestStalledCheck(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fs := newFakes()
		cfg := "version: 1\nservices:\n  - {name: web, url: \"https://web/\", timeout: 1s, check_interval: 5s, failure_policy: {consecutive_failures: 2}}\n"
		in, err := configure(t, map[string]string{"supervisor/a.yaml": cfg}, fs.build)
		if err != nil {
			t.Fatal(err)
		}
		hold := make(chan struct{})
		fs.get("web").set(func(context.Context) Result { <-hold; return Result{} })
		r := start(t, in, "")
		tick(time.Second) // timeout 1
		if s := statusOf(t, in, "web"); s.LastFailure != "timeout" || !s.Stalled || s.ConsecutiveFailures != 1 {
			t.Fatalf("after the timeout: %+v", s)
		}
		tick(5 * time.Second) // still stalled: timeout 2, failed
		evs := r.drain()
		if len(evs) != 1 || evs[0].Type != EventServiceFailed || evs[0].Attributes["failure"] != "timeout" {
			t.Fatalf("events %+v", evs)
		}
		if n := fs.get("web").count(); n != 1 {
			t.Errorf("%d calls of a stalled check, want 1", n)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := in.Stop(ctx); err == nil {
			t.Error("Stop claimed a clean stop with a check still running")
		}
		if !strings.Contains(r.logs.String(), "checks still running") {
			t.Errorf("logs: %s", r.logs)
		}
		close(hold) // the check returns: the bubble can end
	})
}

// A panicking checker and a checker returning free text are recorded with
// fixed codes; neither value reaches logs or status.
func TestCheckerMisbehaviour(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fs := newFakes()
		cfg := "version: 1\nservices:\n  - {name: boom, url: \"https://b/\"}\n  - {name: chatty, url: \"https://c/\"}\n"
		in, err := configure(t, map[string]string{"supervisor/a.yaml": cfg}, fs.build)
		if err != nil {
			t.Fatal(err)
		}
		fs.get("boom").set(func(context.Context) Result { panic("dial https://user:SECRET@b/ failed") })
		fs.get("chatty").set(func(context.Context) Result { return Result{Failure: "connect https://c/?k=SECRET: refused"} })
		r := start(t, in, "")
		defer r.stop(t)
		synctest.Wait()
		if s := statusOf(t, in, "boom"); s.LastFailure != "panic" {
			t.Errorf("boom: %+v", s)
		}
		if s := statusOf(t, in, "chatty"); s.LastFailure != "invalid_result" {
			t.Errorf("chatty: %+v", s)
		}
		st, _ := in.status("")
		out, _ := json.Marshal(st)
		if strings.Contains(r.logs.String()+string(out), "SECRET") {
			t.Errorf("a checker's text leaked:\n%s\n%s", r.logs, out)
		}
	})
}
