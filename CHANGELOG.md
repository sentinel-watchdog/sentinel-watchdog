# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project
uses [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Changed

- Agent tooling is personal (D-091): `.gitignore` excludes `.agents/`,
  `.claude/skills/`, `.claude/commands/` and `.claude/agents/`, and
  `docs/development-workflow.md` states the rules any tool follows
  instead of `codex exec` / `claude -p` recipes.

## [0.2.0] - 2026-10-27

Preview: the Supervisor's process services (Phase 3b): executor,
recovery, service-level reload, operator commands and a memory limit.
Tag only: no packages or published binaries until Phase 3d.

### Changed

- Process services are restarted by default (3b-3): a service without
  `recovery` gets the D-084 defaults instead of staying down after its
  first exit. The cleanup of a group a previous run left is retried every
  `max_delay` instead of keeping the service down until sentineld restarts.

- `redact.Text` finds every secret in the original text and masks the
  union of what they cover: a secret that overlaps another is no longer
  partly left after the other is replaced.
- `fstrust.Resolve` no longer cleans its input lexically: `link/..` goes
  back from where the link leads, as the kernel resolves it.

- D-085: the VM test matrix of `task test-vm` grows to six distributions
  (Debian 13, Ubuntu 24.04 and 26.04, Rocky 9 and 10, openSUSE Leap 15),
  defined in `tests/vm/Vagrantfile` so every contributor can run it;
  supported targets unchanged.

- `task lint` and `task fmt` run golangci-lint at the version CI pins
  (`go run …@GOLANGCI_LINT_VERSION`) instead of the copy installed on the
  machine, so local lint finds what CI finds.
- golangci-lint 2.15.0 (was 2.14.0) in the Taskfile, CI and the dev
  container (B-019); two daemon tests now check the result of the audit
  record they wait for.

### Added

- Phase 3b-6 (D-090): **memory limit** of process services.
  `limits.memory` (1 MiB to 1024 TiB) is enforced: the leader's resident
  memory is sampled from `/proc` every 5 s, through a handle bound to that
  process so a reused pid is never sampled; above the limit the whole
  group is stopped with `stop_signal` and `stop_timeout`, the failure is
  `memory_limit` (`service_failed` carries `rss_bytes` and `limit_bytes`)
  and recovery restarts it within its budget. The stop is audited as
  `supervisor.stop` (`trigger: memory_limit`), intent first but not held
  back by a broken audit log, and keeps its `stop_timeout` across a
  reload, an operator's command or shutdown. A sample that cannot be read
  never reads as zero: the process keeps running and status shows memory
  `UNKNOWN`. Any other limit key is refused, as is `limits` on an http
  service; where processes cannot be sampled (outside Linux) a service
  with a limit is not started (`unsupported`). `supervisor status` shows
  the last sample against the limit. `executor.LeaderRSS` and
  `executor.ErrGone` are new.
- Phase 3b-5 (D-089): **operator commands**. `sentinelctl supervisor
  start|stop|restart|enable|disable|reset <name>` (tier `operate`):
  `stop` holds a process service stopped, `disable` takes any service out
  of supervision; both are persisted and survive reloads and restarts.
  `enabled: false` on a service (new key, both types) wins over them.
  A launch with an open incident goes through the recovery budget, and
  `restart` stops nothing when it is spent; `reset` renews it, and
  `cooldown: 0` now means exhausted until `reset`. A command answers
  within 2 s; a longer stop goes on under its own `stop_timeout`. Audit
  records of commands that act on a named object carry the validated
  name as `target` (`api.Command.TargetsName`), and `sentinelctl audit`
  shows it; the request's correlation id links the module's own records.
  New protocol code `conflict`; `supervisor status` shows who disabled or
  stopped a service.
- Phase 3b-4 (D-088): **service-level reload**. A module may implement
  `module.Reconfigurer` and take a new configuration in place; the
  supervisor does: a reload stops and starts only the services whose
  definition changed, and unchanged processes keep their pid, incident
  and budget. A route change restarts nothing; a change of
  `daemon.timezone` no longer restarts the supervisor. `sentinelctl
  reload` and the audit record list `reconfigured` modules. A service
  that does not stop for a reload keeps running and its new definition
  waits (`blocked`, `reload_stop_failed`) until sentineld restarts; a
  reload applied in part is retried by the next one.
- Phase 3b-3 (D-087): **recovery of process services**. A process that
  ends without being asked to, or cannot be started, is started again at
  most `recovery.max_attempts` times in `window` (default 5 in 10 min),
  after `delay` growing to `max_delay` (`backoff: exponential` or
  `fixed`), then `exhausted` until `cooldown` (default 10 min) renews the
  budget. Attempts are saved before each launch and survive restarts and
  reloads; `stable_after` closes the incident. Every automatic launch is
  recorded in the audit log before and after (`supervisor.start`,
  `supervisor.restart`, `supervisor.cleanup`, actor `daemon`); a launch
  that cannot be recorded or saved is `blocked`, never run unaudited. A
  corrupt state file starts the processes with their budgets spent and
  publishes `daemon_error`. New events `service_restarted` (warning) and
  `service_recovery_exhausted` (critical); `supervisor status` shows
  restarts, attempts, the next attempt, the cooldown and why a launch is
  blocked. `module.Runtime` gains `Audit` and `Fault`.

- Phase 3b-2 (D-086): **process services** in the supervisor. A service
  with `command` (or `script` and `shell`) is started by sentineld once
  `modules.supervisor.processes: true` is set in the central file, with
  its `user`/`group`, `working_dir`, `environment` (nothing of
  sentineld's own, plus a default `PATH`), `output` (`log`, `discard` or
  a file), `stop_signal`, `stop_timeout` and `startup_grace`. A crash is
  one `service_failed`; there is no automatic restart yet (3b-3). The
  program and every directory and link on its way must be trusted.
  Process groups are recorded in the state file, and what a killed
  sentineld left in them is killed at the next start. `sentinelctl
  supervisor status` shows the phase, pid, uptime, starts and last exit.
- `state.LoadMigrating` migrates a module's state file from older schema
  versions; the supervisor's file moves to schema 2 (v0.1.0 refuses it).
- `module.Runtime.Secrets` (from `Config.Secrets`) gives a module the
  secrets of the configuration it was configured from, to mask text from
  outside the module, such as a child's output, in one pass with its own.
- Phase 3b-1 (D-084): `internal/platform/executor` runs a program with an
  explicit environment, working directory and identity, in its own
  process group with a parent-death signal; stops the group with a signal
  and then SIGKILL; kills what is left of a group whose leader exits;
  bounds output (line length, rate) and opens output files like trusted
  files. `internal/platform/privilege` resolves users and groups (numeric
  ids never looked up) and reads capabilities. `task test-vm HOST=…` runs
  the root integration tests (`tests/integration/`) on a VM.
- D-084 records the plan of Phase 3b; the argv-allowlist runner moves to
  Phase 3c.

## [0.1.0] - 2026-10-19

Preview: the core (Phases 2a-2c) and the Supervisor's HTTP checks
(Phase 3a). Tag only: no packages or published binaries until Phase 3d.

### Added

- Phase 3a-5 (D-083): `core.events` and `sentinelctl events [-n N]`
  show the last events (up to 256, newest first), kept in memory by the
  log subscriber across reloads, with the count of events lost to a full
  queue. Tier read: events carry no secrets, which a test checks for the
  supervisor's URL, headers and body.
- Phase 3a-4, part 2: sentineld registers the supervisor with its HTTP
  checker (`modules.supervisor.enabled: true` now works).
  - `sentinelctl supervisor list` and `sentinelctl supervisor status
    [name]` (tier read), as tables or `-json`; flags may follow the name.
  - docs/supervisor.md (keys, states, events, failure codes, state file,
    commands, troubleshooting); the configuration reference links to it.
  - `configs/supervisor/http.yaml`, shipped as
    `examples/supervisor/http.yaml` in the tarball.
- Phase 3a-4, part 1 (D-082): the supervisor's HTTP checker, still
  registered as planned until the CLI and docs of part 2.
  - Keys: `method`, `headers`, `body`, `success_status_codes`,
    `body_contains`, `follow_redirects`, `address` (connect to an IP
    while Host, SNI and the certificate follow the URL) and `tls`
    (`ca_file`, `server_name`, `min_version`, `insecure_skip_verify`).
  - Requests ignore the proxy environment, never reuse connections,
    follow redirects only within the origin, read at most 1 MiB of a body
    and report fixed failure codes, never error text. A cancelled check
    also ends a connection still in its TLS handshake.
  - The state digest covers the whole check: changing what is checked
    starts the service again from `unknown`.
  - `tlsconfig.LoadCA` failures match `ErrUnsafe`, `ErrInvalid` or
    `fs.ErrNotExist`; CA problems of the supervisor name no path.
- Phase 3a-3 (D-081): the supervisor module's core,
  `internal/modules/supervisor`, still registered as planned until its
  HTTP checker (3a-4).
  - Schema: `settings` and `services`, with safe defaults and every
    problem reported with file and line, no value quoted.
  - Checks: one loop per service, at most `max_concurrent_checks` at
    once; a check that ignores its timeout counts as `timeout` and keeps
    `Stop` waiting, never left behind.
  - Policy and events: N consecutive failures emit `service_failed` once,
    M successes emit `service_recovered` with the incident's id, routed
    by each service's `notifications`.
  - State: survives restarts, reconciled by definition digest; a failed
    save does not stop supervision.
  - Commands: `supervisor.list` and `supervisor.status` (tier read).
- Notification route problems name the entry by its position instead of
  quoting it (`channel 2 of the route is not a configured notification
  channel`): a channel name or event filter may come from the environment.
- Phase 3a-2 (D-080): modules can add commands to the control socket. A
  module declares them statically (`module.Commander`: name in its own
  namespace and tier, checked at registration) and its running instance
  handles them (`module.Handler`, required by `Configure` when commands
  are declared). A command of a module that is not running answers
  `unavailable`; stopping a module closes its commands and waits for
  those in flight; a panicking command is recovered, reported as
  `daemon_error` and answered `internal`.
- Phase 3a-1 (D-080): a module can report warnings for a configuration
  that is valid but risky (`module.Warner`); `validate` prints them and
  sentineld logs them with the loader's. A module's problems and warnings
  are redacted with the configuration's secrets before they are printed.
- Phase 2c-4 (D-079): `task snapshot` and a CI job build release-shaped
  artifacts without publishing them — tarballs for linux amd64 and
  arm64, `checksums.txt` and a CycloneDX SBOM per binary — reproducible
  from the commit and checked by `task snapshot-verify`. Under systemd
  (`Type=notify`) sentineld sends `READY=1` once started (status
  `degraded` when a module is not running, nothing when a signal cut the
  start short) and `STOPPING=1` at shutdown; it removes `NOTIFY_SOCKET`
  from its environment and ignores an invalid value. First
  `docs/operations.md` (install, systemd example, signals, reload, tiers,
  audit, troubleshooting), `docs/security.md` and a `docs/README.md`
  index.
- Phase 2c-3b reload (D-078): `sentinelctl reload` (tier `operate`) and
  SIGHUP reread the whole configuration; on any problem nothing changes
  and every problem is reported. Otherwise only the modules whose
  configuration changed restart, `log.level`, `access.*` and routes apply
  at once, a change of channels replaces the notification dispatcher
  (queued deliveries abandoned and counted), and keys bound at start-up
  (`socket`, `socket_mode`, `socket_group`, `state_dir`, `log.format`)
  keep their value with a warning. Each reload is audited with the old
  and new configuration digest (SHA-256 over the files read), announced
  by `config_reloaded` or `config_reload_failed`, and bounded by twice
  `daemon.shutdown_timeout`; `sentinelctl status` shows the digest and
  the last reload. A module may register its event types again when it
  restarts.
- Phase 2c-3a audit log (D-077): `internal/core/audit` appends one
  hash-chained JSON record per line to `<state_dir>/audit.jsonl` (synced,
  rotated 5 × 10 MiB) and checks the whole chain against
  `audit_head.json` at start-up; a broken chain is reported in status and
  by `audit_integrity_broken` and never repaired, and operate and admin
  requests are then refused. The control socket records every operate or
  admin request before it runs (fail closed), denials included, with the
  peer's uid, gid and pid, and limits connections per user (root keeps
  the last slot). `sentinelctl audit [-n N]` shows the last records; each
  record is also logged for the journal.

