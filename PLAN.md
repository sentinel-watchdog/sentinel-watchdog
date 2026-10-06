# Sentinel — Development Plan

This file is the single source of truth for multi-session development.
Every session MUST:

1. read this file first (then `CLAUDE.md` / `AGENTS.md` for conventions);
2. pick the first phase whose status is not `done`;
3. keep the **Status** checkboxes, the **Decision log** and the
   **Open questions** up to date before ending the session;
4. finish every phase with: `task fmt vet test test-race lint build`,
   documentation updates, and a short entry in `CHANGELOG.md` (Unreleased).

Never mark a feature as done when only a stub exists. Planned-but-missing
features must fail loudly (configuration error or `unsupported` status).

---

## 1. Product summary

Sentinel is a Linux service and infrastructure watchdog written in Go.
It watches systemd services, supervised processes, scheduled jobs and
HTTP(S) endpoints (MVP), and later ports, mounts, logs and resources.
On failure it records the event, applies a bounded recovery policy
(restart with delay/backoff/window/max attempts), escalates through
webhook notifications when recovery is exhausted, and notifies again when
the target becomes healthy.

Binaries: `sentineld` (daemon) and `sentinelctl` (CLI over a Unix socket).

## 2. Binding decisions (from the specification)

| Topic | Decision |
|---|---|
| Language | Go, minimum **1.27** |
| License | Apache License 2.0 |
| Configuration | YAML only, `version: 1`, main file + optional `conf.d/` |
| State | JSON file, atomic writes, versioned schema |
| Scheduler | Traditional 5-field cron syntax |
| Notifications | Generic JSON webhook only (Slack/Teams later) |
| systemd | Monitor and restart **existing** units only; never create/modify/enable/disable units |
| Daemon ↔ CLI | Unix domain socket, default `/run/sentinel/sentinel.sock`, no TCP |
| CI | GitHub Actions |
| Targets | RHEL 8+, Ubuntu 22.04+ (systemd), Alpine (OpenRC); amd64 + arm64 |
| Packages | tarball, DEB, RPM (APK post-MVP) |

## 3. Decision log (refinements of the original specification)

Each decision records *why*, so later sessions do not re-litigate it.

