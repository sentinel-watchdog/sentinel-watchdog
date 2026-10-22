# Agent architecture

Status legend: ✅ implemented · 🚧 planned (phase in [PLAN.md](../PLAN.md)) ·
📐 designed only (ADR exists, no code, no phase started).

- [Target architecture](#target-architecture) — core, platform, modules, layering, configuration layout, provider model 📐
- [Observation vs enforcement](#observation-vs-enforcement) 📐
- [Event flow](#event-flow) 📐
- [Firewall pipeline](#firewall-pipeline) 📐
- [Core components](#components) — design before ADR-0015
- [Configuration pipeline](#configuration-pipeline-) ✅ (Phase 2a) · [Logging](#logging-) ✅ (ported) · [Data model](#data-model-) ✅ (events, state, notifications: Phase 2b)

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
emitters                                   bus (internal/core/events)            consumers
supervisors ─┐                                ┌──────────────────────┐   ┌─▶ state store (per module)
cron jobs ┤  model.Event                   │ validate + limit      │   ├─▶ notification dispatcher ─▶ webhooks
firewall ─┤  module, source, source_type,  │ registered types only │───┼─▶ recovery engine (actions)
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
| `internal/core/module` | Module contract (`Module`, `Configured`, `Runtime`, `Router`, `Warner`, `Commander`, `Handler`) and registry | ✅ Phase 2a, runtime 2c-1, warnings 3a-1, commands 3a-2 |
| `internal/core/clock` | Injectable clock and fake for tests | ✅ Phase 2a |
| `internal/core/logging` | slog logger: text/json/journal/auto, attribute redaction | ✅ (ported in 2a) |
| `internal/core/redact` | Secret masking for headers, URLs, env, free text | ✅ (ported in 2a) |
| `internal/core/cronexpr` | Cron expression parser | ✅ parse · 🚧 `Next()` (Phase 3c) |
| `internal/archtest` | Tests enforcing the ADR-0015 dependency rules | ✅ Phase 2a |
| `pkg/model` | Public types: `Event`, severities, core event types | ✅ Phase 2b |
| `internal/core/fstrust` | Ownership and path checks for configuration and state | ✅ Phase 2b |
| `internal/version` | Build metadata via `-ldflags -X` | ✅ |
| `internal/core/{events,state,notify}` | Event registry and bus, per-module state, notification dispatcher and webhook | ✅ Phase 2b |
| `internal/daemon`, `cmd/sentineld`, `cmd/sentinelctl` | Lifecycle of the core and the modules; `sentineld`; `sentinelctl version\|validate` | ✅ Phase 2c-1 |
| `pkg/api`, `internal/core/{transport,authz}`, `internal/platform/peercred` | Control protocol v1, socket server and client, tiers, peer credentials; read commands | ✅ Phase 2c-2 |
| `internal/core/audit` | Hash-chained audit log, start-up check, audit of operate/admin requests, `sentinelctl audit` | ✅ Phase 2c-3a |
| `internal/daemon` (reload) | `core.reload` / `sentinelctl reload` and SIGHUP: whole-configuration validation, per-module restart, live and restart-required keys | ✅ Phase 2c-3b |
| `internal/platform/sdnotify`, `.goreleaser.yaml` | sd_notify readiness and stopping for `Type=notify`; release-shaped snapshot (tarballs, checksums, SBOMs) | ✅ Phase 2c-4 |
| `internal/modules/supervisor` | Supervisor core: schema, checks loop, policy, state, events, commands (D-081); HTTP checker, registered, `sentinelctl supervisor list\|status` (D-082) | ✅ Phase 3a-3, 3a-4 |
| `internal/platform/{executor,privilege}`, `tests/integration` | Run programs in their own group with an explicit environment and identity, stop the group, bounded output and safe output files; user/group resolution and capabilities; root integration tests via `task test-vm` (D-084) | ✅ Phase 3b-1 |
| `internal/modules/supervisor` (processes), `internal/platform/executor` (group records, trusted programs), `state.LoadMigrating`, `module.Runtime.Redact` | Process services: central switch, schema, launch, startup grace, crash as `service_failed`, stop, cleanup of recorded groups; state schema 2 (D-086) | ✅ Phase 3b-2 |
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
config dir ─▶ resolved one component at a time, every directory on the way checked ─▶ opened as os.Root ─▶ checked on the open descriptor
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
  directory looked into while the path is resolved. Files are
  resolved through `os.Root`, and a symbolic link may only name an entry
  of its own directory (D-070). Duplicate and merge keys are
  rejected and every load is size-bounded (D-071).
- Errors are `*config.ValidationError{Problems, Warnings}` with
  `Problem{File, Path, Message}`; modules return the same type so every
  problem of every file is reported at once.

## Data model ✅

### Events (`pkg/model.Event`, `internal/core/events`) ✅ Phase 2b

An event is something that happened, emitted by a module and delivered to
the core's consumers. Fields (also the `event` object of the webhook
payload, [notifications.md](notifications.md)): `event_id`, `timestamp`,
`hostname`, `sentinel_version`, `module`, `source`, `source_type`,
`event_type`, `severity`, `state`, `previous_state`, `message`,
`correlation_id`, `metadata`, `attributes`.

- **Registry.** Each event type belongs to one module and has a default
  severity; the core's `configuration_error` and `daemon_error` are
  pre-registered. Publishing an unregistered type, or a type as another
  module, is an error.
- **Normalisation at the bus** (ADR-0002, T-18): identifiers, message,
  metadata and attribute strings are cleaned (no control, format or
  separator characters, valid UTF-8) and shortened; attribute keys match `^[a-z0-9_.]{1,64}$`
  and values are JSON scalars, string lists or one nested level; the
  encoded event stays under 16 KiB (attributes, then metadata, then the
  message shrink), and the work spent on attributes is bounded by that
  cap. The bus stamps ID, timestamp, hostname and version; `Publish`
  returns only the ID, so emitters never hold the maps subscribers read.
- **Delivery.** Each subscriber has a bounded queue; when it is full the
  oldest event is dropped and counted, so `Publish` never blocks. Order is
  kept per publishing goroutine (per source), not globally.
- Deduplication windows (ADR-0002) are added when a module needs them.

### State (`internal/core/state`) ✅ Phase 2b

```
<daemon.state_dir>/            0750, checked like configuration (fstrust)
├── core/state.json            0700 directory, 0600 file
└── <module>/state.json
```

Each file is an envelope with the module's own schema version:

```json
{"schema_version": 1, "updated_at": "2026-10-08T12:00:00Z", "data": { … }}
```

- `Load[T]` returns a fresh `T`: no file → zero value (`FirstRun`);
  corrupt file (bad JSON, missing/invalid version, data that does not
  decode or fails `T`'s `Validate`, JSON `null` when `T` is a pointer,
  over 16 MiB) → moved aside as
  `state.json.corrupt-<UTC time>`, zero value (`Recovered`); newer or older
  `schema_version` → error, file untouched (no migration exists yet);
  a file or directory another user could write → error.
- `Save` writes a temporary file in the module directory, `fsync`s it,
  renames it over `state.json` and `fsync`s the directory. Everything is
  resolved inside the opened state directory (`os.Root`).
- One `Store` per module: `Load` holds its lock from reading to
  quarantine, so a concurrent `Save` is never moved aside; `Save` refuses
  a state over the 16 MiB that `Load` would reject.
- State never holds secrets (ADR-0011).

### Notifications (`internal/core/notify`) ✅ Phase 2b

The dispatcher reads events, asks a route function (built by the daemon
from `notifications.core` and the modules' item routes) which channels an
event goes to, and queues it on each. Every enabled channel has a bounded
queue (drop oldest + counter) and one worker that delivers with retries
and backoff on the injected clock, applying `repeat_interval`. The webhook
sender never follows redirects, bounds each attempt by the channel
timeout, and never quotes the URL in errors. Contract and behaviour:
[notifications.md](notifications.md).

## Daemon ✅ Phase 2c-1

`sentineld` loads the configuration (`daemon.Load`: central file, module
directories, `Configure` of every enabled module; nothing changes on the
system), builds its logger from `daemon.log`, then `Daemon.Start`:

1. opens `daemon.state_dir` (checked, created if missing, D-074);
2. creates the event registry and bus, the notification dispatcher and
   two subscribers: the dispatcher and a logger that logs every event;
   delivery runs on its own context, so it outlives a signal;
3. starts every enabled module in name order, giving each a
   `module.Runtime` bound to it: logger, clock, an `events.Emitter` (it
   can only register and publish in its own name) and its state store.
   A module that implements `module.Router` routes its events to
   channels; core events follow `notifications.core`.

SIGTERM or SIGINT calls `Daemon.Stop`: running modules stop in reverse
order, sharing `daemon.shutdown_timeout` (a quarter is kept for the last
deliveries); the bus closes, the dispatcher delivers what is queued
until the deadline (then delivery is abandoned, even when a module's
router blocks), and the state directory closes. A log destination that
blocks every write (a full stderr pipe) still blocks sentineld: bounded
asynchronous logging is backlog B-004. A signal during start cancels it: the module starting gets a
cancelled context and no further module starts. From the first signal
on, a second one ends the process at once.

Module calls are isolated (D-075): each `Start`/`Stop` runs in its own
goroutine with a recovered panic and a time limit. A failure, a panic or
a timeout is logged (a panic with its frames, never its value) and
published as `daemon_error` (source: the module; no error text, which
stays in the local log after `Config.RedactText` has masked the values
the configuration knows to be secret). A module that fails to start gets a bounded
`Stop`; the others keep running. A call that does not return in time is
abandoned: Go cannot stop a goroutine, so the state directory is then
left open until the process exits. `Daemon.Snapshot` copies module
states, subscriber drops and channel counters (logged at stop; read by
`sentinelctl status` in 2c-2).

Under systemd, `cmd/sentineld` sends `READY=1` once `Start` has returned
without a cancelling signal and `STOPPING=1` before `Stop`, through
`internal/platform/sdnotify` (D-079); it removes `NOTIFY_SOCKET` from its
environment first, so nothing it starts can report for it. Reloads send
nothing. Operators: [operations.md](operations.md).

The process sets umask `027`. Exit codes: 0 clean stop or valid
configuration; 1 invalid configuration, failed start of the core, or an
unclean stop (a module failed or was abandoned at start or stop, or
delivery was abandoned); 2 usage.

## Control socket ✅ Phase 2c-2

`sentinelctl` connects to `daemon.socket`, writes one request and reads
one response, each a JSON object on one line (`pkg/api`, version 1):

```json
{"version":1,"command":"core.status"}
{"version":1,"result":{"version":"…","uptime_seconds":42,"modules":[…]}}
{"version":1,"error":{"code":"permission_denied","message":"…"}}
```

- **Commands** are `<namespace>.<command>` and declare their tier in
  `pkg/api`: `core.status`, `core.modules`, `core.config_show` (the
  redacted core configuration), `core.events` (the last 256 events,
  kept in memory by the log subscriber, D-083) and `core.audit` are all
  `read`; `core.reload` is `operate`.
- **Module commands** (D-080, Phase 3a-2): a module that implements
  `module.Commander` declares its commands (`<module>.<command>` and a
  tier) on the unconfigured module; the registry checks them at
  `Register`, and `Configure` refuses a module that declares commands
  without a `module.Handler`. The socket binds the commands of every
  module the binary knows once, at start, so neither the commands nor
  their tiers depend on the configuration. Each request goes to the
  module's running instance; a module not enabled, failed or stopping
  answers `unavailable`. Stopping a module (shutdown, reload, a failed
  start) first closes its commands, then waits for those in flight within
  the stop's time limit, so `Stop` never runs beside `Handle`. A panic in
  `Handle`, or in encoding its result (a module's `MarshalJSON` is module
  code too), is recovered and reported like one in `Start`
  (`daemon_error`, operation `command <name>`); the client gets
  `internal`.
- **Authorization** (ADR-0012, D-076): the server reads the peer's uid,
  gid and supplementary groups from the kernel (`SO_PEERCRED`,
  `SO_PEERGROUPS`; `internal/platform/peercred`), maps them to a tier
  (`internal/core/authz`: root and `admin_group` → admin,
  `operator_group` → operate, anyone who could connect → read) and checks
  it against the command's declared tier before running it. Nothing the
  client sends can raise its tier. Outside Linux there are no peer
  credentials: every request answers `unavailable`.
- **Limits:** requests ≤ 64 KiB, responses ≤ 16 MiB, one exchange ≤ 5 s,
  32 connections at once (more are closed at once). The decoder is strict
  (`encoding/json/v2`: no unknown or duplicate member, valid UTF-8,
  nothing after the object) and fuzzed.
- **Audit** (D-077): a request for an operate or admin command is
  recorded before it runs (`intent`, then `ok` or `failed`), or as
  `denied`; if the intent cannot be recorded the command does not run.
  Connections are limited per user (burst 10, then one per second; root
  is not limited and keeps the last slot).

## Audit log ✅ Phase 2c-3a

`internal/core/audit` appends one JSON record per line to
`<state_dir>/audit.jsonl`, synced before it counts, each chained to the
previous one by a SHA-256 over its exact bytes; `audit_head.json` holds
the last record and the anchor before the oldest kept one. Rotation keeps
five files of 10 MiB. At start-up every kept file is checked against the
head: a broken chain is reported (`core.status`, `audit_integrity_broken`)
and never repaired, and the log then refuses new records, so operate and
admin requests are refused until the operator archives the files and
restarts. Each record is also logged at `info` for the journal. Tamper
evidence, not prevention: root can rewrite everything (T-16).

## Reload ✅ Phase 2c-3b

`Daemon.Reload` serves `core.reload` and SIGHUP alike (D-075, D-078). It
takes the lifecycle gate that `Stop` also takes, records
`core.config_reload` (`intent`, with the running digest) and then
prepares everything without effect: load and validate the files, resolve
the access groups, build the new dispatcher if the channels changed. A
problem there keeps the running configuration. Otherwise it stops the
modules removed or changed (reverse order), publishes the new view (the
effective configuration, its redacted JSON and the routers, swapped as one
value), sets the access policy and the log level, hands the forwarder the
new dispatcher (the old one is cancelled; its queue is counted) and starts
the new instances in the new order. Keys bound at start-up keep their
running value. The whole reload has a budget of twice
`daemon.shutdown_timeout`, which also caps each module call and the wait
for the replaced dispatcher. A module whose `Stop` failed or did not
return is remembered (`unstopped`): it is never started again beside its
old instance, and `Stop` keeps the state directory and the socket lock.
- **Socket file:** the directory is created and checked like
  configuration (`fstrust.OpenDir`); `<socket>.lock` is locked (`flock`)
  so two daemons never race; an existing socket is removed only if
  nobody listens on it, anything else is an error; after bind the mode is
  narrowed, the group set, then the final mode applied. The socket is
  acquired before any module starts and served after `Start`; `Stop`
  closes it first, letting running commands answer.

## Supervisor module ✅ Phase 3a-3, 3a-4

`internal/modules/supervisor` (D-080, D-081, D-082) is one package:
`config.go` and `config_http.go` (schema, defaults, located problems),
`check.go` (`Checker`, `Result`, fixed failure codes), `http.go` (the
HTTP checker), `policy.go` (the pure transition function),
`service.go` (the loop), `store.go` (state file, digest, reconciliation),
`events.go`, `commands.go`, `module.go` (contract and lifecycle).

- **Loop.** One goroutine per service, owned by a context of the module
  (not Start's). It checks at once, then one `check_interval` after each
  check ends. A check takes a slot of a channel semaphore
  (`max_concurrent_checks`) and runs in its own goroutine, which gives the
  slot back only when `Check` returns. The loop waits for the result, the
  service `timeout` (injected clock) or the module's end. On timeout the
  check counts as failed (`timeout`) and stays *stalled*: no new call for
  the service until it returns, a `timeout` counted each interval.
- **Policy.** `step(streak, ok, policy)` is pure: N consecutive failures
  → `failed` (`service_failed`), M consecutive successes → `healthy`
  (`service_recovered`); the first success is silent; a check cancelled by
  `Stop` is not counted.
- **Persist, then publish.** On a transition the state file is written
  before the event, whose id (drawn first) is the incident's correlation
  id; counters alone are saved at most every 60 s. A failed save is shown
  in status and retried; supervision goes on.
- **Stop.** Cancel, join the loops, save, then wait for the checks still
  running within the daemon's limit: a check is never left running
  behind a clean stop (D-078 (3)).
- **Commands.** `supervisor.list`, `supervisor.status` (tier read),
  results in `pkg/api`, copied under the lock without waiting for a check
  or the disk.
- **HTTP checker.** Built at `Configure` (the CA bundle is read there, so
  `validate` reports it); one transport per service with no proxy, no
  keep-alive, HTTP/1.1, bounded headers and body reads, redirects only
  within the origin; `address` replaces the dial target, never the URL.
  A finished check closes the connection attempts net/http keeps after a
  cancelled request. Errors become fixed codes (operator reference:
  [supervisor.md](supervisor.md)).

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
nesting level. sentineld passes a `*slog.LevelVar`, which a reload sets
in place.

## Planned interfaces 🚧

See [PLAN.md §6](../PLAN.md#6-key-interfaces-sketches-refined-when-implemented).
