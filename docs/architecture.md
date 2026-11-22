# Agent architecture

How the agent is built: the target design, the packages that exist today
and how the daemon, its control socket and the supervisor work. What is
built and what comes next is in [PLAN.md §7](../PLAN.md#7-phases); the
operator view of each part is in the operator documents linked below.

- [Target architecture](#target-architecture): core, platform, modules,
  layering, configuration layout, provider model
- [Observation vs enforcement](#observation-vs-enforcement)
- [Event flow](#event-flow) and [firewall pipeline](#firewall-pipeline)
  (the firewall is planned for Phase 4)
- [Packages](#packages): every package and directory, with its phase
- How it works today: [configuration pipeline](#configuration-pipeline),
  [data model](#data-model), [daemon](#daemon),
  [control socket](#control-socket), [audit log](#audit-log),
  [reload](#reload), [supervisor module](#supervisor-module),
  [logging](#logging), [interfaces](#interfaces)

Decisions and alternatives: [docs/adr/](adr/README.md). Threats:
[threat-model.md](threat-model.md). Audit of the starting point:
[archive/project-audit.md](archive/project-audit.md).

## Target architecture

The Sentinel agent is a modular monolith (ADR-0001) built from a shared
core and modules enabled by configuration (ADR-0015): one `sentineld`
process, one `sentinelctl`, and strictly separated packages wired by the
daemon through small, consumer-defined interfaces. The seams keep a later
split of the firewall into its own process possible (R-015).

The fleet backend and dashboard are outside this architecture. The Remote
module owns the agent side; `pkg/api` describes the local socket protocol.
The future fleet contract needs its own versioning and ownership decision.
See [project boundaries](project-layout.md).

### Modules and layers

| Part | Packages | Talks to | Phase |
|---|---|---|---|
| Core services | `core/{config,module,events,state,notify,audit,authz,transport,clock}` | Unix socket, webhooks | 2a to 2c |
| Core libraries | `core/{logging,redact,cronexpr,fstrust,tlsconfig}`; planned `core/{netspec,provider}` | none | 2a to 3a; 4a |
| Platform adapters | `platform/{executor,privilege,systemd,logsource,journald,peercred,sdnotify}`; planned `platform/{docker,podman,kubernetes}` | child processes, systemd over D-Bus, journald, the kernel; container APIs, Kubernetes API | 2c to 3c; 4d |
| Supervisor module | `modules/supervisor` (HTTP, process and systemd services, cron jobs, recovery, logs) | HTTP targets, supervised processes, systemd units, the journal | 3a to 3c |
| Firewall module | `modules/firewall/…` (model, planner, transaction, nftables, iptables, blocklist, crowdsec, waf, networkpolicy) | `nft`, `iptables-*`, `ipset`, CrowdSec LAPI, WAF/proxy logs, Kubernetes API | 4a to 4f |
| Remote module | `modules/remote/…` | fleet backend (separate project; agent-side contract) | 5 |
| Monitor candidate | `modules/monitor/…` (reserved name, D-062; scope selected through [discovery](future-modules.md), D-067) | candidate watched paths, package database, advisory feeds | 6+ |
| Control plane | `daemon`, `cmd/*`, `pkg/api` | Unix socket | 2c |
| Packaging | `.goreleaser.yaml` today; `deploy/`, `packaging/` | none | 2c-4, 3d |

### Layering and dependency rules

```
cmd/sentineld, cmd/sentinelctl
        │
internal/daemon        wiring: module registry, lifecycle, reload; the only package that imports modules
        │
        ├── internal/modules/   supervisor · (firewall) · (remote) · (monitor)
        │                       never import each other; interact through events or interfaces injected by the daemon
        ├── internal/platform/  executor · privilege · systemd · logsource · journald · peercred · sdnotify ·
        │                       (docker) · (podman) · (kubernetes)
        └── internal/core/      config · module · events · state · notify · audit · authz · transport ·
                                clock · logging · redact · cronexpr · fstrust · tlsconfig
                                        └──▶ pkg/model, pkg/api
```

- `core` imports only `core`, `internal/version` and `pkg/*`; `platform`
  also imports `platform`; a module imports `core`, `platform`,
  `internal/version`, `pkg/*` and its own sub-packages; `pkg` imports nothing internal. A module never imports
  another module, `daemon` or `cmd`. `internal/archtest` enforces these
  rules; a failing archtest is a design error.
- Example: inside the firewall module, blocklists will receive a
  `SetUpdater` interface and CrowdSec will be a blocklist source; a future
  monitor module would ask the firewall to block through an interface
  injected by the daemon.
- Interfaces are declared by their consumer. There is no `pkg/provider`;
  public wire types live in `pkg/model` and `pkg/api` (ADR-0004).
- Modules are built by explicit factories in `internal/daemon`. Every
  module registered with a factory is constructed unconfigured at
  registration, to learn its name and commands (planned modules have
  none); only enabled modules are configured and
  started, and only their directories are read (`validate --all` also
  checks the disabled ones).
- Runtime dependencies: `go.yaml.in/yaml/v3`, imported only by
  `internal/core/config`, and `github.com/godbus/dbus/v5`, confined to
  `internal/platform/systemd` (D-094, ADR-0013). Both are enforced by
  `internal/archtest`.

### Configuration layout

A central `sentinel.yaml` (daemon, notifications, `modules.<name>` with
`enabled` and safety switches) and one directory per enabled module
(`/etc/sentinel/<module>/*.yaml`). Rules and examples: ADR-0015 §3 and
[configuration](configuration.md).

### Provider model

Planned for Phase 4a. Every external integration is a provider with a
uniform status surface (ADR-0004): `ProviderState` (`disabled | ok |
degraded | unavailable | permission_denied | unsupported | error`), a
list of `Capability{name, status, detail}`, detected `Conflicts` (other
managers such as firewalld, Docker, kube-proxy, the CrowdSec bouncer) and
the effective mode. Providers receive gated clients that only allow the
calls their mode permits, so read-only is enforced by construction and a
read-only provider has no checks to forget.

## Observation vs enforcement

ADR-0003. Observing never changes the host; enforcing does, so it is off
by default and passes through gates.

| | Observation | Enforcement (change) |
|---|---|---|
| Today | HTTP checks; systemd units read; process and job output read; status, events, audit, logs | starting, stopping and restarting process services and jobs; restarting systemd units |
| Today's gates | none beyond the module's `enabled` | the central switches `modules.supervisor.processes` and `modules.supervisor.systemd_restart` (ADR-0015 rule 1); automatic restarts within a recovery budget; launches and restarts only once their audit intent is recorded (a memory stop records its intent if it can, and goes ahead anyway); operator commands at the `operate` tier, audited by the socket |
| Phase 4 | firewall status, list, plan, diff; container and Kubernetes discovery and CNI detection; CrowdSec decisions as events | firewall apply and rollback; blocklist set updates; NetworkPolicy apply |
| Phase 4 gates | none | off by default (`enabled: false`, `mode: read_only`, `dry_run: true`); effective mode `enforce`, fresh plan, protected access, confirmation, audit intent, backup, safety timeout, verification, rollback |
| CLI tier | `read`, except `supervisor logs` (`operate`, audited) | `operate` (supervisor) or `admin` (network) (ADR-0012) |

Effective modes for the firewall's enforcing domains: `read_only`, then
`dry_run` (full pipeline up to the backend's check, nothing committed),
then `enforce`.

## Event flow

```
emitters                                   bus (internal/core/events)            consumers
supervisor ──┐                                ┌──────────────────────┐   ┌─▶ notification dispatcher ─▶ webhooks
core ────────┤  model.Event                   │ validate + limit      │   ├─▶ event log (recent events, logged)
(firewall) ──┤  module, source, source_type,  │ registered types only │───┘
(blocklist) ─┤  event_type, severity,  ─────▶ │ correlation IDs       │
(crowdsec) ──┤  correlation_id,               │ per-subscriber queues │
(container) ─┤  metadata, attributes          └──────────────────────┘
(k8s, waf) ──┘
privileged actions ─────── synchronous ────────▶ internal/core/audit (fail closed)
```

Emitters in parentheses are planned. Details: ADR-0002. Audit records
never depend on the asynchronous bus: a module records its privileged
actions directly through `module.Runtime.Audit`.

## Firewall pipeline

Planned for Phase 4 (ADR-0005).

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

## Packages

Every package and top-level directory, with the phase that added it.
Planned entries have no code yet.

| Package or directory | Responsibility | Phase |
|---|---|---|
| `cmd/sentineld` | The daemon's entry point: flags, signals, sd_notify, exit codes | 2c-1 |
| `cmd/sentinelctl` | The CLI: `version`, `validate`, then every socket command, module commands under the module's name | 2c-1 |
| `internal/daemon` | Composition root: module registry, loading, lifecycle of the core and the modules, reload, the socket's command table | 2c-1, 2c-3b |
| `internal/core/config` | Central file, module directories, ownership checks, `${VAR}` expansion, strict decoding, defaults, validation, redacted view | 2a |
| `internal/core/module` | Module contract (`Module`, `Configured`, `Runtime`, `Router`, `Warner`, `Commander`, `Handler`, `Reconfigurer`, `Auditor`) and registry | 2a, 2c-1, 3a, 3b |
| `internal/core/clock` | Injectable clock and a fake for tests | 2a |
| `internal/core/logging` | slog logger: text, json, journal, auto; attribute redaction | 2a (ported) |
| `internal/core/redact` | Secret masking for headers, URLs, environment, free text | 2a (ported) |
| `internal/core/cronexpr` | Cron expression parser and run times across DST (D-096) | 2a, 3c-3 |
| `internal/core/fstrust` | Ownership and path checks for configuration, state and executables | 2b |
| `internal/core/events` | Event registry, normalisation and bus | 2b |
| `internal/core/state` | Per-module state files: atomic save, quarantine, migration hook | 2b |
| `internal/core/notify` | Notification dispatcher and webhook sender | 2b |
| `internal/core/transport` | Control socket server and client | 2c-2 |
| `internal/core/authz` | Tiers `read`, `operate`, `admin` from peer credentials | 2c-2 |
| `internal/core/audit` | Hash-chained audit log, start-up check, `sentinelctl audit` | 2c-3a |
| `internal/core/tlsconfig` | TLS client configuration shared by webhooks and HTTP checks; CA bundles | 3a |
| `internal/core/netspec` | IP, CIDR, port and protocol parsing | planned, 4a |
| `internal/core/provider` | Provider and capability status, gated clients | planned, 4a |
| `internal/platform/peercred` | Peer credentials of a socket connection (`SO_PEERCRED`, `SO_PEERGROUPS`) | 2c-2 |
| `internal/platform/sdnotify` | sd_notify readiness and stopping for `Type=notify` (D-079) | 2c-4 |
| `internal/platform/executor` | Run programs in their own group with an explicit environment and identity, stop the group, bounded output lines, safe output files, group records, trusted programs, `journalctl` runner | 3b-1, 3b-2, 3c-5 |
| `internal/platform/privilege` | User and group resolution, capabilities | 3b-1 |
| `internal/platform/systemd` | systemd units over D-Bus: read properties, restart (D-093 to D-095) | 3c-1, 3c-2 |
| `internal/platform/logsource` | Recent lines from the journal and from output files (D-099) | 3c-5a |
| `internal/platform/journald` | Program output as journal entries of its own (D-100) | 3c-5b |
| `internal/platform/{docker,podman}` | Container runtime APIs, GET only | planned, 4d |
| `internal/platform/kubernetes` | client-go behind a read-only gate | planned, 4d |
| `internal/modules/supervisor` | The supervisor module; see [Supervisor module](#supervisor-module) | 3a to 3c |
| `internal/modules/firewall` | Firewall module: model, planner, transaction, nftables, iptables, blocklist, crowdsec, waf, networkpolicy | planned, 4 |
| `internal/modules/remote` | Remote module, agent side | planned, 5 |
| `internal/modules/monitor` | Candidate, created only if selected (D-067) | 6+ |
| `internal/archtest` | Tests enforcing the ADR-0015 dependency rules; no production code | 2a |
| `internal/version` | Build metadata through `-ldflags -X` | 0 |
| `pkg/model` | Public types: `Event`, severities, core event types | 2b |
| `pkg/api` | Control protocol v1: requests, responses, commands and their tiers, result types | 2c-2 |
| `configs/` | Example central file and per-module examples (D-058), shipped as `examples/` | 2a, 3a to 3c |
| `deploy/`, `packaging/` | systemd unit and OpenRC script; the installed configuration, package scripts and Debian files (D-102) | 3d-1 |
| `tests/integration/<area>/` | Real-software tests (`//go:build integration`): executor, logsource, supervisor, systemd | 3b to 3c |
| `tests/packaging/` | Package tests: scripts against stubs, package contents, VM lifecycle (D-102) | 3d-1 |
| `tests/vm/` | Vagrant VMs for root integration tests (`task test-vm`, D-085) | 3b-1 |
| `.github/workflows/` | `ci.yml` (lint, tests, builds, snapshot), `codeql.yml`, `scorecard.yml`; release in 3d | 1, 2c-4 |
| `docs/` | Operator, design and contributor documentation ([index](README.md)) | all |

## Configuration pipeline

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
  directory looked into while the path is resolved. Files are resolved
  through `os.Root`, and a symbolic link may only name an entry of its own
  directory (D-070). Duplicate and merge keys are rejected and every load
  is size-bounded (D-071).
- Errors are `*config.ValidationError{Problems, Warnings}` with
  `Problem{File, Path, Message}`; modules return the same type so every
  problem of every file is reported at once.

## Data model

### Events

`pkg/model.Event`, `internal/core/events` (Phase 2b). An event is
something that happened, emitted by a module and delivered to the core's
consumers. Fields (also the `event` object of the webhook payload,
[notifications.md](notifications.md)): `event_id`, `timestamp`,
`hostname`, `sentinel_version`, `module`, `source`, `source_type`,
`event_type`, `severity`, `state`, `previous_state`, `message`,
`correlation_id`, `metadata`, `attributes`.

- Registry: each event type belongs to one module and has a default
  severity; the core's `configuration_error` and `daemon_error` are
  pre-registered. Publishing an unregistered type, or a type as another
  module, is an error.
- Normalisation at the bus (ADR-0002, T-18): identifiers, message,
  metadata and attribute strings are cleaned (no control, format or
  separator characters, valid UTF-8) and shortened; attribute keys match
  `^[a-z0-9_.]{1,64}$` and values are JSON scalars, string lists or one
  nested level; the encoded event stays under 16 KiB (attributes, then
  metadata, then the message shrink), and the work spent on attributes is
  bounded by that cap. The bus stamps ID, timestamp, hostname and version;
  `Publish` returns only the ID, so emitters never hold the maps
  subscribers read.
- Delivery: each subscriber has a bounded queue; when it is full the
  oldest event is dropped and counted, so `Publish` never blocks. Order is
  kept per publishing goroutine (per source), not globally.
- Deduplication windows (ADR-0002) are added when a module needs them.

### State

`internal/core/state` (Phase 2b).

```
<daemon.state_dir>/            0750, checked like configuration (fstrust)
├── core/state.json            0700 directory, 0600 file
└── <module>/state.json
```

Each file is an envelope with the module's own schema version:

```json
{"schema_version": 1, "updated_at": "2026-10-08T12:00:00Z", "data": { … }}
```

- `Load[T]` returns a fresh `T`: no file gives the zero value
  (`FirstRun`). A corrupt file (bad JSON, missing or invalid version, data
  that does not decode or fails `T`'s `Validate`, JSON `null` when `T` is
  a pointer, over 16 MiB) is moved aside as `state.json.corrupt-<UTC
  time>` and gives the zero value (`Recovered`). A newer
  `schema_version` is an error and leaves the file untouched; so is an
  older one, unless the module migrates it through `LoadMigrating` (the
  supervisor migrates its schemas 1 and 2). A file or directory another
  user could write is an error.
- `Save` writes a temporary file in the module directory, `fsync`s it,
  renames it over `state.json` and `fsync`s the directory. Everything is
  resolved inside the opened state directory (`os.Root`).
- One `Store` per module: `Load` holds its lock from reading to
  quarantine, so a concurrent `Save` is never moved aside; `Save` refuses
  a state over the 16 MiB that `Load` would reject.
- State never holds secrets (ADR-0011).

### Notifications

`internal/core/notify` (Phase 2b). The dispatcher reads events, asks a
route function (built by the daemon from `notifications.core` and the
modules' item routes) which channels an event goes to, and queues it on
each. Every enabled channel has a bounded queue (drop oldest and count)
and one worker that delivers with retries and backoff on the injected
clock, applying `repeat_interval`. The webhook sender never follows
redirects, bounds each attempt by the channel timeout, and never quotes
the URL in errors. Contract and behaviour:
[notifications.md](notifications.md).

## Daemon

Phase 2c-1. `sentineld` loads the configuration (`daemon.Load`: central
file, module directories, `Configure` of every enabled module; nothing
changes on the system), builds its logger from `daemon.log`, then
`Daemon.Start`:

1. opens `daemon.state_dir` (checked, created if missing, D-074);
2. creates the event registry and bus, the notification dispatcher and
   two subscribers: the dispatcher and a logger that logs every event and
   keeps the last 256 for `core.events` (D-083); delivery runs on its own
   context, so it outlives a signal;
3. starts every enabled module in name order, giving each a
   `module.Runtime` bound to it.

`module.Runtime` holds what a module may use of the core: `Logger`,
`Clock`, `Events` (an emitter that can only register and publish in the
module's own name), `State` (a function that opens the module's state
store for the schema version it writes), `Secrets` (the
configuration's secret values, to mask text that comes from outside the
module, such as program output), `Audit` (records the module's own
privileged actions before and after they happen, in its own namespace,
ADR-0011) and `Fault` (reports a failure that is not the error of a call,
as `daemon_error` with two fixed codes). A module that implements
`module.Router` routes its events to channels; core events follow
`notifications.core`.

SIGTERM or SIGINT calls `Daemon.Stop`: running modules stop in reverse
order, sharing `daemon.shutdown_timeout` (a quarter is kept for the last
deliveries); the bus closes, the dispatcher delivers what is queued until
the deadline (then delivery is abandoned, even when a module's router
blocks), and the state directory closes. A log destination that blocks
every write (a full stderr pipe) still blocks sentineld: bounded
asynchronous logging is backlog B-004. A signal during start cancels it:
the module starting gets a cancelled context and no further module
starts. From the first signal on, a second one ends the process at once.

Module calls are isolated (D-075): each `Start` and `Stop` runs in its
own goroutine with a recovered panic and a time limit. A failure, a panic
or a timeout is logged (a panic with its frames, never its value) and
published as `daemon_error` (source: the module; no error text, which
stays in the local log after `Config.RedactText` has masked the values
the configuration knows to be secret). A module that fails to start gets
a bounded `Stop`; the others keep running. A call that does not return in
time is abandoned: Go cannot stop a goroutine, so the state directory is
then left open until the process exits. `Daemon.Snapshot` copies module
states, subscriber drops and channel counters (logged at stop, read by
`sentinelctl status`).

Under systemd, `cmd/sentineld` sends `READY=1` once `Start` has returned
without a cancelling signal and `STOPPING=1` before `Stop`, through
`internal/platform/sdnotify` (D-079); it removes `NOTIFY_SOCKET` from its
environment first, so nothing it starts can report for it. Reloads send
nothing. Operators: [operations.md](operations.md).

The process sets umask `027`. Exit codes: 0 clean stop or valid
configuration; 1 invalid configuration, failed start of the core, or an
unclean stop (a module failed or was abandoned at start or stop, or
delivery was abandoned); 2 usage.

## Control socket

Phase 2c-2. `sentinelctl` connects to `daemon.socket`, writes one request
and reads one response, each a JSON object on one line (`pkg/api`,
version 1):

```json
{"version":1,"command":"core.status"}
{"version":1,"result":{"version":"…","uptime_seconds":42,"modules":[…]}}
{"version":1,"error":{"code":"permission_denied","message":"…"}}
```

- Commands are `<namespace>.<command>` and declare their tier in
  `pkg/api`: `core.status`, `core.modules`, `core.config_show` (the
  redacted core configuration), `core.events` (the last 256 events, kept
  in memory by the log subscriber, D-083) and `core.audit` are `read`;
  `core.reload` is `operate`.
- Module commands (D-080): a module that implements `module.Commander`
  declares its commands (`<module>.<command>` and a tier) on the
  unconfigured module; the registry checks them at `Register`, and
  `Configure` refuses a module that declares commands without a
  `module.Handler`. The socket binds the commands of every module the
  binary knows once, at start, so neither the commands nor their tiers
  depend on the configuration. Each request goes to the module's running
  instance; a module not enabled, failed or stopping answers
  `unavailable`. Stopping a module (shutdown, reload, a failed start)
  first closes its commands, then waits for those in flight within the
  stop's time limit, so `Stop` never runs beside `Handle`. A panic in
  `Handle`, or in encoding its result (a module's `MarshalJSON` is module
  code too), is recovered and reported like one in `Start`
  (`daemon_error`, operation `command <name>`); the client gets
  `internal`.
- Authorization (ADR-0012, D-076): the server reads the peer's uid, gid
  and supplementary groups from the kernel (`SO_PEERCRED`,
  `SO_PEERGROUPS`; `internal/platform/peercred`), maps them to a tier
  (`internal/core/authz`: root and `admin_group` give admin,
  `operator_group` operate, anyone who could connect read; each tier
  includes the ones below) and checks it against the command's declared
  tier before running it. Nothing the client sends can raise its tier.
  Outside Linux there are no peer credentials: every request answers
  `unavailable`.
- Limits: requests at most 64 KiB, responses 16 MiB, one exchange 5 s
  (after authorization `supervisor logs` gets 15 s and `core.reload` up to
  20 min 5 s), 32 connections at once (more are closed at once). The
  decoder is strict (`encoding/json/v2`: no unknown or duplicate member,
  valid UTF-8, nothing after the object) and fuzzed.
- Audit (D-077): a request for an operate or admin command is recorded
  before it runs (`intent`, then `ok` or `failed`), or as `denied`; if the
  intent cannot be recorded the command does not run. Connections are
  limited per user (burst 10, then one per second; root is not limited
  and keeps the last slot).
- Socket file: the directory is created and checked like configuration
  (`fstrust.OpenDir`); `<socket>.lock` is locked (`flock`) so two daemons
  never race on a socket (the state directory has its own lock,
  `sentineld.lock`, taken by `state.OpenDir`: one daemon per state
  directory, whatever its socket, D-092). An existing socket is removed
  only if nobody listens on it; anything else is an error. After bind the
  mode is narrowed, the group set, then the final mode applied. The socket
  is acquired before any module starts and served after `Start`; `Stop`
  closes it first, letting running commands answer.

## Audit log

Phase 2c-3a. `internal/core/audit` appends one JSON record per line to
`<state_dir>/audit.jsonl`, synced before it counts, each chained to the
previous one by a SHA-256 over its exact bytes; `audit_head.json` holds
the last record and the anchor before the oldest kept one. Rotation keeps
five files of 10 MiB. At start-up every kept file is checked against the
head: a broken chain is reported (`core.status`, `audit_integrity_broken`)
and never repaired, and the log then refuses new records, so operate and
admin requests are refused until the operator archives the files and
restarts. Each record is also logged at `info`. The log is tamper
evidence and prevents nothing: root can rewrite everything (T-16).
Modules record their own privileged actions through
`module.Runtime.Audit`, with sentineld as the actor.

## Reload

Phase 2c-3b. `Daemon.Reload` serves `core.reload` and SIGHUP alike
(D-075, D-078). It takes the lifecycle gate that `Stop` also takes,
records `core.config_reload` (`intent`, with the running digest) and then
prepares everything without effect: load and validate the files, resolve
the access groups, build the new dispatcher if the channels changed. A
problem there keeps the running configuration. Otherwise it stops the
modules removed or changed (reverse order), publishes the new view (the
effective configuration, its redacted JSON and the routers, swapped as
one value), sets the access policy and the log level, hands the forwarder
the new dispatcher (the old one is cancelled; its queue is counted) and
starts the new instances in the new order. Keys bound at start-up keep
their running value. The whole reload has a budget of twice
`daemon.shutdown_timeout`, which also caps each module call and the wait
for the replaced dispatcher. A module whose `Stop` failed or did not
return is remembered (`unstopped`): it is never started again beside its
old instance, and `Stop` keeps the state directory and the socket lock.

A running instance that implements `module.Reconfigurer` (D-088, Phase
3b-4) is not stopped: once the new view and dispatcher are published, it
is offered the new instance in the new start order, among the starts,
with a context that ends a quarter before the call's limit. `nil` reports
it `reconfigured`; `module.ErrRestart` stops it there and starts the new
instance in its place; another error keeps it running and `pending`, so
the next reload offers it again with unchanged files; a panic or no
return marks it `unstopped`. A module's error text is masked with the
published configuration's secrets and with those its instance may know
(its own configuration's, and the new one's during a reconfiguration or
after a partial one).

## Supervisor module

`internal/modules/supervisor` is one package (D-080 to D-082, D-084,
D-086 to D-090, D-093 to D-100). Operator reference:
[supervisor.md](supervisor.md).

| File | Holds |
|---|---|
| `module.go` | Module contract, lifecycle (`Start`, `Stop`), the instance and its locks |
| `config.go`, `config_http.go`, `config_process.go`, `config_unit.go`, `config_job.go` | Schema, defaults and located problems for each type of entry |
| `check.go`, `policy.go` | `Checker`, `Result`, fixed failure codes; the pure transition function |
| `service.go` | The loop of a checked service |
| `http.go` | The HTTP checker |
| `unit.go`, `unit_restart.go` | systemd services: reading a unit, restart attempts (D-093, D-095) |
| `process.go`, `launcher.go`, `memory.go` | Process services: launch, stop, cleanup of recorded groups, memory samples (D-086, D-090) |
| `recovery.go` | The recovery budget (D-087) |
| `job.go`, `job_slot.go`, `job_run.go` | Jobs: schedule loop, slots and admission, runs (D-097, D-098) |
| `store.go`, `store_job.go` | State file, digests, migration, reconciliation |
| `events.go` | Event types and builders |
| `commands.go`, `operate.go`, `logs.go` | Read commands, operator commands (D-089), `supervisor logs` (D-099) |
| `output.go` | Program output to the journal or the log (D-100) |
| `reconfigure.go` | Reload in place (D-088) |

- Loop: one goroutine per service, owned by a context of the module (not
  Start's). It checks at once, then one `check_interval` after each check
  ends. A check takes a slot of a channel semaphore
  (`max_concurrent_checks`) and runs in its own goroutine, which gives the
  slot back only when `Check` returns. The loop waits for the result, the
  service `timeout` (injected clock) or the module's end. On timeout the
  check counts as failed (`timeout`) and stays stalled: no new call for
  the service until it returns, a `timeout` counted each interval.
- Policy: `step(streak, ok, policy)` is pure: N consecutive failures give
  `failed` (`service_failed`), M consecutive successes `healthy`
  (`service_recovered`); the first success is silent; a check cancelled by
  `Stop` is not counted.
- Persist, then publish: on a transition the state file is written before
  the event, whose id (drawn first) is the incident's correlation id;
  counters alone are saved at most every 60 s. A failed save is shown in
  status and retried; supervision goes on.
- Audit before acting: every launch and restart sentineld makes by itself
  needs its intent recorded through `Runtime.Audit` first; a refused
  intent blocks it. A memory stop tries to record its intent and goes
  ahead if it cannot. The cleanup of a previous run's group is recorded after it,
  when the log takes records. Stops at shutdown, at a reload or at a job's
  timeout have no record of their own.
- Stop: cancel, join the loops, save, then wait for the checks still
  running within the daemon's limit: a check is never left running behind
  a clean stop (D-078 (3)).
- Reconfigure: each service loop has its own lifetime inside the
  module's (cancel, done, its checks). A reload keeps unchanged services,
  retires changed and removed ones (loop, checks, process) in parallel
  within its budget, swaps the service list, the routes (an
  `atomic.Pointer`) and the redaction under the lock, saves, then starts
  the new loops; a service that does not end is kept in `retired` and its
  name blocked until sentineld restarts.
- Commands: `supervisor.list` and `supervisor.status` (tier read), results
  in `pkg/api`, copied under the lock without waiting for a check or the
  disk.
- Operator commands (D-089): `supervisor.start|stop|restart|enable|
  disable|reset` (tier operate, `TargetsName`: the transport records the
  validated name as the audit target and puts the request's correlation
  id in the context). `Handle` refuses what the service's state does not
  allow, then runs the action in a goroutine of the instance (`workers`,
  `life`, the service's own bound) and waits for it at most two seconds.
  One action at a time and never beside `Reconfigure` (`ops`, a one-slot
  channel). An action first ends the service's loop and joins it, then
  decides with nothing else changing the service, then starts a new loop
  with a fixed `gate` (configuration, operator, stopped, not armed): a
  gated loop cleans up and returns; a loop that finds a running process
  adopts it. Permissive flags are saved before they apply (`commit`).
- HTTP checker: built at `Configure` (the CA bundle is read there, so
  `validate` reports it); one transport per service with no proxy, no
  keep-alive, HTTP/1.1, bounded headers and body reads, redirects only
  within the origin; `address` replaces the dial target, never the URL. A
  finished check closes the connection attempts net/http keeps after a
  cancelled request. Errors become fixed codes.

## Logging

`internal/core/logging` returns a `*slog.Logger`.

| Format | Output |
|---|---|
| `text` | `time=… level=INFO msg=… key=value` |
| `json` | one JSON object per line |
| `journal` | `<6>level=INFO msg=… key=value`: no timestamp, syslog priority prefix parsed by journald (`<3>` error, `<4>` warn, `<6>` info, `<7>` debug) |
| `auto` | `journal` when stderr's device:inode equals `$JOURNAL_STREAM` (set by systemd), otherwise `text` |

Attributes whose key looks secret (`token`, `password`, `authorization`,
`api_key`, `secret`, `cookie`, …) are replaced by `[REDACTED]` at any
nesting level. sentineld passes a `*slog.LevelVar`, which a reload sets
in place.

## Interfaces

The interfaces that exist are documented in their packages (`go doc
./internal/core/module`, for example) and follow the rules in
[AGENTS.md](../AGENTS.md): declared by their consumer, one to three
methods. The firewall's planned interfaces (`Observer`, `Committer`) are
sketched in ADR-0005, and the blocklists' `SetUpdater` in ADR-0008.
