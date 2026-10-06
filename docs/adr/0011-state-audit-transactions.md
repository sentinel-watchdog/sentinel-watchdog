# ADR-0011: State schema v2, audit log and firewall transactions

- Status: Accepted
- Date: 2026-10-06
- Phases: 3 (schema v2 + migration), 8 (audit log), 13 (transactions, backups)

## Context

The Phase 1 state file (`schema_version: 1`) holds supervisors and a bounded
event log. New domains need provider state, the pinned firewall backend,
managed resources, transaction IDs, last plan/apply/rollback, pending
confirmations, CrowdSec decision cursors and provenance, and an audit
trail. Secrets must never be persisted.

## Decision

### File layout (default `settings.state_dir` = `/var/lib/sentinel`)

```
/var/lib/sentinel/                 0750 root:root (existing)
  state.json                       0600 hot state, schema v2
  audit.jsonl                      0600 append-only, hash-chained; rotated audit.jsonl.1 … .N
  firewall/                        0700
    transactions/<tx-id>.json      0600 transaction metadata
    backups/<tx-id>.json           0600 snapshot of managed objects before the transaction
```

`settings.state_file` keeps working; `state_dir` defaults to its
directory (exact key decided in Phase 3, Q-010).

### state.json v2

```jsonc
{
  "schema_version": 2,
  "updated_at": "…",
  "supervisors": { … },                 // unchanged semantics
  "events": [ … ],                   // ADR-0002 model (source/source_type)
  "providers": {                     // last known status per provider
    "nftables": { "state": "ok", "effective_mode": "dry_run", "checked_at": "…", "last_error": "" }
  },
  "firewall": {
    "backend": "nftables",           // pinned by the first commit (ADR-0005)
    "table": "inet sentinel",
    "install_id": "…",
    "managed": { "policies": ["baseline"], "rules": ["allow-ssh-management"], "sets": ["bl_local-abuse_v4"] },
    "last_plan":     { "plan_id": "…", "fingerprint": "sha256:…", "created_at": "…" },
    "last_apply":    { "tx_id": "…", "at": "…", "status": "confirmed" },
    "last_rollback": { "tx_id": "…", "at": "…", "reason": "safety_timeout" },
    "pending":       { "tx_id": "…", "deadline": "…" }
  },
  "security": {
    "crowdsec": { "last_poll": "…", "stream_started": true, "active_decisions": 1234 }
  },
  "audit_head": { "seq": 4711, "hash": "sha256:…" }
}
```

- v1 files are migrated to v2 on load (supervisor names → `source`); newer
  versions are refused (D-015 unchanged).
- Individual decisions are **not** stored in state.json (could be
  100k entries); the set content is authoritative and the audit log holds
  the provenance. Only counters and cursors are kept.
- The specification's "limited audit events in state" is implemented as
  `audit_head` plus the bounded event log; the audit itself lives in
  `audit.jsonl`, read through the daemon (`sentinelctl audit`).

### Audit log (`internal/audit`)

Record (one JSON object per line):

```json
{"seq":4712,"time":"…","actor":{"kind":"cli","uid":0,"gid":0,"pid":1234},
 "action":"firewall.apply","target":"inet sentinel","tx_id":"tx-…","correlation_id":"tx-…",
 "effective_mode":"enforce","result":"intent|ok|failed|denied","detail":{…},
 "prev_hash":"sha256:…","hash":"sha256:…"}
```

- `hash` = SHA-256 over the canonical record without `hash`; `prev_hash`
  chains records across rotations. `audit_head` in state.json lets the
  daemon detect truncation or replacement at start-up (`audit_integrity:
  broken` in status + critical event; Sentinel keeps running and never
  "repairs" the chain silently).
- Tamper-*evident*, not tamper-proof: root can rewrite everything.
  Off-host copies come from journald (each audit record is also logged at
  `info`) and optional webhook routing of audit events.
- Writes: `O_APPEND|O_WRONLY|O_CREAT|O_NOFOLLOW`, `fsync` per record for
  enforcement actions. Size-based rotation (default 10 MiB × 5).
- **Fail closed**: an enforcement operation writes an `intent` record
  first; if that fails, the operation is aborted. A failed `result` record
  after commit marks the daemon `degraded` and emits a critical event (the
  change is not rolled back, because rollback would be another unaudited
  change).
- Audited: every firewall/blocklist/Kubernetes change and dry-run, every
  `operate`/`admin` control-plane request including denials (ADR-0012),
  configuration reloads, backend pin changes.

### Transactions

- ID: `tx-<UTC yyyymmddThhmmssZ>-<12 hex random>` (sortable, unguessable
  enough to avoid collisions; not a secret).
- Metadata: id, created_at, actor, backend, plan_id, fingerprint before and
  after, abstract changes, rendered payload SHA-256, backup file SHA-256,
  status (`planned | dry_run | committed | pending_confirmation |
  confirmed | rolled_back | failed | rollback_failed`), previous tx id,
  TTL expiries.
- A single firewall lock (in-process mutex + `flock` on
  `/run/sentinel/firewall.lock`) serialises plan-apply-verify-rollback.
- On start-up: a `pending_confirmation` transaction past its deadline is
  rolled back before anything else; one still within its deadline gets its
  timer re-armed.
- Restore validates the backup hash and that the backup touches only owned
  objects before using it.
- Retention: last 50 transactions plus any referenced by `pending`,
  `last_apply`, `last_rollback`.

### File-system hardening (applies to all state files)

- Open the state directory with `os.OpenRoot` and do all file operations
  relative to it (no symlink escape, no `..`).
- At start-up refuse to run enforcement if the state directory or its
  subdirectories are group/world-writable or not owned by the daemon's
  uid; read-only observation continues with a critical event.

## Consequences

- Phase 3 bumps `state.SchemaVersion` to 2 with migration tests.
- Phase 8 adds `internal/audit`; Phase 13 adds `internal/firewall/transaction`.
- The audit format is part of the external contract (documented in
  docs/security.md).