- Shared editor settings and a dev container: `.vscode/settings.json` and
  `extensions.json`, `.zed/settings.json`, `.devcontainer/` (Go 1.27,
  golangci-lint and task at the CI versions, normal user) usable on any
  Docker context, and `task dev-shell` for a Linux shell with a copy of
  the sources on a local or remote engine.
- Phase 2c-2 control socket (D-076): protocol v1 in `pkg/api` (one JSON
  line each way, strict and fuzzed decoder, tiers declared per command),
  `internal/core/transport` (socket file with a lock, safe stale-socket
  removal, bounded requests, responses, time and connections),
  `internal/core/authz` (tiers from kernel peer credentials,
  `SO_PEERCRED` + `SO_PEERGROUPS`), `sentinelctl status`, `modules`,
  `events` (from Phase 3a) and `config show` with `-socket` and `-json`.
- Phase 2c-1 daemon tracer bullet (D-075): `sentineld` (`-config`,
  `-validate`, `-version`) starts the core (state directory, event bus,
  notification dispatcher, event log) and the enabled modules, isolates
  module failures and panics (`daemon_error`, never the panic value),
  stops within `daemon.shutdown_timeout` on SIGTERM/SIGINT and sets umask
  027; `sentinelctl version` and `sentinelctl validate [-all]`. Modules
  receive a `module.Runtime` (logger, clock, event emitter, state store)
  and may route their events with `module.Router`.
