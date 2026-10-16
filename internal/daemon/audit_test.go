package daemon

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/audit"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/clock"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/state"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/api"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/model"
)

func (h *harness) coreEvents(t model.EventType) []model.Event {
	var out []model.Event
	for _, p := range h.ops.delivered() {
		if p.Event.Type == t {
			out = append(out, p.Event)
		}
	}
	return out
}

// waitEvents waits for n delivered events of type typ.
func (h *harness) waitEvents(t *testing.T, typ model.EventType, n int) []model.Event {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		evs := h.coreEvents(typ)
		if len(evs) >= n || time.Now().After(deadline) {
			return evs
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func uid32(v uint32) *uint32 { return &v }

func operatorEntry(result audit.Result) audit.Entry {
	return audit.Entry{Actor: audit.Actor{Kind: audit.ActorSocket, UID: uid32(1000), GID: uid32(1000), PID: 7},
		Action: "core.reload", Result: result, CorrelationID: "c1"}
}

// core.audit returns the last records, as the control socket wrote them
// through the daemon's recorder, and status shows the chain's integrity.
func TestControlAudit(t *testing.T) {
	h, _ := controlHarness(t)
	rec := &auditRecorder{d: h.d}
	for _, r := range []audit.Result{audit.ResultDenied, audit.ResultIntent, audit.ResultOK} {
		if _, err := rec.Record(context.Background(), operatorEntry(r)); err != nil {
			t.Fatal(err)
		}
	}
	resp := h.call(t, api.CoreAudit.Name, `{"limit":2}`)
	if resp.Error != nil {
		t.Fatal(resp.Error)
	}
	var got api.AuditResult
	if err := json.Unmarshal(resp.Result, &got); err != nil {
		t.Fatal(err)
	}
	if got.Audit.Integrity != audit.StateOK || got.Audit.Seq != 3 || len(got.Records) != 2 {
		t.Fatalf("result %+v", got)
	}
	r := got.Records[1]
	if r.Seq != 3 || r.Result != "ok" || r.Action != "core.reload" || *r.Actor.UID != 1000 || r.Actor.PID != 7 ||
		r.PrevHash != got.Records[0].Hash {
		t.Errorf("record %+v", r)
	}
	var st api.Status
	if err := json.Unmarshal(h.call(t, api.CoreStatus.Name, "").Result, &st); err != nil {
		t.Fatal(err)
	}
	if st.Audit.Integrity != audit.StateOK || st.Audit.Seq != 3 {
		t.Errorf("status audit %+v", st.Audit)
	}
	if resp := h.call(t, api.CoreAudit.Name, `{}`); resp.Error != nil {
		t.Errorf("default limit: %+v", resp.Error)
	}
	for _, args := range []string{`{"limit":0}`, `{"limit":-1}`, `{"limit":1001}`, `{"lines":5}`, `{"limit":"5"}`} {
		if resp := h.call(t, api.CoreAudit.Name, args); resp.Error == nil || resp.Error.Code != api.CodeBadRequest {
			t.Errorf("args %s: %+v", args, resp.Error)
		}
	}
}

// A chain found broken at start-up does not stop sentineld: status says
// so and audit_integrity_broken is published.
func TestBrokenChainAtStart(t *testing.T) {
	h := newHarness(t)
	if err := os.MkdirAll(h.cfg.Daemon.StateDir, 0o750); err != nil {
		t.Fatal(err)
	}
	// A head that knows records which are gone: truncation.
	head := `{"version":1,"last":{"seq":5,"hash":"sha256:` + zeros + `"},"anchor":{"seq":0,"hash":"sha256:` + zeros + `"}}`
	if err := os.WriteFile(filepath.Join(h.cfg.Daemon.StateDir, "audit_head.json"), []byte(head), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.d.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.d.Stop(context.Background()) })
	if st := h.d.status(); st.Audit.Integrity != audit.StateBroken || st.Audit.Reason != audit.ReasonHeadAhead {
		t.Errorf("status audit %+v", st.Audit)
	}
	evs := h.waitEvents(t, model.EventAuditIntegrityBroken, 1)
	if len(evs) != 1 || evs[0].Severity != model.SeverityCritical || evs[0].Attributes["reason"] != audit.ReasonHeadAhead {
		t.Errorf("events %+v", evs)
	}
	if _, err := (&auditRecorder{d: h.d}).Record(context.Background(), operatorEntry(audit.ResultIntent)); err == nil {
		t.Error("a record was written to a broken chain")
	}
}

const zeros = "0000000000000000000000000000000000000000000000000000000000000000"

// A record that cannot be written stops the audit log and publishes
// audit_write_failed once.
func TestAuditWriteFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	h, _ := controlHarness(t)
	rec := &auditRecorder{d: h.d}
	if err := os.Chmod(h.cfg.Daemon.StateDir, 0o550); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(h.cfg.Daemon.StateDir, 0o750) }) // before the daemon's own cleanup
	for range 2 {
		if _, err := rec.Record(context.Background(), operatorEntry(audit.ResultIntent)); err == nil {
			t.Fatal("record written to a read-only directory")
		}
	}
	if st := h.d.status(); st.Audit.Reason != audit.ReasonWriteFailed {
		t.Errorf("status audit %+v", st.Audit)
	}
	time.Sleep(50 * time.Millisecond) // a second event would arrive meanwhile
	if evs := h.waitEvents(t, model.EventAuditWriteFailed, 1); len(evs) != 1 {
		t.Errorf("audit_write_failed events: %d", len(evs))
	}
}

