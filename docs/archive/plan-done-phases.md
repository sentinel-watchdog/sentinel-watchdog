> Historical, not maintained: the checklists of the phases done before
> Phase 3c and the repository scope review, moved from
> [PLAN.md](../../PLAN.md) in Phase 3c-6b. The current status is in
> PLAN.md §7.

# Done phases of the plan

### Phase 0: Prototype and design baseline · `done` (2026-10-05 … 2026-10-06)

- [x] Prototype code: configuration, logging, redaction, state store,
  cron parser, event model (`internal/config`, `internal/logging`,
  `internal/redact`, `internal/state`, `internal/scheduler/cronexpr`,
  `pkg/model`); `task check` green; ~93 % coverage on config.
- [x] Design baseline: ADR-0001…0014, threat model, project audit,
  architecture document.
- [x] Naming (D-057), monitor → supervisor rename (D-062), module
  architecture and configuration layout (D-063, ADR-0015), clean restart
  (D-064), roadmap and versioning (D-065).

Notes (carry forward):

- The prototype is **not** the base of the new code (D-064). Phase 2a
  ports the generic parts with review and deletes the rest.
- Review findings to fix while porting: cron `*/N` in day-of-week must be
  treated as `*` for the day-matching rule (Vixie/cronie behaviour);
  the strict-key walker follows YAML aliases without a bound (cap alias
  expansion).
- `docs/configuration.md` and `configs/` describe the prototype until
  Phase 2a rewrites them.

### Phase 1: Repository, CI and security baseline · `done` (2026-10-06)

Goal: a protected repository where every change is built, tested and
scanned before it reaches `main`. No product code beyond what CI needs.

GitHub settings (maintainer, 2026-10-06):

- [x] Repository `sentinel-watchdog/sentinel-watchdog` created, public
- [x] Organisation: 2FA required
- [x] Organisation Actions policy: SHA pinning required, allowed actions
  restricted to GitHub-owned plus `golangci/golangci-lint-action` and
  `ossf/scorecard-action` (verified through the repository's effective
  permissions)
- [x] Merge methods squash + rebase, auto-delete head branches, wiki off
- [x] Ruleset `main` active (PR, linear history, no force push/deletion)
- [x] Ruleset `main`: required status check `ci-ok` (GitHub Actions,
  strict), merge methods squash + rebase
- [x] Ruleset `release-tags`: `v*` tags cannot be moved or deleted
- [x] Fork pull request workflows need approval for all external
  contributors (organisation and repository)
- [x] Organisation security configuration `sentinel-baseline` is the
  default for new public repositories
- [x] Private vulnerability reporting, Dependabot alerts and security
  updates, secret scanning and push protection
- [x] Labels created (`task labels`)

Repository content (branch `phase-1/repo-baseline`):

- [x] `.github/workflows/ci.yml`: gofmt, `go mod tidy -diff`, vet,
  golangci-lint, unit + race tests on native amd64 and arm64 runners,
  linux/amd64 + linux/arm64 builds, govulncheck, actionlint + zizmor,
  aggregate `ci-ok`; `contents: read`; actions pinned by SHA;
  `persist-credentials: false`; concurrency groups (D-052)
- [x] Dependabot for Go modules and GitHub Actions, 7-day cooldown
- [x] CodeQL (Go + Actions) and OpenSSF Scorecard workflows
- [x] `task test-linux` (glibc with race, musl, non-root; sources
  streamed as tar, so local and remote Docker contexts work),
  `task lint-actions`, `task labels`, `task tidy-check`; tool versions
  pinned (D-051)
- [x] `task test-linux` green on a remote Docker host (glibc + race, musl)
- [x] `SECURITY.md`, `CONTRIBUTING.md`, `CODEOWNERS`, PR and issue
  templates (D-059)
- [x] `docs/development.md`: setup, tasks, test levels, CI, supply-chain
  rules, repository settings, Go toolchain patterns (D-060)
- [x] First CI, CodeQL and Scorecard runs green on GitHub (PR #1 and
  `main`); Scorecard 6.8/10, gaps explained in docs/development.md
- Moved to Phase 2c: GoReleaser snapshot build (needs a `main` package).

Notes:

- Workflows validated locally with actionlint v1.7.12 and zizmor v1.30.1
  (auditor persona): no findings.
- Incident (2026-10-06): the first `task test-linux` attempt ran on the
  remote Docker context by mistake while the task still
  used bind mounts; it pulled `golang:1.27` and `alpine:latest` and
  likely created empty directories mirroring the local path on that host.
  The task now streams sources and prints the engine it uses.