| ID | Decision | Rationale |
|---|---|---|
| D-001 | YAML library: `go.yaml.in/yaml/v3`. | `gopkg.in/yaml.v3` is archived; `go.yaml.in/yaml/v3` is the maintained continuation by the YAML org, same API. Only external runtime dependency. |
| D-002 | Task runner is **Taskfile** (`Taskfile.yml`, go-task v3) instead of a Makefile. Task names mirror the Makefile targets requested in the spec (`build`, `test`, `test-race`, `vet`, `fmt`, `lint`, `clean`, `install`, `uninstall`, `package`, `package-deb`, `package-rpm`, `release`). | Requested at Phase 1 kickoff. CI installs task via `arduino/setup-task`. |
| D-003 | All monitors use `check_interval` (the draft's HTTP-only `interval` key is not accepted). | One name for one concept. |
| D-004 | `notifications` has a short form (list of channel names) and a long form `{channels: [...], events: [...]}`. Event filters use the canonical event type names (`job_failed`, not `failure`). The draft key `on` was renamed `events`. | Same vocabulary as the webhook payload `event_type`; `on` is a YAML 1.1 boolean. |
| D-005 | Command execution: `command` (absolute path, exec'd directly with `args`) **xor** `script` (inline text run by an explicit `shell`, default `/bin/sh`). No implicit shell, no `$PATH` lookup. | Removes command/script ambiguity and command-injection risk. |
| D-006 | `${VAR}` expansion happens on parsed scalar **values** (never keys, never raw text). Undefined variable = error. `$${` escapes a literal `${`. No defaults syntax (`${VAR:-x}`) in v1. | Prevents YAML structure injection through env values; fails closed. |
| D-007 | `conf.d/*.yaml` / `*.yml` fragments are loaded in lexical byte order after the main file; hidden files and other extensions are ignored. Fragments must declare `version: 1` and may only contain `notifications` and `monitors`; `settings` is main-file only. | Deterministic merge with no override semantics to reason about. |
| D-008 | Unknown keys, duplicate keys, multiple YAML documents and unimplemented monitor/notification types are **errors**. Planned types report "planned but not implemented". | Strict validation; no silent ignore. |
| D-009 | Output targets for supervised commands: `log` (default, lines forwarded to the sentineld logger → journald), `file` (append, configurable mode), `discard` (explicit opt-out). | "No silent loss of errors" and journald support without writing to the journal socket directly. |
| D-010 | Durations are strings (`15s`, `2h`); integers are rejected. Sizes accept integers (bytes) or unit strings (`512MiB`, `1GB`). File modes are quoted octal strings (`"0660"`). | YAML 1.2 octal/integer ambiguity. |
| D-011 | Monitor and channel names match `^[a-z0-9][a-z0-9._-]{0,62}$`. | Safe in CLI args, file names, log fields and URLs. |
| D-012 | `recovery` is allowed on `systemd` and `process` monitors only in the MVP. `failure_policy` (consecutive failures / successes) is allowed on `systemd` and `http`. | HTTP has nothing to restart until the `execute` action exists. |
| D-013 | Recovery `cooldown`: after `exhausted`, wait `cooldown` then reset attempts and resume recovery. `0` = stay exhausted until `sentinelctl reset`. | Defines the spec's otherwise unspecified "cooldown". |
| D-014 | Restart-attempt timestamps are persisted in the state file. | Restart-loop protection must survive daemon restarts. |
| D-015 | State file: a newer `schema_version` is refused (never overwritten); a corrupt file is quarantined as `state.json.corrupt-<UTC timestamp>` and a fresh state is started. | Downgrade safety; recover without losing evidence. |
| D-016 | Logging format `auto`: `journal` when `JOURNAL_STREAM` matches stderr (no timestamps, `<N>` syslog priority prefixes), `text` otherwise. `json` and `text` can be forced. | journald by default under systemd, readable in foreground. |
| D-017 | `time/tzdata` is embedded. | Alpine/minimal containers often lack `/usr/share/zoneinfo`. |
| D-018 | Cron: 5 fields plus `@yearly/@annually/@monthly/@weekly/@daily/@midnight/@hourly`. No `@reboot`, no `every 5m`. `concurrency_policy: forbid` (default) \| `allow` \| `replace`. `missed_runs: skip` (default) \| `run_once`. | Traditional syntax; restart without duplicate runs. |
| D-019 | HTTP `follow_redirects` defaults to `false`; TLS verification always on unless `tls.insecure_skip_verify: true` (emits a validation warning). | Safe defaults, explicit opt-in. |
| D-020 | Operator `sentinelctl enable/disable` is persisted in state. `enabled: false` in config always wins. | Predictable precedence. |
| D-021 | Resource limits (`limits`) are modelled and validated now, enforced only for Sentinel-supervised processes (sampling `/proc`) in Phase 3. Anything not enforced reports `unsupported`. cgroups v2 is post-MVP. | "Do not claim support for a limit that is not applied." |
| D-022 | Code, docs and commit messages are in English. | Open-source audience. |
| D-023 | Executables built with `CGO_ENABLED=0` (static). | One binary for RHEL 8 / Ubuntu / Alpine (musl). Consequence: see R-004. |
| D-024 | Packaging (Phase 6) uses **GoReleaser** + its embedded **nFPM** for tarball/DEB/RPM, driven from Taskfile and the release workflow. | Maintained, reproducible (`-trimpath`, `mod_timestamp`), one config for all formats; APK is a one-line addition later. |

## 4. Risks and ambiguities

| ID | Risk | Mitigation |
|---|---|---|
| R-001 | Restarting systemd units and switching process users needs privileges. A dedicated unprivileged user cannot run `systemctl restart` without polkit/sudo, nor `setuid` children. | Default unit runs as root with a reduced `CapabilityBoundingSet` and strict sandboxing; document an unprivileged mode (polkit rule scoped to listed units, no `user:` in process monitors). Decide final defaults in Phase 6. |
| R-002 | `ProtectSystem=strict` may block writes needed by supervised processes (they inherit the sandbox). | Document `ReadWritePaths=` drop-ins; ship conservative defaults. |
| R-003 | Cron jobs across daemon restarts: duplicates or missed runs. | Persist last scheduled slot per job (D-018), re-compute next slot on start. |
| R-004 | With `CGO_ENABLED=0`, `os/user` reads only `/etc/passwd`/`/etc/group` (no SSSD/LDAP). | Accept numeric `uid`/`gid` in `user`/`group`; document. |
| R-005 | Supervised processes must not survive Sentinel (orphans). | Own process group + `Pdeathsig` on Linux, SIGTERM → timeout → SIGKILL to the group on shutdown. |
| R-006 | Secrets may leak through error strings (URLs with tokens, headers). | `internal/redact` used by logging, `config show`, events and notifications. |
| R-007 | systemd is absent on Alpine. | systemd monitors report `unavailable` at runtime (not a config error, so one config can be shared); OpenRC monitor post-MVP. |
| R-008 | Webhook outages could block monitors. | Async bounded notification queue, retries with backoff, drop + log when full. |

## 5. Target repository layout

```
cmd/sentineld/          daemon entrypoint                       (Phase 5)
cmd/sentinelctl/        CLI entrypoint                          (Phase 5)
internal/config/        YAML model, loader, env expansion, validation   ✔ Phase 1
internal/logging/       slog setup, journal handler, redaction hook      ✔ Phase 1
internal/redact/        secret redaction helpers                         ✔ Phase 1
internal/state/         JSON state model + atomic store                  ✔ Phase 1
internal/scheduler/cronexpr/ cron expression parser (Next(): Phase 4)    ✔ parse
internal/version/       build metadata (ldflags)                         ✔ Phase 1
internal/events/        event bus, deduplication                 (Phase 2)
internal/recovery/      recovery engine                          (Phase 2)
internal/notification/  provider interface + webhook             (Phase 2)
internal/executor/      process spawning, creds, output sinks    (Phase 3)
internal/privilege/     uid/gid resolution, capability checks    (Phase 3)
internal/monitor/{common,systemd,process,http,cron}/              (Phases 3–4)
internal/health/        resource sampling (/proc)                (Phase 3)
internal/daemon/        orchestration, reload                    (Phase 5)
internal/lifecycle/     signals, shutdown                        (Phase 5)
internal/transport/     Unix socket server/client                (Phase 5)
pkg/model/              public types: events, states             ✔ Phase 1
pkg/api/                versioned socket protocol                (Phase 5)
configs/                example configuration                    ✔ Phase 1
deploy/systemd, deploy/openrc                                    (Phase 6)
packaging/              nFPM/GoReleaser assets, scripts          (Phase 6)
docs/                   architecture, configuration (✔), others  (by phase)
.github/workflows/      ci.yml (Phase 2), release.yml (Phase 6)
```

## 6. Core data model (summary)

Full detail: `docs/architecture.md` and `docs/configuration.md`.

- **Config** → `Settings`, `[]NotificationChannel`, `[]Monitor`.
  `Monitor` = `MonitorCommon` (name, type, enabled, description,
  notifications) + exactly one spec (`Systemd`, `Process`, `HTTP`, `Cron`).
- **Event** (`pkg/model`): id, timestamp, hostname, version, monitor
  name/type, state, previous state, event type, message, counters,
  last error, metadata. Also the webhook payload body.
- **Monitor states**: `unknown, starting, running, healthy, failing,
  failed, recovering, exhausted, stopped, disabled`.
- **Capability status**: `supported, unsupported, unavailable,
  permission_denied, error`.
- **State file**: `schema_version`, `updated_at`, `monitors{name →
  MonitorState}`, `events` (bounded ring). `MonitorState` holds counters
  (restart_count, failure_count, consecutive_failures,
  consecutive_successes, total_recoveries), restart attempt timestamps,
  timestamps (last_check, last_transition, last_failure, last_recovery),
  last error/event/exit code, job info for cron, bounded history.

## 7. Planned interfaces (to be introduced in their phases)

Small, defined where consumed. Sketches only — refine when implemented.

```go
// internal/monitor/common — implemented by each monitor type.
type Checker interface {
    Check(ctx context.Context) Result // one probe (systemd, http)
}
type Supervisor interface {
    Run(ctx context.Context, report func(Result)) error // long-running (process, cron)
}

// internal/recovery
type Action interface {
    Recover(ctx context.Context) error // restart a unit/process
}

// internal/notification
type Notifier interface {
    Notify(ctx context.Context, ev model.Event) error
}

// internal/executor — abstracts exec for tests (systemctl mocking).
type Runner interface {
    Run(ctx context.Context, cmd Command) (Output, error)
}

// internal/state — consumed by the daemon.
type Store interface {
    Load() (*state.File, LoadReport, error)
    Save(*state.File) error
}

// Time source injected everywhere timing matters (recovery, cron).
type Clock interface { Now() time.Time; NewTimer(d time.Duration) Timer }
```

## 8. Phases

Status legend: `todo` · `in progress` · `done`.

### Phase 1 — Foundation · `done` (2026-10-05)

- [x] PLAN.md, repository layout, LICENSE (Apache-2.0), README, CHANGELOG
- [x] `go.mod` (Go 1.27), Taskfile, golangci-lint v2 config, `.gitignore`
- [x] Config model (`internal/config`): types, custom scalars (Duration, ByteSize, FileMode)
- [x] YAML loading: main file + `conf.d`, deterministic order, size limit, single document
- [x] Strict decoding (unknown keys with line numbers), polymorphic monitors
- [x] `${VAR}` expansion with `$${` escape, fail on undefined
- [x] Defaults + rigorous validation (all problems collected, path-qualified), warnings
- [x] Cron expression parser for validation (`internal/scheduler/cronexpr`)
- [x] Redacted view of the config (for `config show`)
- [x] Logging (`internal/logging`): levels, text/json/journal/auto, redaction
- [x] Event model and monitor/capability states (`pkg/model`)
- [x] State model + atomic JSON persistence, corrupt-file recovery, retention
- [x] Unit tests for all of the above; example config validated by a test
- [x] docs/architecture.md, docs/configuration.md

Phase 1 notes (carry forward):

- No binaries yet; `task build` compiles packages only. Taskfile tasks
  `install`, `uninstall`, `package*`, `release` are added in Phase 6.
- `config.Load` returns `*ValidationError` (problems + warnings);
  `Config.Redacted()` is ready for `sentinelctl config show`.
- `cronexpr` only parses; `Next()` with DST handling is a Phase 4 task.
- `limits` are validated but not enforced; the runtime must report
  `unsupported` until Phase 3 lands.
- `logging.Format` values equal `config.LogFormat` values; convert with
  `logging.Format(cfg.Settings.LogFormat)`.
- Verified: tests pass on macOS (race) and in `golang:1.27-alpine` as a
  non-root user; cross-builds for linux/amd64 and linux/arm64.

### Phase 2 — Engine core (pure logic) · `todo`

- [ ] `.github/workflows/ci.yml`: gofmt check, vet, golangci-lint, test, race, build matrix amd64/arm64, govulncheck
- [ ] `Clock` abstraction + fake clock for tests
- [ ] `internal/events`: in-process bus, per-monitor dedup (suppress identical event while condition persists)
- [ ] `internal/recovery`: policy evaluation (max_attempts in window, delay, fixed/exponential backoff with max_delay, stable_after reset, cooldown, exhausted), restart-loop protection, emits `recovery_started` / `recovery_exhausted` / `monitor_recovered`
- [ ] `cronexpr.Schedule.Next(time.Time)` with timezone and DST tests
- [ ] `internal/notification`: Notifier interface, async dispatcher (bounded queue), webhook provider (method, headers, timeout, retry + backoff, accepted status codes, redaction, never blocks monitors)
- [ ] Tests: max attempts, backoff, stable_after, dedup, webhook retry/timeout (httptest)
- [ ] docs/notifications.md

### Phase 3 — Execution, process and systemd monitors · `todo`

- [ ] `internal/executor`: direct exec / explicit shell, env, working dir, uid/gid (`SysProcAttr.Credential`), process group, `Pdeathsig`, stop signal → timeout → SIGKILL to group, exit code + signal capture, output sinks (log/file/discard) with secure file modes
- [ ] `internal/privilege`: user/group resolution (names + numeric), "requires root" checks with clear `permission_denied`
- [ ] `internal/monitor/common`: Result, Checker/Supervisor contracts, failure-policy counter
- [ ] `internal/monitor/systemd`: `systemctl show -p LoadState,ActiveState,SubState,Result,ExecMainStatus,NRestarts --value`, `is-active`; not-found / inactive / failed / unknown; restart with timeout; journal excerpt via `journalctl -u … -n N -o cat`; `unavailable` when systemd absent. Tests with mocked Runner.
- [ ] `internal/monitor/process`: supervised foreground process, startup grace, crash detection, recovery integration
- [ ] `internal/health`: /proc CPU & RSS sampling for supervised processes; limit actions log/notify/restart/stop/kill with `sustained_for`
- [ ] Tests: lifecycle, exit codes, signals, SIGKILL escalation, no orphans (Linux-only tests behind build tag where needed)
- [ ] docs/monitors.md (systemd, process)

### Phase 4 — HTTP and cron monitors · `todo`

- [ ] `internal/monitor/http`: method/headers/body, timeout, status codes, body_contains with `body_max_bytes` limit, TLS (CA file, server name, min version), redirect policy, consecutive failures / successes, redacted errors
- [ ] `internal/scheduler`: cron runner with timezone, overlap policy, missed-run policy, persisted last slot
- [ ] `internal/monitor/cron`: executes via executor; duration, exit code; success/failure/timeout events
- [ ] Tests: status, timeout, body match, oversize body, TLS, scheduling, overlap, restart without duplicates
- [ ] docs/monitors.md (http, cron)

### Phase 5 — Daemon, socket protocol, CLI · `todo`

- [ ] `pkg/api`: versioned JSON request/response protocol (newline-delimited, `protocol_version`, structured errors: `unknown_command`, `not_found`, `permission_denied`, `invalid_request`, `unsupported_version`, `internal`)
- [ ] `internal/transport`: Unix socket server (mode/group from config, stale socket handling, SO_PEERCRED authorization, deadlines), client with timeouts
- [ ] `internal/daemon`: builds monitors from config, wires recovery/events/notifications/state, periodic + on-change state saves
- [ ] `internal/lifecycle`: SIGTERM/SIGINT graceful shutdown, SIGHUP reload (invalid config keeps old; unchanged monitors keep running; diff-based start/stop), SIGUSR1 diagnostic dump, umask 027, `sd_notify` readiness (optional, no dependency)
- [ ] `cmd/sentineld`: flags (`-config`, `-config-dir`, `-validate`), version
- [ ] `cmd/sentinelctl`: status [name], list, start/stop/restart/enable/disable/reset <name>, reload, validate (offline), config show (redacted), events, logs <name>
- [ ] Tests: protocol, socket permissions, CLI against in-process server, reload, shutdown, goroutine-leak checks
- [ ] docs/operations.md, docs/security.md, docs/troubleshooting.md

### Phase 6 — Deployment, packaging, release · `todo`

- [ ] `deploy/systemd/sentineld.service` (hardened; capabilities documented)
- [ ] `deploy/openrc/sentineld` (supervise-daemon, `need localmount`, `after net`)
- [ ] GoReleaser config: tarballs + DEB + RPM for amd64/arm64, ldflags version, reproducible builds
- [ ] Package scripts: create `sentinel` group, dirs with correct modes, config as `config|noreplace`, keep state on upgrade/uninstall
- [ ] Taskfile `install`, `uninstall`, `package*`, `release`
- [ ] `.github/workflows/release.yml` on semver tags; tarball content verification; artifact upload
- [ ] Install docs per distro

### Phase 7 — Hardening and v0.1.0 · `todo`

- [ ] Integration tests (`//go:build integration`) in Linux containers: real systemd (Ubuntu), OpenRC (Alpine)
- [ ] Security review (gosec, path validation, socket auth), fuzz tests for config and protocol
- [ ] Documentation pass, README limits/roadmap, CHANGELOG `0.1.0`

### Post-MVP roadmap

port, mount, log, advanced resource and `process_group` monitors; OpenRC
service monitor; cgroups v2 limits; recovery action `execute`; Slack and
Teams providers; Prometheus metrics; local HTTP API; APK package;
dashboard.

## 9. Open questions

- Q-001: Default service user for sentineld (root + sandbox vs dedicated
  user + polkit). Proposed default in R-001; confirm in Phase 6.
- Q-002: Should `sentinelctl` write operations require root/`sentinel`
  group membership only, or also an allow-list in config? Proposed:
  socket mode/group + SO_PEERCRED (read-only for non-members is not
  possible with one socket; consider two sockets post-MVP).
- Q-003: GitHub owner for the module path. `github.com/sentinel-watchdog/sentinel`
  is a placeholder; replace it everywhere (`go.mod`, imports, Taskfile
  `MODULE`, `.golangci.yml` local-prefixes) once the repository exists.
