package daemon

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/audit"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/authz"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/state"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/api"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/model"
)

// openAudit opens the audit log in the state directory and checks its
// chain. A broken chain does not stop sentineld (ADR-0011): it is reported
// once the bus runs (reportAudit), and operate and admin requests are
// refused.
func openAudit(dir *state.Dir, d *Daemon) (*audit.Log, error) {
	root, err := dir.OpenRoot()
	if err != nil {
		return nil, err
	}
	l, err := audit.Open(root, audit.Options{Clock: d.clock, Logger: d.log})
	if err != nil {
		_ = root.Close() // the open error is the one to report
		return nil, err
	}
	return l, nil
}

// reportAudit publishes audit_integrity_broken when the chain failed its
// check at start-up.
func (d *Daemon) reportAudit() {
	i := d.audit.Integrity()
	if i.State != audit.StateBroken {
		return
	}
	d.publishCore(model.EventAuditIntegrityBroken,
		"audit chain broken ("+i.Reason+"): operate and admin requests are refused until the evidence is restored",
		map[string]any{"reason": i.Reason})
}

// auditRecorder is the audit log as the control socket sees it. When a record
// cannot be written the log stops recording (the end of the chain is
// uncertain); the first such failure is published as audit_write_failed.
type auditRecorder struct {
	d        *Daemon
	reported atomic.Bool
}

func (r *auditRecorder) Record(ctx context.Context, e audit.Entry) (audit.Record, error) {
	rec, err := r.d.audit.Record(ctx, e)
	if err != nil && r.d.audit.Integrity().Reason == audit.ReasonWriteFailed && r.reported.CompareAndSwap(false, true) {
		r.d.publishCore(model.EventAuditWriteFailed,
			"audit record not written: operate and admin requests are refused until sentineld restarts", nil)
	}
	return rec, err
}

// publishCore publishes an event of the core about the daemon itself.
func (d *Daemon) publishCore(t model.EventType, message string, attrs map[string]any) {
	_, err := d.bus.Publish(model.Event{Module: model.ModuleCore, Type: t, Source: "sentineld",
		SourceType: model.SourceTypeDaemon, Message: message, Attributes: attrs})
	if err != nil {
		d.log.Error("core event not published", "event_type", t, "error", err)
	}
}

// auditStatus is the integrity of the chain, for core.status and core.audit;
// empty before Start.
func (d *Daemon) auditStatus() api.AuditStatus {
	if d.audit == nil {
		return api.AuditStatus{}
	}
	i := d.audit.Integrity()
	return api.AuditStatus{Integrity: i.State, Reason: i.Reason, Where: i.Where, Seq: i.Seq}
}

// auditTail is core.audit: the last records and the integrity of the chain.
func (d *Daemon) auditTail(ctx context.Context, _ authz.Peer, args jsontext.Value) (any, *api.Error) {
	var a api.AuditArgs
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a, json.RejectUnknownMembers(true)); err != nil {
			return nil, &api.Error{Code: api.CodeBadRequest, Message: "invalid arguments: " + err.Error()}
		}
	}
	limit := api.DefaultAuditLimit
	if a.Limit != nil {
		limit = *a.Limit
	}
	if limit < 1 || limit > api.MaxAuditLimit {
		return nil, &api.Error{Code: api.CodeBadRequest, Message: fmt.Sprintf("limit must be 1 to %d", api.MaxAuditLimit)}
	}
	recs, skipped, err := d.audit.Tail(ctx, limit)
	if err != nil {
		d.log.Error("core.audit: audit log not read", "error", err)
		code := api.CodeInternal
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			code = api.CodeUnavailable
		}
		return nil, &api.Error{Code: code, Message: "the audit log could not be read"}
	}
	out := api.AuditResult{Audit: d.auditStatus(), Records: make([]api.AuditRecord, 0, len(recs)), Skipped: skipped}
	for _, r := range recs {
		out.Records = append(out.Records, api.AuditRecord{
			Seq: r.Seq, Time: r.Time, Action: r.Action, Target: r.Target, Result: string(r.Result),
			Actor:         api.AuditActor{Kind: string(r.Actor.Kind), UID: r.Actor.UID, GID: r.Actor.GID, PID: r.Actor.PID},
			CorrelationID: r.CorrelationID, Detail: r.Detail, PrevHash: r.PrevHash, Hash: r.Hash,
		})
	}
	return out, nil
}