- Agent workflow (D-069): `AGENTS.md` as the single instruction file for
  any AI agent (`CLAUDE.md` imports it) with design principles applied
  the Go way (Go idioms, Clean Architecture through ADR-0015, SOLID,
  Clean Code, Pragmatic Programmer, KISS/YAGNI), guidelines for privileged code
  and GitHub workflows in `docs/guidelines/`, agent-neutral review, plan
  and implementation templates in `docs/agents/`,
  `docs/development-workflow.md` covering Claude Code and Codex alone,
  together or calling each other, `task fuzz`, shared
  `.claude/settings.json` denying credential reads and force pushes, PR
  template sections for privileged code and independent review.
- Phase 2b core: event model per ADR-0002 (`module`, `source`,
  `source_type`, `severity`, `correlation_id`, bounded attributes) with
  per-module event type registration and an in-process bus with bounded
  queues; per-module state files with quarantine and downgrade refusal
  under the checked state directory; notification dispatcher and webhook
  delivery (retries with backoff, no redirects, TLS, `repeat_interval`)
  with a versioned payload documented in `docs/notifications.md`; routes
  (`notifications.core`, D-072).
- Phase 2a core: `internal/core/config` loads the central `sentinel.yaml`
  (daemon, notifications, module switches) and one directory per enabled
  module, with ownership/mode checks on the pinned configuration
  directory and its parents (`os.Root`, D-070), strict decoding with line
  numbers, duplicate and merge keys rejected and size limits (D-071),
  `${VAR}` expansion and all problems reported at once; `internal/core/module` adds the module
  contract and registry; `internal/core/clock` an injectable clock with a
  fake; `internal/archtest` enforces the ADR-0015 dependency rules;
  `configs/sentinel.yaml` example. Hardened after a dual audit (Claude
  Code and Codex): environment values never appear in configuration
  problems or sensitive log groups, symbolic links may only name an entry
  of their own directory, every byte and directory entry is bounded,
  module factories are isolated at registration, cron steps cannot
  overflow; fuzz targets for every parser (`task fuzz`).
