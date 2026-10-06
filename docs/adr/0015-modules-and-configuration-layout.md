# ADR-0015: Core, platform and modules; configuration layout

- Status: Accepted
- Date: 2026-10-06
- Phases: 2 (framework and loader), every module phase after it
- Amends: ADR-0001 (package layout), ADR-0002 (event fields and names),
  ADR-0003 (where safety gates live), ADR-0011 (state files)
- Supersedes: D-007 (`conf.d` fragments), D-042 (main-file-only sections)

## Context

Sentinel started as a service watchdog and grew into several product
areas: supervision, host firewall with CrowdSec/WAF integration, and,
later, remote management through a dashboard and host-integrity
monitoring in the role of a Wazuh/OSSEC agent. More areas may follow.

The prototype code (old Phase 1) was designed for supervision only: one
flat configuration (`settings`, `notifications`, `supervisors`),
supervisor-centric events and one state file. ADR-0001 already chose a
modular monolith but organised the code by *domain*, not by
*user-visible module*, and kept every enforcing section in the main file
(D-042).

The maintainer wants:

- a **core** with the services every module shares (notifications, log
  reading, events, state, control socket);
- **modules** enabled or disabled by configuration;
- one daemon (`sentineld`) and one control CLI (`sentinelctl`);
- a central configuration file with the common settings and the module
  switches, and one directory per module for its own configuration.

## Options considered

### Configuration layout

| Option | Description | Verdict |
|---|---|---|
| A. One file, one section per module | `sentinel.yaml` holds everything (old D-042 for network sections). | Simple for tiny hosts; large files; no drop-in file per service; safety gates mixed with content. |
| B. `conf.d/` fragments of any kind | Any fragment may contain any section (old D-007, limited to supervisors). | No ownership: a fragment could arm enforcement or change the socket. |
| **C. Central file + one directory per module, gates central** | Central file: core settings, module switches and safety gates. Module directories: what the module does. | **Chosen.** |
| D. Central file + module directories holding their own gates | Each module fully self-contained. | A dropped-in file could arm the firewall. Rejected. |

### Module mechanism

Compile-time modules registered explicitly by the daemon (chosen); Go
`plugin` (rejected in ADR-0001: cgo, toolchain coupling); out-of-process
modules (rejected for now, seam kept as in ADR-0001).

## Decision

### 1. Three layers

```
cmd/sentineld, cmd/sentinelctl
        │
internal/daemon        wiring: module registry, lifecycle, reload — the only package that imports modules
        │
        ├── internal/modules/   supervisor · firewall · (remote) · (monitor)
        │                       never import each other; interact through events or interfaces injected by the daemon
        ├── internal/platform/  adapters to the OS and external systems, shared by modules:
        │                       executor · privilege · systemd · logsource · docker · podman · kubernetes
        └── internal/core/      daemon services and pure libraries:
                                config · module · events · state · notify · audit · authz · transport ·
                                clock · logging · redact · cronexpr · netspec
                                        └──▶ pkg/model, pkg/api
```

Dependency rules, enforced by an architecture test (`go list -deps`):

- `core` imports only `core`, `pkg/*` and approved dependencies;
- `platform` imports `core` and `pkg/*`, never `modules` or `daemon`;
- a module imports `core`, `platform`, `pkg/*` and its own sub-packages,
  never another module or `daemon`;
- only `daemon` and `cmd/*` see every module.

"Common code" means `core` (services of the daemon) plus `platform`
(shared adapters): log reading, for example, is `platform/logsource`,
used by the supervisor (`logs <name>`), firewall WAF adapters and the
future monitor.

### 2. Module contract

Defined in `internal/core/module`; a sketch to be refined in Phase 2:

```go
type Module interface {
    Name() string // "supervisor": config key, directory name, CLI namespace, event field
    // Configure decodes and validates the module's central block and its
    // directory. No goroutines, no system changes. Returns all problems.
    Configure(c config.ModuleConfig) (Configured, []config.Problem)
}

type Configured interface {
    Start(ctx context.Context, rt Runtime) error // rt: logger, clock, events, state, notify, audit
    Stop(ctx context.Context) error
    Status(ctx context.Context) model.ModuleStatus
    Commands() []api.Command // CLI commands with their authorization tier
}
```

- The daemon holds an explicit list of module factories. Heavy modules can
  be excluded with build tags (`nofirewall`, `nokubernetes`); an excluded
  or not-yet-implemented module named in the configuration is an error
  ("planned but not implemented" / "not built into this binary").
- `Configure` is pure, so `sentinelctl validate` and reload use the same
  path: on SIGHUP the whole configuration is loaded and configured again;
  any problem keeps the running configuration (reload granularity: Q-016).
- A module's panics are recovered by the daemon, reported as module
  status `error` and an event; other modules keep running.

### 3. Configuration layout

```
/etc/sentinel/
├── sentinel.yaml      core: daemon, notifications, module switches and safety gates
├── supervisor/        read only when modules.supervisor.enabled is true
│   ├── nginx.yaml
│   └── backup.yaml
└── firewall/          read only when modules.firewall.enabled is true
    └── policies.yaml
```

