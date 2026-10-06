# ADR-0002: Unified event model and in-process event bus

- Status: Accepted, amended by ADR-0015
- Date: 2026-10-06
- Phases: 3 (model + bus); later phases add event types

## Context

`pkg/model.Event` (Phase 1) is supervisor-centric: `supervisor_name` and
`supervisor_type` identify the emitter, `failure_count` / `restart_count` /
`last_error` are top-level fields, and `EventScope` knows only `supervisor`,
`job` and `daemon`. `state.File.AppendEvent` keys history on
`SupervisorName`.

Events must now come from supervisors, cron jobs, the firewall, blocklists,
CrowdSec, container runtimes, Kubernetes and WAF providers, and be consumed
by the state store, the webhook notifier, the recovery engine, the audit
log and a future metrics exporter. Deduplication, cooldown and correlation
IDs are required.

Nothing has been released, so the webhook payload can still change without
a compatibility burden. After v0.1.0 it cannot.

## Decision

### Model (replaces the Phase 1 struct in Phase 3)

```go
type Event struct {
    ID              string            `json:"event_id"`
    Timestamp       time.Time         `json:"timestamp"`
    Hostname        string            `json:"hostname"`
    SentinelVersion string            `json:"sentinel_version"`
    Source          string            `json:"source"`          // supervisor name, provider name, "daemon"
    SourceType      SourceType        `json:"source_type"`     // systemd|process|http|cron|firewall|blocklist|crowdsec|docker|podman|kubernetes|waf|daemon
    Type            EventType         `json:"event_type"`
    Severity        Severity          `json:"severity"`        // info|warning|error|critical
    State           string            `json:"state,omitempty"` // validated against the source type's state enum
    PreviousState   string            `json:"previous_state,omitempty"`
    Message         string            `json:"message"`
    CorrelationID   string            `json:"correlation_id,omitempty"`
    Metadata        map[string]string `json:"metadata,omitempty"`   // small string labels, used for routing/filters
    Attributes      map[string]any    `json:"attributes,omitempty"` // structured, typed details
}
```

- `supervisor_name`/`supervisor_type` become `source`/`source_type`.
- `failure_count`, `restart_count`, `last_error`, `exit_code` move to
  `attributes` with documented keys per source type.
- `State` is a string so providers can use `model.ProviderState`
  (ADR-0004) while supervisors keep `model.State`; `Validate` checks it
  against the enum of `SourceType`.
- `Attributes` is restricted at the bus boundary: keys match
  `^[a-z0-9_.]{1,64}$`; values are JSON scalars, `[]string` or one nested
  level of the same; the encoded event is capped (default 16 KiB, larger
  attributes are truncated with `attributes_truncated: true`). Values from
  external systems (CrowdSec scenarios, container labels, Kubernetes
  annotations) are stripped of control characters.
- Severity is set by the emitter from a per-event-type default table;
  configuration cannot lower `critical`.
- `EventScope` gains `provider`, `firewall`, `blocklist`, `security`,
  `container`, `kubernetes`. New event types are registered in
  `pkg/model` in the phase that emits them; unknown types fail
  validation (no free-form event types).

### Correlation

`CorrelationID` groups the events of one incident or operation:

| Flow | Correlation ID |
|---|---|
| supervisor failure → recovery attempts → exhausted/recovered | ID of the event that entered `failed` |
| firewall plan → apply → verify → confirm/rollback | transaction ID |
| CrowdSec decision → blocklist update → firewall set update | `crowdsec:<decision id>` |
| recovery action triggered by another event | the triggering event's correlation ID |

### Bus

`internal/events` provides an in-process bus:

- `Publish(ctx, Event)` never blocks the emitter: each subscriber has a
  bounded queue and an overflow policy (`drop_oldest` + counter +
  rate-limited `daemon_error`).
- Subscribers: state store, notification dispatcher, recovery engine,
  audit recorder (non-enforcement events), metrics (later).
- Ordering is guaranteed per source, not globally.
- **Not used for enforcement-critical writes**: firewall transactions
  write their audit records synchronously through `internal/audit`
  (ADR-0011) and abort if that write fails. The bus is for observation and
  notification, never the only record of a security change.

### Deduplication and cooldown

- **Dedup key** = `source_type/source/event_type/fingerprint`, where the
  fingerprint is a hash of the attribute keys the event type declares as
  identifying (e.g. decision value for CrowdSec, rule ID for firewall
  drift).
- **State-based suppression** for supervisors (as already planned): an
  identical event is not re-emitted while the condition persists.
- **Time-based suppression** for providers: `dedup_window` per event type
  (default 5m); suppressed occurrences are counted and reported in the
  next emitted event (`attributes.suppressed_count`).
- **Notification repeat interval**: per (channel, dedup key) minimum
  interval between deliveries. It is named `repeat_interval`, not
  `cooldown`, because `recovery.cooldown` already means something else
  (D-013).

## Consequences

- Phase 3 rewrites `pkg/model.Event`, the state history summary and the
  webhook payload documentation; all tests move to the new fields.
- The state file schema moves to v2 with a v1→v2 migration (ADR-0011).
- Webhook consumers get one schema for all domains and can filter on
  `source_type`, `event_type`, `severity`.

## Follow-ups

- Phase 3: model, validation, bus, dedup, repeat interval, migration.
- Each later phase registers its event types and documents their attributes.