- Future-module discovery process (D-067): select the operator problem,
  service and scope before design/delivery; Monitor remains a candidate,
  with OpenObserve, Prometheus and Grafana integrations tracked in Q-018.
- Agent implications of contextual vulnerability triage research (2026-10-07);
  Q-017 and R-023 track evidence and correctness risks. Full commercial
  assessment and experiment belong to workspace coordination (D-066/D-068).
- Repository and CI security baseline (Phase 1): CI workflow (lint, tests
  on amd64 and arm64, Linux builds, govulncheck, workflow linting with
  actionlint and zizmor), CodeQL and OpenSSF Scorecard workflows,
  Dependabot with cooldown, `SECURITY.md`, `CONTRIBUTING.md`, `CODEOWNERS`,
  pull request and issue templates, `docs/development.md`, Taskfile tasks
  `test-linux` (local or remote Docker, sources streamed as tar),
  `lint-actions`, `labels`, `tidy-check`.
- Module architecture and configuration layout
  ([ADR-0015](docs/adr/0015-modules-and-configuration-layout.md)): core,
  platform adapters and modules (supervisor, firewall, later remote and
  monitor); central `sentinel.yaml` with module switches and safety gates
  plus one configuration directory per module.
- Decision log moved to `docs/decisions.md` (D-063 module architecture,
  D-064 clean restart, D-065 roadmap and versioning).
