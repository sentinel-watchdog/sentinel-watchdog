# Agent architecture

Status legend: ✅ implemented · 🚧 planned (phase in [PLAN.md](../PLAN.md)) ·
📐 designed only (ADR exists, no code, no phase started).

- [Target architecture](#target-architecture) — core, platform, modules, layering, configuration layout, provider model 📐
- [Observation vs enforcement](#observation-vs-enforcement) 📐
- [Event flow](#event-flow) 📐
- [Firewall pipeline](#firewall-pipeline) 📐
- [Core components](#components) — design before ADR-0015
- [Configuration pipeline](#configuration-pipeline-) ✅ (Phase 2a) · [Logging](#logging-) ✅ (ported) · [Data model](#data-model-) — **prototype**, replaced in Phase 2b (D-064)

Decisions and alternatives: [docs/adr/](adr/README.md). Threats:
[threat-model.md](threat-model.md). Audit of the starting point:
[project-audit.md](project-audit.md).

## Target architecture

The Sentinel agent is a **modular monolith** (ADR-0001) built from a shared core and
modules enabled by configuration (ADR-0015): one `sentineld` process, one
`sentinelctl`, and strictly separated packages wired by the daemon through
small, consumer-defined interfaces. The seams keep a later split of the
firewall into its own process possible.

The fleet backend and dashboard are outside this architecture. The Remote
module owns the agent side; `pkg/api` describes the local socket protocol.
The future fleet contract needs its own versioning and ownership decision.
See [project boundaries](project-layout.md).

### Modules and layers

| Part | Packages | Talks to | Phase |
|---|---|---|---|
| Core services | `core/{config,module,events,state,notify,audit,authz,transport,clock}` | Unix socket, webhooks | 2a–2c |
| Core libraries | `core/{logging,redact,cronexpr,netspec,provider}` | — | 2a, 4a |
| Platform adapters | `platform/{executor,privilege,systemd,logsource,docker,podman,kubernetes}` | child processes, systemctl, journald, container APIs, Kubernetes API | 3b–3c, 4d |
| Supervisor module | `modules/supervisor/…` (services: http, process, systemd; jobs: cron; recovery; scheduler) | HTTP targets, supervised processes, systemd units | 3a–3e |
| Firewall module | `modules/firewall/…` (model, planner, transaction, nftables, iptables, blocklist, crowdsec, waf, networkpolicy) | `nft`, `iptables-*`, `ipset`, CrowdSec LAPI, WAF/proxy logs, Kubernetes API | 4a–4f |
| Remote module | `modules/remote/…` | fleet backend (separate project; agent-side contract) | 5 |
| Monitor candidate | `modules/monitor/…` (reserved name, D-062; scope selected through [discovery](future-modules.md), D-067) | candidate watched paths, package database, advisory feeds | 6+ |
| Control plane | `daemon`, `cmd/*`, `pkg/api` | Unix socket | 2c |
| Packaging | `deploy/`, `packaging/`, GoReleaser | — | 1, 3d |

### Layering and dependency rules

```
cmd/sentineld, cmd/sentinelctl
        │
internal/daemon        wiring: module registry, lifecycle, reload — the only package that imports modules
        │
        ├── internal/modules/   supervisor · firewall · (remote) · (monitor)
        │                       never import each other; interact through events or interfaces injected by the daemon
        ├── internal/platform/  executor · privilege · systemd · logsource · docker · podman · kubernetes
        └── internal/core/      config · module · events · state · notify · audit · authz · transport ·
                                clock · logging · redact · cronexpr · netspec · provider
                                        └──▶ pkg/model, pkg/api
```

- `core` imports only `core` and `pkg/*`; `platform` imports `core` and
  `pkg/*`; a module imports `core`, `platform`, `pkg/*` and its own
  sub-packages. Checked by an architecture test (Phase 2a).
- A module never imports another module. Example: inside the firewall
  module, blocklists receive a `SetUpdater` interface and CrowdSec is a
  blocklist *source*; a future monitor module would ask the firewall to
  block through an interface injected by the daemon.
- Interfaces are declared by their consumer. There is no `pkg/provider`;
  public wire types live in `pkg/model` and `pkg/api` (ADR-0004).
- Modules are built by explicit factories in `internal/daemon`; a
  disabled module is not constructed and its configuration directory is
  not read.

### Configuration layout

A central `sentinel.yaml` (daemon, notifications, `modules.<name>` with
`enabled` and safety gates) and one directory per enabled module
(`/etc/sentinel/<module>/*.yaml`). Rules and examples: ADR-0015 §3.

### Provider model

Every external integration is a **provider** with a uniform status
surface (ADR-0004): `ProviderState` (`disabled | ok | degraded |
unavailable | permission_denied | unsupported | error`), a list of
`Capability{name, status, detail}`, detected `Conflicts` (other managers
such as firewalld, Docker, kube-proxy, the CrowdSec bouncer) and the
effective mode. Providers receive **gated clients** that only allow the
calls their mode permits, so read-only is enforced by construction rather
than by `if` statements.

## Observation vs enforcement

| | Observation | Enforcement (change) |
|---|---|---|
| Examples | systemd/process/HTTP/cron checks; firewall status, list, plan, diff; container discovery; Kubernetes discovery and CNI detection; CrowdSec decisions as events | restart/stop/kill; firewall apply/rollback; blocklist set updates; NetworkPolicy apply |
| Default | on for configured supervisors | **off**: domains `enabled: false`, `mode: read_only`, `dry_run: true` |
| Gates | — | effective mode `enforce`, fresh plan, protected access, confirmation, audit intent, backup, safety timeout, verification, rollback (ADR-0003) |
| CLI tier | `read` | `operate` (supervisors) / `admin` (network) (ADR-0012) |

Effective modes for enforcing domains: `read_only` → `dry_run` (full
pipeline up to the backend's check, nothing committed) → `enforce`.

## Event flow

```
emitters                                   bus (internal/events)                 consumers
supervisors ─┐                                ┌──────────────────────┐   ┌─▶ state store (bounded history)
cron jobs ┤  model.Event                   │ validate + limit      │   ├─▶ notification dispatcher ─▶ webhooks
firewall ─┤  source, source_type,          │ dedup (state / window)│───┼─▶ recovery engine (actions)
blocklist ┤  event_type, severity,  ─────▶ │ correlation IDs       │   ├─▶ audit recorder (non-enforcement)
crowdsec ─┤  correlation_id,               │ per-subscriber queues │   └─▶ metrics (later)
container ┤  metadata, attributes          └──────────────────────┘
k8s, waf ─┘
firewall transactions ──────── synchronous ────────▶ internal/audit (fail closed)
```

Details: ADR-0002. Enforcement records never depend on the asynchronous
bus.

## Firewall pipeline

```
config.firewall ─▶ planner.Validate (netspec, duplicates, conflicts, protected access, limits)
                 ─▶ backend.Observe (owned table/chains only) ─▶ planner.Diff ─▶ Plan{changes, fingerprint}
                 ─▶ backend.Render (owned objects only) ─▶ backend.Check (nft --check / iptables-restore --test)
                          │ read_only / dry_run stop here (plan shown, dry-run audited)
                          ▼ enforce
   lock ─▶ re-observe + fingerprint check ─▶ audit intent ─▶ backup ─▶ Commit (one atomic transaction)
        ─▶ verify (re-observe == expected) ─▶ pending_confirmation (safety_timeout) ─▶ confirm | auto-rollback
        ─▶ audit result ─▶ events (correlation_id = tx id)
```

Backends: nftables via `nft -j` in a dedicated `inet sentinel` table
(ADR-0006); iptables via `iptables-save`/`iptables-restore --noflush` in
dedicated `SENTINEL-*` chains (ADR-0007). Selection `auto` prefers
nftables and pins the backend after the first commit. Sentinel's `drop` is
final; its `accept` only ends evaluation inside Sentinel's own chains.

## Components

> Written before ADR-0015 and the clean restart (D-064). The sections from
> here to the end describe the prototype and the earlier supervision-only
> design; Phase 2 rewrites them for the core and module layout.

```
             ┌──────────────┐  Unix socket (JSON, versioned)  ┌──────────────┐
             │ sentinelctl  │ ───────────────────────────────▶│  sentineld   │
             └──────────────┘          🚧 Phase 5             └──────┬───────┘
                                                                     │
  ┌──────────── internal/daemon (orchestration, reload) 🚧 ──────────┤
  │                                                                  │
  │  config ✅ ──▶ supervisors 🚧 ──Result──▶ recovery 🚧 ──Event──▶ events 🚧 ──▶ notification 🚧
  │                 │  ▲                     │                           │
  │                 ▼  │                     ▼                           ▼
  │            executor 🚧            systemctl / process          state ✅ (JSON)
  │            scheduler (cronexpr ✅ parse, runner 🚧)
  └──────────── logging ✅ · redact ✅ · lifecycle 🚧 · privilege 🚧
```

| Package | Responsibility | Status |
|---|---|---|
| `internal/core/config` | Central file, module directories, ownership checks, env expansion, strict decoding, defaults, validation, redacted view | ✅ Phase 2a |
| `internal/core/module` | Module contract (`Module`, `Configured`) and registry | ✅ Phase 2a (runtime services, status, commands: 2b–2c) |
| `internal/core/clock` | Injectable clock and fake for tests | ✅ Phase 2a |
| `internal/core/logging` | slog logger: text/json/journal/auto, attribute redaction | ✅ (ported in 2a) |
| `internal/core/redact` | Secret masking for headers, URLs, env, free text | ✅ (ported in 2a) |
| `internal/core/cronexpr` | Cron expression parser | ✅ parse · 🚧 `Next()` (Phase 3c) |
| `internal/archtest` | Tests enforcing the ADR-0015 dependency rules | ✅ Phase 2a |
| `internal/state`, `pkg/model` | Prototype state store and event model | prototype, replaced in Phase 2b |
| `internal/version` | Build metadata via `-ldflags -X` | ✅ |
| `internal/core/{events,state,notify}` | Event bus, per-module state, notifications | 🚧 Phase 2b |
| `internal/core/{transport,authz,audit}`, `internal/daemon`, `pkg/api`, `cmd/*` | Socket, tiers, audit log, daemon, protocol, binaries | 🚧 Phase 2c |
| `internal/platform/*`, `internal/modules/*` | Adapters and modules | 🚧 Phases 3–4 |

Dependency rules (ADR-0015, enforced by `internal/archtest`): `core`
imports neither `platform`, `modules`, `daemon` nor `cmd`; `platform`
imports neither `modules`, `daemon` nor `cmd`; a module never imports
another module, `daemon` or `cmd`; `pkg` imports nothing internal.
Interfaces are defined by their consumer.

External dependencies: `go.yaml.in/yaml/v3` only.

## Configuration pipeline ✅

Implemented in `internal/core/config` (Phase 2a, ADR-0015).

```
config dir ─▶ parents checked up to / ─▶ opened as os.Root ─▶ checked on the open descriptor
sentinel.yaml (opened inside the root, non-blocking) ─▶ ownership/mode check ─▶ read (≤4 MiB, ≤16 MiB total)
  ─▶ yaml.Node (one document, top-level mapping) ─▶ reject duplicate and merge keys
  ─▶ ${VAR} expansion on scalar values (bounded growth) ─▶ version check
  ─▶ unknown-key check (reflection over yaml tags, line numbers, bounded walk)
  ─▶ decode daemon / notifications / modules ─▶ defaults ─▶ validation
  ─▶ for each module under `modules`: known name? enabled? available?
       └─ enabled + available ─▶ <dir>/<module>/*.yaml, each through the same
          check/read/expand/version steps ─▶ config.ModuleConfig{Central, Files}
  ─▶ *config.Config (+ warnings, ignored directories)
module.Registry.Configure(cfg) ─▶ each enabled module's Configure(ModuleConfig)
  ─▶ module-level strict decode + validation ─▶ []module.Instance or all problems
```

Design notes:

- `internal/core/config` never imports a module. The daemon passes the
  known module names and their availability (available, planned, not
  built) from `module.Registry`; modules decode their own sections.
- yaml.v3's `KnownFields` does not propagate into custom `UnmarshalYAML`
  implementations, so strictness is enforced by `checkKnownFields`, which
  walks the node tree alongside the Go type (inline fields, aliases, merge
  keys). The walk has a node budget so YAML aliases cannot make it
  explode; `yaml.Node` fields are skipped and decoded later by their owner.
- Ownership and write bits are checked on the opened descriptors of the
  configuration directory, module directories and files, and on every
  parent directory. Files are resolved through `os.Root`, so symbolic
  links cannot leave their directory (D-069). Duplicate and merge keys are
  rejected and every load is size-bounded (D-070).
- Errors are `*config.ValidationError{Problems, Warnings}` with
  `Problem{File, Path, Message}`; modules return the same type so every
  problem of every file is reported at once.

## Data model ✅

### Supervisor states (`pkg/model.State`)

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

Allowed transitions are enforced by the recovery engine (Phase 4).

### Capability status (`pkg/model.CapabilityStatus`)

`supported`, `unsupported` (feature not implemented for this target),
`unavailable` (missing on this host, e.g. no systemd), `permission_denied`,
`error`.

### Events (`pkg/model.Event`)

| Type | Scope | Emitted when |
|---|---|---|
| `supervisor_failed` | supervisor | entering `failed` |
| `recovery_started` | supervisor | a recovery attempt starts |
| `recovery_exhausted` | supervisor | entering `exhausted` |
| `supervisor_recovered` | supervisor | back to `healthy` after a failure |
| `job_succeeded` / `job_failed` / `job_timeout` | job (cron) | a run ends |
| `configuration_error` | daemon | reload rejected |
| `daemon_error` | daemon | internal error |

JSON fields (also the webhook body): `event_id`, `timestamp`, `hostname`,
`sentinel_version`, `supervisor_name`, `supervisor_type`, `state`,
`previous_state`, `event_type`, `message`, `failure_count`,
`restart_count`, `last_error`, `metadata`. Emitters must redact messages
before creating an event.

> 🚧 Phase 3 replaces this supervisor-centric shape with the generalised model
> of [ADR-0002](adr/0002-event-model-and-bus.md) (`source`, `source_type`,
> `severity`, `correlation_id`, `attributes`) before any release freezes
> the webhook payload. The state file moves to schema v2 with a migration
> ([ADR-0011](adr/0011-state-audit-transactions.md)).

### State file (`internal/state`)

```json
{
  "schema_version": 1,
  "updated_at": "2026-10-05T12:00:00Z",
  "supervisors": {
    "worker": {
      "supervisor_name": "worker",
      "supervisor_type": "process",
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
      "last_event": { "event_id": "…", "time": "…", "event_type": "supervisor_recovered" },
      "history": [ … ]
    }
  },
  "events": [ … ]
}
```

- Cron supervisors add `job`: `last_scheduled` (dedup across restarts),
  `last_started`, `last_finished`, `last_duration_ms`, `last_outcome`,
  `runs`, `failures`.
- Retention: `settings.history_limit` per supervisor, `settings.event_limit`
  globally; oldest entries dropped first.
- `Prune` drops state for supervisors removed from configuration.

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

`internal/core/logging` returns a `*slog.Logger`.

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

See [PLAN.md §6](../PLAN.md#6-key-interfaces-sketches-refined-when-implemented).
