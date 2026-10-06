# Architecture

Status legend: ✅ implemented · 🚧 planned (phase in [PLAN.md](../PLAN.md)).

## Components

```
             ┌──────────────┐  Unix socket (JSON, versioned)  ┌──────────────┐
             │ sentinelctl  │ ───────────────────────────────▶│  sentineld   │
             └──────────────┘          🚧 Phase 5             └──────┬───────┘
                                                                     │
  ┌──────────── internal/daemon (orchestration, reload) 🚧 ──────────┤
  │                                                                  │
  │  config ✅ ──▶ monitors 🚧 ──Result──▶ recovery 🚧 ──Event──▶ events 🚧 ──▶ notification 🚧
  │                 │  ▲                     │                           │
  │                 ▼  │                     ▼                           ▼
  │            executor 🚧            systemctl / process          state ✅ (JSON)
  │            scheduler (cronexpr ✅ parse, runner 🚧)
  └──────────── logging ✅ · redact ✅ · lifecycle 🚧 · privilege 🚧
```

| Package | Responsibility | Status |
|---|---|---|
| `internal/config` | YAML model, file discovery, env expansion, strict decoding, defaults, validation, redacted view | ✅ |
| `internal/logging` | slog logger: text/json/journal/auto, attribute redaction | ✅ |
| `internal/redact` | Secret masking for headers, URLs, env, free text | ✅ |
| `internal/state` | State model, retention, atomic JSON store, corruption recovery | ✅ |
| `internal/scheduler/cronexpr` | Cron expression parser | ✅ parse · 🚧 `Next()` (Phase 4) |
| `internal/version` | Build metadata via `-ldflags -X` | ✅ |
| `pkg/model` | Public types: monitor types, states, capability statuses, events | ✅ |
| `internal/events` | Event bus and de-duplication | 🚧 Phase 2 |
| `internal/recovery` | Shared recovery engine | 🚧 Phase 2 |
| `internal/notification` | Provider interface, webhook | 🚧 Phase 2 |
| `internal/executor`, `internal/privilege` | Process spawning, credentials, output sinks | 🚧 Phase 3 |
| `internal/monitor/*` | systemd, process, http, cron | 🚧 Phases 3–4 |
| `internal/health` | `/proc` resource sampling | 🚧 Phase 3 |
| `internal/daemon`, `internal/lifecycle`, `internal/transport`, `pkg/api` | Daemon, signals, socket server, protocol | 🚧 Phase 5 |
| `cmd/sentineld`, `cmd/sentinelctl` | Binaries | 🚧 Phase 5 |

Dependency rule: `pkg/model` depends on nothing internal; `internal/*`
packages depend on `pkg/model` and on lower-level internal packages
(`redact`, `config`, `state`) but never on `daemon`. Interfaces are defined
by their consumer.

External dependencies: `go.yaml.in/yaml/v3` only.

## Configuration pipeline ✅

```
files (main + conf.d sorted) ─▶ read (≤4 MiB, single document)
  ─▶ yaml.Node ─▶ ${VAR} expansion on scalar values
  ─▶ unknown-key check (reflection over yaml tags, line numbers)
  ─▶ decode (Monitor dispatches on `type`)
  ─▶ merge (duplicate names across files, settings only in main)
  ─▶ defaults ─▶ validation (all problems collected) ─▶ *Config + warnings
```

Design notes:

- yaml.v3's `KnownFields` does not propagate into custom `UnmarshalYAML`
  implementations, so strictness is enforced by `checkKnownFields`, which
  walks the node tree alongside the Go type and handles `inline`, aliases
  and merge keys.
- `Monitor` is a tagged union: `MonitorCommon` + exactly one of
  `Systemd`, `Process`, `HTTP`, `Cron`. Its YAML form is flat; a generic
  `monitorDoc[T]` combines common and type-specific fields for both
  decoding and encoding.
- Decoding errors from custom unmarshalers are returned as
  `*yaml.TypeError` so errors from sibling monitors are accumulated rather
  than stopping at the first.
- Errors are `*config.ValidationError{Problems, Warnings}` with
  `Problem{File, Path, Message}` for precise CLI output.

## Data model ✅

### Monitor states (`pkg/model.State`)