- Open for the maintainer: remove the `admin:org` scope from the local gh
  token (`gh auth refresh -h github.com -r admin:org`).
- OpenSSF Best Practices badge: deferred to Phase 3e.
- Conventional commits from here on (the history before this phase has
  two non-conforming commits; it is not rewritten).

### Agent workflow · `done` (2026-10-07, D-069)

- [x] `AGENTS.md` as the single instruction file for any AI agent
  (commands, design and dependency rules, errors/logging/config, tests,
  security, Git, review, definition of done) and design principles
  applied the Go way, with a "Learning Go" reading list in
  `docs/development.md` (D-060); `CLAUDE.md` imports it
- [x] Guidelines for humans and agents: `docs/guidelines/privileged-code.md`,
  `docs/guidelines/github-workflows.md`
- [x] Agent-neutral prompt templates in `docs/agents/` (review, plan and
  plan critique, delegated implementation); working material in
  gitignored `.plans/`
- [x] `docs/development-workflow.md`: the change loop, five ways of
  working (Claude Code and/or Codex, either calling the other), review
  and findings, dual planning, delegation in a worktree
- [x] `task fuzz`; shared `.claude/settings.json` denying credential reads
  and force pushes; PR template sections for privileged code and
  independent review
- Deferred: fuzzing in CI (nightly job) until fuzz targets exist and their
  run time is known; a Claude Code hook running `task check` (slow, CI
  enforces the same checks). Both in the [backlog](../backlog.md)
  (B-002, B-003).

### Phase 2a (Core): libraries, configuration loader, module framework · `done` (2026-10-08, #6)

- [x] Delete the prototype configuration package; port with review into
  `internal/core/`: `redact`, `logging`, `cronexpr` (fixed: a day field
  starting with `*`, such as `*/2`, is unrestricted for the day-matching
  rule, as in Vixie cron/cronie), strict YAML decoding (bounded walk
  against alias expansion), `${VAR}` expansion, `Duration` / `ByteSize` /
  `FileMode`, problem collection
- [x] `internal/core/clock`: `Clock`/`Timer` + `Fake` with `Advance`,
  `PendingTimers`, `BlockUntilTimers` (written by the agent; Q-013)
- [x] `internal/core/config`: central file schema (`version`, `daemon`,
  `notifications`, `modules`), module directories, `ModuleConfig` with
  `Section.Decode` strict helper, rules 1 to 9 of ADR-0015 (closed module
  names, planned/not-built errors, disabled directories not read and
  reported, `settings` once per module directory, ownership and mode
  checks of the pinned configuration directory and its parents (D-070),
  duplicate/merge keys rejected and size limits (D-071)),
  `IncludeDisabled` for `validate --all`, redacted view
- [x] `internal/core/module`: `Module`/`Configured` contract, explicit
  registry (`Register`, `Planned`, `NotBuilt`, `Availability`,
  `Configure`, `Validate`), panic isolation in `Configure`; test-only
  modules. Build tags will call `NotBuilt` from the daemon's wiring (2c)
- [x] Fuzz targets for every parser of untrusted input (configuration
  documents and `Section.Decode`, `${VAR}` expansion, `ParseByteSize`,
  `cronexpr.Parse` and its steps), run with `task fuzz` (D-069)
- [x] Dual audit (Claude Code + Codex, 2026-10-08): five known review
  findings and the audit findings fixed with regression tests; fuzzing
  then found two more bugs, fixed (`.plans/` holds the working notes,
  outside Git)
- [x] Independent review of the whole Phase 2a range, then the
  maintainer's review (privileged code)
- [x] Architecture test (`internal/archtest`): ADR-0015 dependency rules
  over every package's imports, test imports included (`go list`), plus a
  table test of the rules
- [x] Tests: loader (central + directories, order, duplicates, disabled
  modules, unknown/planned/not-built modules, ownership and modes, env
  expansion and injection), strict decoding (inline, skipped fields,
  alias bomb), scalars, registry end-to-end with config
- [x] docs/configuration.md rewritten; `configs/sentinel.yaml` example
  (validated by a test); docs/development.md Phase 2a patterns;
  architecture.md configuration pipeline

Phase 2a notes (carry forward):

- Coverage: clock 100 %, config ≈ 92 %, module ≈ 97 %.
- Independent review by Codex (read-only) of the first draft found nine
  issues, all fixed in the same branch: directory checks did not pin the
  path (now `os.Root` + parent checks, D-070); duplicate keys and YAML
  merge keys could make switches ambiguous or bypass the one-`settings`
  rule (rejected, D-071); `validate --all` skipped unnamed modules; factory
  panics and typed-nil results escaped the registry's recovery; panic
  payloads could leak secrets; a FIFO could block loading; no aggregate
  size bound; the architecture test used denylists (now allowlists, also
  checked for GOOS=linux with the `integration` tag).