// Review 2c-3a R1: an outcome that cannot be recorded stops the audit log
// and publishes audit_write_failed once.
func TestLostOutcomeIsReported(t *testing.T) {
	h, _ := controlHarness(t)
	rec := &auditRecorder{d: h.d}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for range 2 {
		if _, err := rec.Record(ctx, operatorEntry(audit.ResultOK)); err == nil {
			t.Fatal("recorded with an ended context")
		}
	}
	if st := h.d.status(); st.Audit.Integrity != audit.StateBroken || st.Audit.Reason != audit.ReasonWriteFailed {
		t.Errorf("status audit %+v", st.Audit)
	}
	time.Sleep(50 * time.Millisecond) // a second event would arrive meanwhile
	if evs := h.waitEvents(t, model.EventAuditWriteFailed, 1); len(evs) != 1 {
		t.Errorf("audit_write_failed events: %d", len(evs))
	}
}

// Review 2c-3a R6: a second sentineld stops at the socket's lock, before
// the audit check, which could otherwise update the head that the running
// daemon writes.
func TestRejectedStartLeavesTheAuditHeadAlone(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	dir, err := state.OpenDir(h.cfg.Daemon.StateDir, h.d.clock)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	l, err := openAudit(dir, h.d)
	if err != nil {
		t.Fatal(err)
	}
	headPath := filepath.Join(h.cfg.Daemon.StateDir, "audit_head.json")
	before, err := os.ReadFile(headPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Record(ctx, operatorEntry(audit.ResultIntent)); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(ctx); err != nil {
		t.Fatal(err)
	}
	// A durable record whose head is not saved yet: the check would update it.
	if err := os.WriteFile(headPath, before, 0o600); err != nil {
		t.Fatal(err)
	}
	server, err := h.d.listen() // the running daemon holds the socket
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close(ctx)
	if err := h.d.Start(ctx); err == nil {
		t.Fatal("a second daemon started")
	}
	if after, err := os.ReadFile(headPath); err != nil || !bytes.Equal(before, after) {
		t.Fatalf("the rejected start changed the audit head (%v)", err)
	}
}

// Review 2c-3a R7: a stopped daemon keeps its lock until Stop returns, and
// its audit log refuses records from then on, even if Stop gave up early;
// so a new daemon never writes beside it.
func TestStoppedAuditCannotWriteBesideANewDaemon(t *testing.T) {
	h := newHarness(t)
	bg := context.Background()
	if err := h.d.Start(bg); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(bg)
	cancel()
	_ = h.d.Stop(ctx) // an interrupted shutdown
	next := New(h.cfg, nil, h.d.opts)
	if err := next.Start(bg); err != nil {
		t.Fatalf("the next daemon did not start after Stop returned: %v", err)
	}
	t.Cleanup(func() { _ = next.Stop(bg) })
	if _, err := h.d.audit.Record(bg, operatorEntry(audit.ResultOK)); !errors.Is(err, audit.ErrClosed) {
		t.Fatalf("the old audit log is still writable beside the new daemon: %v", err)
	}
}

// pausedAuditClock pauses the first call of Now once armed: Record calls
// it after checking that the log is open, so a record is "being written".
type pausedAuditClock struct {
	clock.Clock
	armed   atomic.Bool
	entered chan struct{}
	resume  chan struct{}
}

func (c *pausedAuditClock) Now() time.Time {
	if c.armed.CompareAndSwap(true, false) {
		close(c.entered)
		<-c.resume
	}
	return c.Clock.Now()
}

// Review 2c-3a R7 (fourth review): when Stop gives up waiting for a record
// being written, it keeps the socket's lock, so a new daemon cannot start
// beside that writer.
func TestStopKeepsTheLockForAWriterInProgress(t *testing.T) {
	h := newHarness(t)
	bg := context.Background()
	c := &pausedAuditClock{Clock: h.d.clock, entered: make(chan struct{}), resume: make(chan struct{})}
	h.d.clock = c
	if err := h.d.Start(bg); err != nil {
		t.Fatal(err)
	}
	c.armed.Store(true)
	done := make(chan error, 1)
	go func() {
		_, err := h.d.audit.Record(bg, operatorEntry(audit.ResultOK))
		done <- err
	}()
	<-c.entered
	ctx, cancel := context.WithCancel(bg)
	cancel()
	if err := h.d.Stop(ctx); err == nil {
		t.Error("Stop reported a clean stop with a record still being written")
	}
	next := New(h.cfg, nil, h.d.opts)
	err := next.Start(bg)
	close(c.resume)
	<-done
	if err == nil {
		_ = next.Stop(bg)
		t.Fatal("a new daemon started beside an audit record still being written")
	}
	h.d.server.Release() // the process exit, in production
}

// Review 2c-3a R7 (fifth review): a second Stop does nothing, so it never
// releases the lock the first one kept for a writer still in progress.
func TestRepeatedStopKeepsTheLockForAWriterInProgress(t *testing.T) {
	h := newHarness(t)
	bg := context.Background()
	c := &pausedAuditClock{Clock: h.d.clock, entered: make(chan struct{}), resume: make(chan struct{})}
	h.d.clock = c
	if err := h.d.Start(bg); err != nil {
		t.Fatal(err)
	}
	c.armed.Store(true)
	done := make(chan error, 1)
	go func() {
		_, err := h.d.audit.Record(bg, operatorEntry(audit.ResultOK))
		done <- err
	}()
	<-c.entered
	ctx, cancel := context.WithCancel(bg)
	cancel()
	_ = h.d.Stop(ctx)
	if err := h.d.Stop(ctx); err != nil {
		t.Errorf("second Stop: %v", err)
	}
	next := New(h.cfg, nil, h.d.opts)
	err := next.Start(bg)
	close(c.resume)
	<-done
	if err == nil {
		_ = next.Stop(bg)
		t.Fatal("a second Stop released the lock beside a writer in progress")
	}
	h.d.server.Release() // the process exit, in production
}

// Review 2c-3a R9: an intent being written when another outcome is lost
// does not let its command run.
func TestInFlightIntentFailsAfterALostOutcome(t *testing.T) {
	h := newHarness(t)
	bg := context.Background()
	c := &pausedAuditClock{Clock: h.d.clock, entered: make(chan struct{}), resume: make(chan struct{})}
	h.d.clock = c
	if err := h.d.Start(bg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.d.Stop(bg) })
	if _, err := h.d.audit.Record(bg, operatorEntry(audit.ResultIntent)); err != nil {
		t.Fatal(err)
	}
	c.armed.Store(true) // B stops after its checks, before writing
	done := make(chan error, 1)
	go func() {
		e := operatorEntry(audit.ResultIntent)
		e.CorrelationID = "c2"
		_, err := h.d.audit.Record(bg, e)
		done <- err
	}()
	<-c.entered
	ctx, cancel := context.WithCancel(bg)
	cancel()
	_, outcomeErr := h.d.audit.Record(ctx, operatorEntry(audit.ResultOK)) // A's outcome is lost
	close(c.resume)
	if intentErr := <-done; !errors.Is(intentErr, audit.ErrBroken) {
		t.Fatalf("in-flight intent allowed its command: %v (outcome: %v)", intentErr, outcomeErr)
	}
}