```yaml
# /etc/sentinel/sentinel.yaml
version: 1

daemon:
  socket: /run/sentinel/sentinel.sock
  state_dir: /var/lib/sentinel
  log: { level: info, format: auto }
  access: { operator_group: sentinel }

notifications:
  channels:
    - name: ops
      type: webhook
      url: ${OPS_WEBHOOK_URL}

modules:
  supervisor:
    enabled: true
  firewall:
    enabled: true
    mode: read_only        # safety gates live here (ADR-0003)
    dry_run: true
```

```yaml
# /etc/sentinel/supervisor/nginx.yaml
version: 1
services:
  - name: nginx
    type: systemd
    unit: nginx.service
    recovery: { action: restart, max_attempts: 3, window: 10m }
    notifications: [ops]
```

Rules:

1. **Only the central file configures the core** (daemon, access,
   notifications) and the **module switches and safety gates**: `enabled`
   and, for enforcing modules, `mode`, `dry_run` and the other gates of
   ADR-0003/ADR-0005 (`protected_access`, `safety_timeout`,
   `confirm_required`). A module directory describes *what* the module
   does and can never raise its own privileges.
2. **Risky opt-ins require central permission.** An `allow_*` key inside
   a module directory (e.g. a rule's `allow_protected_access_block`,
   D-049) is a configuration error unless the central block of that
   module allows it. Exact keys are defined with the module.
3. **Disabled module = directory not read.** Its files are not parsed and
   their `${VAR}` references are not resolved. `sentinelctl status`
   reports ignored directories; `sentinelctl validate --all` checks them.
4. **Enabled module without directory:** the module decides — the
   supervisor starts empty with a warning; the firewall refuses to start.
5. **Module names are closed.** An unknown key under `modules` is an
   error; a known but unimplemented module (`remote`, `monitor` until
   their phases) is "planned but not implemented".
6. **Inside a module directory:** regular `*.yaml`/`*.yml` files in
   lexical byte order, hidden files ignored, subdirectories ignored with
   a warning, each file declares `version: 1`; lists are merged and
   names are unique within the module; the module's `settings` block may
   appear in **at most one** file.
7. **Each module owns its schema.** The core reads files, enforces size
   limits, single document, no duplicate keys, expands `${VAR}` in scalar
   values (D-006) and hands the module a `config.ModuleConfig` (its
   central block minus `enabled`, plus its documents). Modules decode
   through a strict helper (unknown keys with line numbers, D-008) and
   never import the YAML library directly. `internal/core/config`
   imports no module.
8. **File ownership and modes are checked at load:** the central file and
   module directories must be owned by root (or the daemon user) and not
   group- or world-writable (threat model T-02).
9. Fixed paths: module directories are siblings of the central file
   (`<dir>/<module>/`); there is no per-module path override.

### 4. Supervisor vocabulary (refines D-062)

The supervisor's directory holds `settings`, `services` (types `systemd`,
`process`, `http`, later `port`, `mount`, `openrc`, …) and `jobs` (type
`cron`). Services and jobs share one name space inside the module.
Events: `service_failed`, `service_recovered`, `recovery_started`,
`recovery_exhausted`, `job_succeeded`, `job_failed`, `job_timeout`.

### 5. Events, state and CLI

- **Events** (amends ADR-0002): every event carries `module` (`core`,
  `supervisor`, `firewall`, …) next to `source` and `source_type`. Each
  module registers its event types at start; unknown types fail
  validation. Notification routing can filter on `module`, `event_type`
  and `severity`.
- **State** (amends ADR-0011): one directory, `daemon.state_dir`, with
  `core/` and one subdirectory per module (`supervisor/state.json`,
  `firewall/transactions/`, …). Each module's state file has its own
  `schema_version`, so modules evolve and recover from corruption
  independently. The atomic store, quarantine and downgrade protection
  (D-015) are core services. Closes Q-010.
- **CLI**: core commands (`status`, `modules`, `events`, `validate`,
  `config show`, `reload`, `audit`, `version`) and module commands under
  the module's name (`sentinelctl supervisor restart nginx`,
  `sentinelctl firewall plan`). The protocol names commands
  `<module>.<command>`; each declares its tier (ADR-0012), enforced by
  the core.

## Consequences

- The prototype configuration, event and state code is replaced in
  Phase 2; generic pieces are ported deliberately (D-064). No migration
  code: nothing has been released.
- ADR-0001's decision (modular monolith) stands; its package layout is
  replaced by §1. ADR-0003's modes stand; their keys move to
  `modules.<name>`. Phase numbers in ADR-0001…0014 refer to the old
  roadmap; PLAN.md maps them to the new phases.
- Adding a module touches `internal/modules/<name>`, the daemon's factory
  list and the documentation — not the core.
- The layout prepares the remote module (Phase 5): a dashboard can deliver
  module directories while module switches and gates stay in the local,
  root-owned central file.
- Small hosts need two files instead of one. Accepted: the central file
  stays short and every module example ships ready to copy.

## Follow-ups

- Phase 2a: loader, `config.ModuleConfig`, strict helper, module
  registry, architecture test, ownership checks.
- Phase 2b: event `module` field, per-module state stores.
- Phase 2c: namespaced commands, tiers, reload (Q-016).
- Phase 4a: firewall central gates and `allow_*` permissions.