| State | Meaning |
|---|---|
| `unknown` | not checked yet / undecidable |
| `starting` | supervised process inside its startup grace period |
| `running` | cron job executing |
| `healthy` | last check or run succeeded |
| `failing` | failures below `consecutive_failures` threshold |
| `failed` | threshold reached; recovery may start |
| `recovering` | recovery attempt scheduled or running |
| `exhausted` | attempts exhausted; waits for cooldown or `sentinelctl reset` |
| `stopped` | stopped by the operator |
| `disabled` | disabled in configuration or by the operator |

Allowed transitions are enforced by the recovery engine (Phase 2).

### Capability status (`pkg/model.CapabilityStatus`)

`supported`, `unsupported` (feature not implemented for this target),
`unavailable` (missing on this host, e.g. no systemd), `permission_denied`,
`error`.

### Events (`pkg/model.Event`)

| Type | Scope | Emitted when |
|---|---|---|
| `monitor_failed` | monitor | entering `failed` |
| `recovery_started` | monitor | a recovery attempt starts |
| `recovery_exhausted` | monitor | entering `exhausted` |
| `monitor_recovered` | monitor | back to `healthy` after a failure |
| `job_succeeded` / `job_failed` / `job_timeout` | job (cron) | a run ends |
| `configuration_error` | daemon | reload rejected |
| `daemon_error` | daemon | internal error |

JSON fields (also the webhook body): `event_id`, `timestamp`, `hostname`,
`sentinel_version`, `monitor_name`, `monitor_type`, `state`,
`previous_state`, `event_type`, `message`, `failure_count`,
`restart_count`, `last_error`, `metadata`. Emitters must redact messages
before creating an event.

### State file (`internal/state`)

```json
{
  "schema_version": 1,
  "updated_at": "2026-10-05T12:00:00Z",
  "monitors": {
    "worker": {
      "monitor_name": "worker",
      "monitor_type": "process",
      "current_state": "healthy",
      "enabled": true,
      "restart_count": 2,
      "failure_count": 3,
      "consecutive_failures": 0,
      "consecutive_successes": 12,
      "total_recoveries": 2,
      "restart_attempts": ["2026-10-05T11:58:00Z"],
      "last_check": "2026-10-05T12:00:00Z",
      "last_transition": "2026-10-05T11:58:30Z",
      "last_failure": "2026-10-05T11:57:55Z",
      "last_recovery": "2026-10-05T11:58:30Z",
      "last_exit_code": 137,
      "last_event": { "event_id": "…", "time": "…", "event_type": "monitor_recovered" },
      "history": [ … ]
    }
  },
  "events": [ … ]
}
```

- Cron monitors add `job`: `last_scheduled` (dedup across restarts),
  `last_started`, `last_finished`, `last_duration_ms`, `last_outcome`,
  `runs`, `failures`.
- Retention: `settings.history_limit` per monitor, `settings.event_limit`
  globally; oldest entries dropped first.
- `Prune` drops state for monitors removed from configuration.

**Persistence** (`state.Store`):

1. encode (indented JSON) → temp file `.<name>.*.tmp` in the same directory
   (mode 0600);
2. `fsync` the temp file, close, `rename` over the target;
3. `fsync` the directory so the rename is durable.

Directory created with 0750 if missing. On load: missing file → empty
state (`FirstRun`); invalid JSON / missing or invalid `schema_version` /
broken invariants / over the size limit → file renamed to
`state.json.corrupt-<UTC timestamp>` and empty state (`Recovered`); newer
`schema_version` → `ErrNewerSchema`, file untouched; permission and I/O
errors returned as-is (`errors.Is(err, fs.ErrPermission)`).

## Logging ✅

`internal/logging` returns a `*slog.Logger`.

| Format | Output |
|---|---|
| `text` | `time=… level=INFO msg=… key=value` |
| `json` | one JSON object per line |
| `journal` | `<6>level=INFO msg=… key=value` — no timestamp, syslog priority prefix parsed by journald (`<3>` error, `<4>` warn, `<6>` info, `<7>` debug) |
| `auto` | `journal` when stderr's device:inode equals `$JOURNAL_STREAM` (set by systemd), otherwise `text` |

Attributes whose key looks secret (`token`, `password`, `authorization`,
`api_key`, `secret`, `cookie`, …) are replaced by `[REDACTED]` at any
nesting level. Pass a `*slog.LevelVar` to change the level on reload.

## Planned interfaces 🚧

See [PLAN.md §7](../PLAN.md#7-planned-interfaces-to-be-introduced-in-their-phases).