- The module contract is deliberately minimal (`Start(ctx)`,
  `Stop(ctx)`): the runtime services a module receives (events, state,
  notifications, audit), its status and its CLI commands are added in 2b
  and 2c, where they have consumers (R-022).
- Module panics (in `Register` and `Configure`) are reported without
  their value. Decided in 2c-1 (D-075): the daemon never records a panic
  value; it logs the panic's frames (function, file, line), not a full
  stack, whose arguments could hold secrets.
- The list of known module names (`supervisor`, `firewall`, `remote` as
  planned) is wired by the daemon in 2c; `config.Load` only knows what it
  is given.
- Old phase numbers remain in `docs/threat-model.md` (R-xxx/T-xxx text);
  the phase mapping in this file applies.

### Phase 2b (Core): events, state, notifications · `done` (2026-10-08, #7)

- [x] `pkg/model.Event` per ADR-0002 as amended by ADR-0015: `module`,
  `source`, `source_type`, `event_type`, `severity`, `correlation_id`,
  bounded `attributes`; per-module event type registration
  (`events.Registry`, core types pre-registered)
- [x] `internal/core/events`: in-process bus, bounded per-subscriber
  queues, overflow accounting; normalisation at the boundary (control
  characters, UTF-8, length limits, attribute shapes, 16 KiB cap). Dedup
  windows wait for a module that needs them (D-072)
- [x] `internal/core/state`: atomic store (ported to `os.Root`), per-module
  state files with their own `schema_version`, quarantine, downgrade
  refusal, ownership checks (`internal/core/fstrust`, extracted from
  config)
- [x] `internal/core/notify`: async bounded dispatcher, webhook provider
  (method, headers, timeout, retry + backoff, accepted status codes, no
  redirects, URL never in errors), routing by route function,
  `repeat_interval` with `suppressed_count` (D-072)
- [x] Configuration: `config.Route` (D-004 short/long form) with
  `Problems` for modules, `notifications.core`, `repeat_interval`,
  `ModuleConfig.Channels`
- [x] Tests: event validation and limits (+ fuzz), bus overflow and
  concurrency, state corruption and quarantine, webhook retry/timeout/TLS/
  redirects (httptest), dispatcher on a fake clock
- [x] docs/notifications.md (payload contract); prototype `internal/state`
  and old `pkg/model` types removed

Phase 2b notes (carry forward):

- The daemon (2c) wires the runtime a module receives at `Start` (events,
  state, notify), builds the route function from `notifications.core`
  and the modules' routes, and reports bus drops and channel counters
  (`Subscription.Dropped`, `Dispatcher.Stats`) in status (D-072).
- Phase 3: the supervisor registers its event types, validates item
  routes with `config.Route.Problems`, and defines its state type.
- The first module that changes its state schema adds a migration;
  until then an older schema is an error (`state.ErrOlderSchema`).
- Independent Claude Code review after merge (2026-10-09): nine findings
  missed by the four Codex reviews, all fixed with regression tests
  (D-074). The daemon also reports `Stats.SuppressedLost`; `Dispatcher.Run`
  is single-use.

### Phase 2c (Core): daemon, control socket, authorization, audit, CLI · `done` (2026-10-09 … 2026-10-15)

Goal: `sentineld` and `sentinelctl` run with zero modules. Delivered in
five PRs (maintainer, 2026-10-09; 2c-3 split in two on 2026-10-12, D-077);
each is green and reviewed on its own.

**2c-1: Daemon tracer bullet** (D-075)

- [x] `internal/daemon`: production registry (planned `supervisor`,
  `firewall`, `remote`), `Load`/`Validate` shared by both binaries,
  `Daemon.Start/Stop/Snapshot`: state directory, bus, dispatcher, event
  logger, module lifecycle with recovered panics (frames only), time
  limits, one shutdown budget, cleanup `Stop` after a failed `Start`,
  `daemon_error` for every module failure, routing by
  `notifications.core` and `module.Router`
- [x] `module.Runtime` (logger, clock, `events.Emitter`, state store)
  given to `Start`; `config.WriteDiagnostics`
- [x] `cmd/sentineld` (`-config`, `-validate`, `-version`, SIGTERM/SIGINT,
  umask 027); `cmd/sentinelctl` (`version`, `validate [-all]`; socket
  commands fail loudly)