- Three-project layout documented: core, design and website, with technical
  documentation owned by core and visual asset masters owned by design.
- Sentinel Watchdog logo: transparent PNG emblem for repository and tool
  avatars, with usage notes and generation prompt in `docs/branding.md`.
- Foundation verification refresh in `docs/project-audit.md` (2026-10-06).
- Phase 1 foundation (now the Phase 0 prototype, replaced in Phase 2 per D-064):
  - YAML configuration model (`version: 1`) with main file and `conf.d/`
    fragments, deterministic load order, strict unknown-key detection with
    line numbers, `${VARIABLE}` expansion, defaults and rigorous validation.
  - Cron expression parser used for configuration validation.
  - Structured logging with text, JSON and journald formats and secret
    redaction.
  - Public event, supervisor state and capability status model.
  - JSON state file with atomic writes, schema versioning, corruption
    quarantine and retention.
  - Taskfile, golangci-lint configuration, documentation and development
    plan.
- Phase 2 design baseline for the network and security features (documentation
  only, no runtime behaviour):
  - Project audit (`docs/project-audit.md`) with the collisions between
    supervision and the new domains.
  - Architecture Decision Records 0001–0014 (`docs/adr/`): modular
    monolith, generalised event model, observation vs enforcement modes,
    provider/capability model, backend-agnostic firewall service,
    nftables and iptables backends, blocklists and CrowdSec, container
    runtimes, Kubernetes, state/audit/transactions, control-plane
    authorization tiers, dependency policy, WAF providers.
  - Threat model (`docs/threat-model.md`).
  - Target architecture in `docs/architecture.md`; decisions, risks and
    open questions in `PLAN.md`; README feature table distinguishes
    implemented, scheduled and design-only features.
  - Roadmap as one numbered sequence (Phases 1–22) with a walking
    skeleton at Phase 5 and releases v0.1.0 (supervision) through v1.0.0
    (Kubernetes, WAF and iptables included).

### Fixed

- Design review of the whole repository (2026-10-16), findings fixed with
  regression tests:
  - an `audit_write_failed` event was missed when the intent of a SIGHUP
    reload could not be written, and published twice when a reload's
    outcome and the next socket request both found the log broken: one
    recorder now serves the socket and reloads;
  - the reload budget is measured on the daemon's clock, like the module
    calls it bounds (a clock far from wall time gave them no time at all);
  - `state.Load` with a pointer state type skipped its `Validate` method;
    JSON `null` for it is now a corrupt file, never a nil state; an error
    opening a state file names the file;
  - two declarations of one event type with different severities in one
    `Register` call were accepted;
  - the module name `core` is reserved;
  - the transport refuses a binding without a function and options without
    a peer-credentials reader at `Listen`, instead of panicking later;
    `Shutdown` reports a socket file it cannot inspect;
  - a notification route reports every unknown key and bad value, and no
    longer hides the problems of the keys after it; a YAML type error no
    longer hides other joined problems;
  - redacting a module's problems no longer rewrites the error the module
    returned.