// Design review 2026-10-16 Q1: a reload by SIGHUP whose intent cannot be
// written stops the audit log and publishes audit_write_failed, as a
// request through the socket does.
func TestReloadIntentWriteFailureIsReported(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	h := newReloadHarness(t, "", "", nil)
	stateDir := filepath.Join(h.dir, "state")
	if err := os.Chmod(stateDir, 0o550); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(stateDir, 0o750) }) // before the daemon's own cleanup
	if _, apiErr := h.reload(t); apiErr == nil || apiErr.Code != api.CodeUnavailable {
		t.Fatalf("reload with an unwritable audit log: %+v", apiErr)
	}
	if evs := h.waitDelivered(t, model.EventAuditWriteFailed); len(evs) != 1 {
		t.Errorf("audit_write_failed events: %d", len(evs))
	}
}

// Design review 2026-10-16 Q1: one write failure is published once, even
// when a reload's outcome finds it first and the control socket's next
// record finds the log broken.
func TestAuditWriteFailureIsReportedOnceAcrossPaths(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	h := newReloadHarness(t, "", "", nil)
	stateDir := filepath.Join(h.dir, "state")
	if err := os.Chmod(stateDir, 0o550); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(stateDir, 0o750) })
	h.d.outcome(context.Background(), audit.Actor{Kind: audit.ActorSignal}, "c1", audit.ResultOK, nil)
	if resp := h.call(t, api.CoreReload.Name, ""); resp.Error == nil || resp.Error.Code != api.CodeUnavailable {
		t.Fatalf("reload on a broken audit log: %+v", resp.Error)
	}
	time.Sleep(50 * time.Millisecond) // a second event would arrive meanwhile
	if evs := h.waitDelivered(t, model.EventAuditWriteFailed); len(evs) != 1 {
		t.Errorf("audit_write_failed events: %d", len(evs))
	}
}