- Deferred to the backlog (B-004, medium): logging that cannot block the
  daemon when its destination stops reading (found in Codex's review)
- [x] Tests: lifecycle under `testing/synctest` (no goroutine left),
  failures and panics isolated without payload, hung `Stop` abandoned
  within the budget, routing, `validate -all` with a test module, real
  signals and inherited umask through a re-executed test binary

**2c-2: Protocol, socket, authorization, read commands** (D-076)

- [x] `pkg/api` v1: versioned newline-delimited JSON, structured errors,
  commands `<namespace>.<command>` with declared tier; strict decoder
  (`encoding/json/v2`) with a fuzz target
- [x] `internal/core/transport`: Unix socket server and client (mode and
  group from config, lock file, stale socket handling, deadlines, request
  and response size limits, connection limit, bounded close)
- [x] `internal/core/authz` and `internal/platform/peercred`:
  `SO_PEERCRED` + `SO_PEERGROUPS` tiers `read` / `operate` / `admin`
  (ADR-0012), `daemon.access`; outside Linux requests answer `unavailable`
- [x] `sentinelctl` `status` (with `Daemon.Snapshot`), `modules`,
  `config show`, `events` (answers `unsupported` until Phase 3a); CLI
  tests against an in-process daemon; tiers tested in transport and authz

**2c-3a: Audit log** (D-077)

- [x] `internal/core/audit`: hash-chained `audit.jsonl` (hash over the
  exact bytes), fsync per record, `audit_head.json` with last record and
  anchor, rotation 5 × 10 MiB, start-up check of every kept file; a broken
  chain or a failed write refuses new records (never repaired)
- [x] Control socket: every `operate`/`admin` request audited before it
  runs (`intent` → `ok`/`failed`, `denied`), fail closed; peer pid in
  `authz.Peer`; per-user connection limit and a slot kept for root
- [x] `core.audit` / `sentinelctl audit [-n N]` (tier read), audit
  integrity in `core.status`; events `audit_integrity_broken`,
  `audit_write_failed`; journal copy of every record
- [x] Tests: golden line, chain across restarts and rotations, tampering
  (edit, delete, reorder, truncate, torn line, oversized line, removed
  files, replaced chain, head missing/corrupt, symlink, FIFO, modes),
  crash windows (head one behind, each rotation step), write and head
  failures, gate and admission in transport, daemon and CLI wiring; fuzz
  of the line parser and of the start-up check

**2c-3b: Reload** (D-075, D-077, D-078)

- [x] Same-owner re-registration of event types (a restarted module)
- [x] Reload by SIGHUP and `core.reload` / `sentinelctl reload` (tier
  operate): whole configuration validated, changed modules restarted,
  live and restart-required `daemon.*` keys, access policy for new
  connections, dispatcher replaced (old queue abandoned and counted),
  configuration digest audited; lifecycle gate with Start/Stop
- [x] Tests: valid/invalid reload, granularity, abandoned modules,
  notifications, concurrency under synctest, SIGHUP

**2c-4: Release snapshot and operations docs** (D-079)

- [x] GoReleaser configuration and snapshot build in CI (no publishing):
  tarballs, checksums, SBOM (moved from Phase 1), reproducible,
  verified by `task snapshot-verify`, one SBOM per binary (cyclonedx-gomod)
- [x] Optional `sd_notify` (READY/STOPPING; D-075); no reload messages
  (B-007), `NOTIFY_SOCKET` removed from the environment
- [x] docs/operations.md, docs/security.md (first versions), docs/README.md
- [x] Build date in UTC (the Taskfile printed local time with a `Z`)

### Phase 3a (Supervisor): module skeleton, HTTP services · `done` (2026-10-15 … 2026-10-19, v0.1.0)

Design: D-080 (dual plan by Claude Code and Codex). Delivered in five
pull requests, each green on its own.

- [x] 3a-1 Core preparation: CA/TLS knowledge in `internal/core/tlsconfig`
  (webhook unchanged); `Section.Decode` reports unknown keys and bad
  values together; module warnings (`module.Warner`) reach
  `Config.Warnings`, module problems and warnings redacted; `settings`
  in two files located by file and line
- [x] 3a-1b Design review fixes (2026-10-16), before the module code
  builds on the core: a whole-repository review (two Claude Code reviewers
  and Codex, then Codex's critique of the merged findings) found the core
  sound and these contract defects, each fixed with a failing-first test:
  one audit recorder for the socket and reloads (`audit_write_failed`
  missed or doubled), the reload budget on the daemon's clock, pointer
  state types validated and JSON `null` refused, conflicting event
  declarations in one call refused, `core` reserved as a module name,
  transport wiring checked at `Listen`, socket cleanup errors reported,
  every problem of a route reported, module errors not rewritten by
  redaction; fuzz targets for the state file, responses and the audit head.
  Structural findings (daemon file layout, helpers, test sleeps, lint
  rules, shared readers, DRY of names) are in the backlog (B-011 … B-018)
  and are picked up when nearby work benefits
- [x] 3a-2 Module commands on the control socket: `Commander` (static,
  on `Module`), `Handler` (on the running instance), lifecycle-safe
  dispatch, tests with a fake module
- [x] 3a-3 `internal/modules/supervisor` (D-081, two PRs: configuration
  helpers `Section.Child`/`Items`/`KeyLine` and scalar problems that let
  decoding go on without quoting values; then the module): schema (`settings`, `services`,
  `jobs`), `Checker`, failure policy, state, events
  (`service_failed`, `service_recovered`), routes, commands; still
  registered as planned; fuzz target. `Watcher` moves to 3b (process
  services)
- [x] 3a-4 (D-082, two PRs: the HTTP checker and its keys, including
  `address`; then registration, CLI and docs) HTTP services:
  method, headers, body, timeout, status codes,
  `body_contains` with a size limit, TLS (CA file, server name, minimum
  version), same-origin redirect policy (D-019, D-080), fixed failure
  codes; supervisor registered; CLI `supervisor status [name]`,
  `supervisor list`; docs/supervisor.md; `configs/supervisor/` examples
- [x] 3a-5 `core.events`: recent events (D-083)
- [x] Tag v0.1.0 (preview, binaries only; 2026-10-19, at the maintainer's request). No release
  artifacts are published: the signed release pipeline is Phase 3d, and
  `task snapshot` builds the same archives locally

### Phase 3b (Supervisor): executor, processes, recovery · `done` (2026-10-20 … 2026-10-27, v0.2.0)

Design: D-084 (dual plan by Claude Code and Codex). Delivered in six pull
requests, each green on its own.

- [x] 3b-1 `internal/platform/executor`: direct exec / explicit shell
  (D-005), environment, working directory, uid/gid, process group,
  `Pdeathsig`, stop signal → timeout → SIGKILL to the group, exit code and
  signal, output sinks (D-009); `internal/platform/privilege`: user/group
  resolution (names and numeric, R-004), `CapEff` checks, clear
  `permission_denied`; `task test-vm HOST=…` with the first integration
  tests (D-056)
- [x] 3b-2 Process services: schema, central switch, startup grace, crash
  detection, orphan cleanup; no automatic restart yet (D-086: trusted
  programs, state schema 2 and `state.LoadMigrating` moved here from
  3b-3, `module.Runtime.Secrets`)
- [x] 3b-3 Recovery engine: attempts in window, delay, fixed/exponential
  backoff, `stable_after`, cooldown (D-013), exhausted, persisted attempts
  (D-014); `restart` action; optional fields in state schema 2 (D-086);
  audit of automatic actions (D-087: `module.Runtime.Audit` and `Fault`,
  fail-closed launches, `service_restarted`, `service_recovery_exhausted`)
- [x] 3b-4 Service-level reload (`Reconfigurer`, D-088: in place,
  `module.ErrRestart`, pending retry of a partial reload, blocked names
  until restart; amends D-078 (3) for the time zone)
- [x] 3b-5 CLI: `supervisor start|stop|restart|enable|disable|reset <name>`
  (tier `operate`, audited with the target; D-089: `stop` and `disable`
  persisted, per-service `enabled`, `cooldown: 0` with `reset`)
- [x] 3b-6 Resource limits: `limits.memory` sampled from `/proc` (D-021,
  D-084); docs, examples, VM matrix (D-085: six distributions) (D-090:
  leader RSS every 5 s through a pinned `/proc/<pid>`, stop with
  `memory_limit` then recovery, audited, fail open on unreadable samples)
- [x] Tag v0.2.0 (preview; 2026-10-27, at the maintainer's request). No
  release artifacts are published: the signed release pipeline is
  Phase 3d, and `task snapshot` builds the same archives locally

### Repository scope review · completed (2026-10-07)

- [x] Agent ownership and cross-project boundaries documented (D-068).
- [x] Global roadmap, commercial assessment and discovery process prepared
  in the workspace parent; agent summaries stay self-contained.
- [x] Phase 5 limited to the Remote module and contract/integration work.
- Runtime phases are unchanged; Phase 2a was next (done 2026-10-07).