- Unit tests failed under umask 002, the default on Ubuntu and other
  Linux desktops: `t.TempDir()` directories became group-writable and the
  configuration and state checks refused them. The packages that write
  under `t.TempDir()` now run their tests with umask 022.
- `FuzzParse` (sd_notify) failed on values that are part of the error's
  own fixed text, such as `" sock"`; it now checks that every error is
  one of the fixed texts, a stronger check (input kept as a regression).
- An unknown key in a module or central block hid the bad values next to
  it: `validate` now reports both at once. Two files of a module
  directory with `settings` are reported at the extra file and line,
  naming the first.
- The build date that `task build` links (`sentineld -version`) was the
  local time of the commit with a `Z` suffix; it is now UTC.
- Event text with a long run of stray UTF-8 continuation bytes was cut
  to nothing, valid prefix included, so an event whose `source` came from
  outside could be rejected; the cut now looks back at most three bytes
  (found by `FuzzNormalize`, backlog B-006).
- `TestRetriesWithBackoff` read the channel counters before the
  dispatcher had updated them and failed now and then under load (B-005).
- Phase 2b review (D-074): `repeat_interval` no longer stops working after
  4096 events with pending suppressed counts (the oldest expired one is
  forgotten and counted as lost); `state.Save` never replaces a state file
  with another schema version; `state.OpenDir` checks the path before it
  creates missing directories and rejects relative paths; event
  normalisation work is bounded by the event cap; Unicode format and
  separator characters are removed from event text; the smallest event
  cap holds every unshortened field (12 KiB); `Bus.Publish` returns only
  the event ID; `Dispatcher.Run` returns an error when called twice
  instead of panicking.
- Cron: a day-of-month or day-of-week field starting with `*` (such as
  `*/2`) no longer takes part in the day-matching OR rule, as in Vixie
  cron and cronie.

### Changed

- Configuration (Phase 3a-3 preparation): a bad duration, size or file
  mode no longer stops the decode of its section, so every bad value is
  reported at once, and the problem no longer quotes the value (a secret
  shorter than four bytes was not masked). Modules can decode and locate
  nested blocks and list items with `Section.Child`, `Section.Items` and
  `Section.KeyLine`.
- Fuzz targets for the state file (`FuzzLoad`, including the refusal to
  replace a file of another schema version), control-socket responses
  (`FuzzReadResponse`) and the audit head (`FuzzOpenHead`). Removed unused
  `module.Registry.Names` and `state.Store`'s module field.

- The CA bundle of a webhook is read by the new `internal/core/tlsconfig`
  package, which HTTP checks will share (Phase 3a); behaviour unchanged.
- The unscheduled backlog moved from PLAN.md to `docs/backlog.md`, which
  also lists deferred review findings and follow-ups with a severity
  (B-001 … B-003).
- `redact`, `logging` and `cronexpr` moved under `internal/core/`; the
  prototype configuration package and its examples were removed (D-064).
- Clarified agent-only repository ownership (D-068): PLAN and Phase 5 cover
  agent delivery and the Remote contract; dashboard implementation has its
  own project plan. Product roadmap, commercial assessment and shared
  discovery moved to workspace coordination, with self-contained agent
  summaries retained locally. Documented dedicated branch/PR workflow.
- Recorded the product repositories (D-073): design, website and dashboard
  repositories, private coordination repository, and a future public
  agent–backend contract repository; project boundaries updated.
- Roadmap restructured into phases 1–6: repository and CI security
  baseline, core, supervisor (v1.0.0), firewall (v1.x), remote, monitor.
  New modules and capabilities ship as minor releases.
- Project renamed to **Sentinel Watchdog**; Go module path is now
  `github.com/sentinel-watchdog/sentinel-watchdog`. Binaries stay
  `sentineld` and `sentinelctl`.
- "Monitor" renamed to "supervisor" across configuration (`supervisors:`),
  types, event types (`supervisor_failed`, `supervisor_recovered`), state
  keys and docs; "monitoring" is reserved for a future host-integrity
  domain (D-062). The observe/change split is now called "observation vs
  enforcement".
