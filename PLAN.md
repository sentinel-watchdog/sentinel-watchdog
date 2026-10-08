# Sentinel Watchdog agent — Development Plan

This file is the single source of truth for multi-session **agent** development.
It covers `sentineld`, the local `sentinelctl`, modules and agent releases.
Product coordination lives temporarily in the workspace parent (`ROADMAP.md`,
`DECISIONS.md`, `docs/`); dashboard, website and design own their delivery plans.
This checkout has no dependency on those local files. Boundaries: [project
layout](docs/project-layout.md), D-068.
Every session MUST:

1. read this file first (then `CLAUDE.md` / `AGENTS.md` for conventions);
2. pick the first phase in §7 whose status is not `done`;
3. keep the **Status** checkboxes, the [decision log](docs/decisions.md),
   the **Risks** and the **Open questions** up to date before ending the
   session;
4. finish every phase with: `task check`, `GOOS=linux GOARCH=arm64 go build ./...`,
   documentation and example YAML updates, a short entry in `CHANGELOG.md`
   (Unreleased), and phase notes listing risks and what is still not
   implemented.

Never mark a feature as done when only a stub exists. Planned-but-missing
features must fail loudly (configuration error or `unsupported` status).

Team: two developers (the maintainer and an AI agent). Sub-phases are
sized for 1–3 working sessions each and end with green CI.

Project goals (D-060): one tool for Linux hosts, container hosts and
Kubernetes nodes; **simple to configure** wherever safety allows (D-058);
**secure by construction**, because Sentinel runs as root and can change
the firewall; a showcase-quality open-source project; and a way for the
maintainer to learn Go — every phase explains the Go patterns it
introduces in `docs/development.md`.

Detailed design: [docs/adr/](docs/adr/README.md) (start with
[ADR-0015](docs/adr/0015-modules-and-configuration-layout.md));
decisions: [docs/decisions.md](docs/decisions.md); threats:
[docs/threat-model.md](docs/threat-model.md).

---

## 1. Agent

Sentinel Watchdog is a Linux daemon (`sentineld`) with a control CLI
(`sentinelctl`) over a Unix socket. It is built from a shared **core**
and **modules** that the configuration enables (D-063, ADR-0015).

| Part | What it does | Phase | Release |
|---|---|---|---|
| **Core** | Configuration, events, state, notifications (webhook), audit log, control socket with authorization tiers, CLI; shared platform adapters (process execution, systemd, log reading, Docker/Podman, Kubernetes) | 2 | — |
| **Supervisor** module | Services (systemd units, supervised processes, HTTP endpoints) and jobs (cron): checks, bounded recovery (restart with backoff and loop protection), events and notifications | 3 | **v1.0.0** |
| **Firewall** module | Host firewall in a dedicated nftables table (iptables fallback) with plan / apply / confirm / rollback; dynamic blocklists; Sentinel as a CrowdSec remediation component; WAF/reverse-proxy adapters; works on container hosts and Kubernetes nodes without breaking what Docker, the CNI or kube-proxy manage; NetworkPolicy planning | 4 | v1.1 … v1.6 |
| **Remote** module | Connection to a dashboard (separate project): enrollment, configuration delivery, event and status sync | 5 | later |
| **Future modules** (Monitor candidate) | Discover and select services after Remote: candidate host integrity, inventory/vulnerability triage and observability integrations; scope and module boundaries decided before delivery (D-067) | 6+ | exploratory |

Sentinel orchestrates; it does not replace CrowdSec, a WAF, a CNI or the
container runtime. More modules can be added later without touching the
core (ADR-0015).

## 2. Binding decisions

| Topic | Decision |
|---|---|
| Language | Go, minimum **1.27** |
| License | Apache License 2.0 |
| Name | **Sentinel Watchdog** ("Sentinel" in prose); module `github.com/sentinel-watchdog/sentinel-watchdog`; binaries `sentineld`, `sentinelctl`; package `sentinel-watchdog`; paths `/etc/sentinel`, `/var/lib/sentinel`, `/run/sentinel` (D-057) |
| Architecture | Modular monolith (ADR-0001): `internal/core`, `internal/platform`, `internal/modules/<name>`; modules never import each other; explicit registry, no `init()` registration (ADR-0015) |
| Configuration | YAML only, `version: 1`; central `sentinel.yaml` (core, module switches, safety gates) + one directory per enabled module (ADR-0015) |
| State | JSON files, atomic writes, versioned schema per module, under `daemon.state_dir` |
| Scheduler | Traditional 5-field cron syntax |
| Notifications | Core service; generic JSON webhook first (Slack/Teams later) |
| systemd | Supervise and restart **existing** units only; never create/modify/enable/disable units |
| Daemon ↔ CLI | Unix domain socket, default `/run/sentinel/sentinel.sock`, no TCP listener; the remote module (Phase 5) connects outbound only |
| CI | GitHub Actions; actions pinned by SHA; supply-chain checks from Phase 1 |
| Targets | RHEL 8+ and compatibles (Rocky, Alma), Debian 12+, Ubuntu 22.04+ (systemd), Alpine (OpenRC); amd64 + arm64 |
| Packages | tarball, DEB, RPM (APK later) |
| Integrations | Every external integration replaceable, mockable, testable without the real software in standard CI |
| Firewall backends | nftables preferred; iptables only if nftables is unavailable or explicitly configured; never both on the same policy |
| Firewall safety | dry_run, plan, read_only, confirm_required, managed_only, transaction IDs, audit, rollback, backup, validation, duplicate blocking, admin-access protection, safety timeout, post-apply verification; **never a global flush** |
| Enforcement | No firewall, container or Kubernetes change without an explicit enforcing mode set in the central file |
| CrowdSec | Detects; Sentinel observes and applies decisions as a remediation component; no re-implementation of scenarios |
| Kubernetes | NetworkPolicy never assumed enforced; no full controller in the first deliveries |
| Versioning | Semantic versioning; v1.0.0 = supervisor; new modules and capabilities are minor releases (D-065) |
| Delivery | Incremental phases; implement only what the current phase includes |

## 3. Decisions

All decisions D-001 … D-069 are in [docs/decisions.md](docs/decisions.md).
The ones that shape the current plan:

- D-063 / ADR-0015 — core, platform and modules; configuration layout.
- D-064 — clean restart: the old Phase 1 code is a prototype, ported
  piece by piece in Phase 2.
- D-065 — phases 1–6 and versioning.
- D-066 — open source and Go learning first, with parallel commercial
  validation; Q-017 tracks evidence for any later change in delivery order.
- D-067 — future modules require product discovery before design/delivery;
  Monitor and observability integrations are candidate directions.
- D-068 — this repository owns only the agent; product coordination lives
  temporarily in the workspace parent; dashboard delivery is separate.

## 4. Risks

| ID | Risk | Mitigation |
|---|---|---|
| R-001 | Restarting systemd units and switching process users needs privileges. A dedicated unprivileged user cannot run `systemctl restart` without polkit/sudo, nor `setuid` children. | Default unit runs as root with a reduced `CapabilityBoundingSet` and strict sandboxing; document an unprivileged mode (polkit rule scoped to listed units, no `user:` in process supervisors). Decide final defaults in Phase 3d. |
| R-002 | `ProtectSystem=strict` may block writes needed by supervised processes (they inherit the sandbox). | Document `ReadWritePaths=` drop-ins; ship conservative defaults. |
| R-003 | Cron jobs across daemon restarts: duplicates or missed runs. | Persist last scheduled slot per job (D-018), re-compute next slot on start. |
| R-004 | With `CGO_ENABLED=0`, `os/user` reads only `/etc/passwd`/`/etc/group` (no SSSD/LDAP). | Accept numeric `uid`/`gid` in `user`/`group`; document. |
| R-005 | Supervised processes must not survive Sentinel (orphans). | Own process group + `Pdeathsig` on Linux, SIGTERM → timeout → SIGKILL to the group on shutdown. |
| R-006 | Secrets may leak through error strings (URLs with tokens, headers). | `internal/redact` used by logging, `config show`, events and notifications. |
| R-007 | systemd is absent on Alpine. | systemd supervisors report `unavailable` at runtime (not a config error, so one config can be shared); OpenRC supervisor post-MVP. |
| R-008 | Webhook outages could block supervisors. | Async bounded notification queue, retries with backoff, drop + log when full. |
| R-009 | **Firewall lockout** of administrators. | Safe defaults, plan + fingerprint, protected access (D-046), confirmation, `safety_timeout` auto-rollback, rollback at start-up, docs recommending out-of-band console (ADR-0003/0005). |
| R-010 | `nft` JSON input/output coverage differs between distro versions (RHEL 8 minors, Ubuntu 22.04, Alpine). | Phase 4a verification matrix in containers per target; gaps reported as `unsupported`; fallback to native-script writes only via a new ADR (ADR-0006). |
| R-011 | Docker, firewalld, kube-proxy, netavark, CrowdSec bouncer change rules concurrently; Sentinel's `accept` is not final across tables/chains. | Owned objects only, conflict detection, fingerprint re-check under lock, post-apply verification, explicit documentation of precedence. |
| R-012 | Container sockets and kubeconfig grant elevated privileges. | Opt-in, GET-only gates, RBAC self-review, documentation (threat model T-10/T-11). |
| R-013 | Scope creep turns Sentinel into a monolith or a WAF/IDS. | ADR-0001/ADR-0015 dependency rules + architecture test (Phase 2a); non-goals in the threat model; each provider read-only first. |
| R-014 | Poisoned decisions or feeds cause mass blocking. | Allowlists, protected access, origin filter, caps, change-ratio circuit breaker, fail-static (D-035, D-036). |
| R-015 | One process holds every privilege of every enabled domain. | Disabled domains not constructed; seams for an out-of-process firewall agent if needed (ADR-0001). |
| R-016 | Privileged integration tests could modify the CI runner's network. | Run only in disposable containers with their own netns; separate, opt-in jobs (D-043). |
| R-017 | NetworkPolicy objects give a false sense of isolation without a policy-capable CNI. | Never report enforcement as `supported` from heuristics; docs and status state the CNI dependency (D-038). |
| R-018 | The full scope (supervisor, firewall with containers/Kubernetes/WAF, remote, monitor) is large for two developers working as a hobby. | One module at a time; v1.0.0 is the supervisor only; the firewall ships in 1.x marked experimental until 4c; remote and monitor are not planned in detail before their ADRs (D-065); strict "planned features fail loudly" keeps partial releases honest. |
| R-019 | client-go adds a large dependency graph and follows the Kubernetes release cadence. | Confined to one package, pinned, Dependabot-updated one minor at a time, size tracked, `nokubernetes` build tag (D-055). |
| R-020 | Integration tests on personal VMs are not reproducible by contributors. | Same tests also run on GitHub-hosted runners where possible; VM runs are documented in `docs/development.md` (D-056). |
| R-021 | The remote module turns the dashboard into a path to root on every node (configuration contains commands run as root). | Design-first ADR (Phase 5): enrollment, mTLS, signed bundles, outbound-only connection; module switches, safety gates and command execution stay under the local central file (ADR-0015 rule 1). |
| R-022 | The module framework is designed before its consumers and turns out wrong or over-general. | Minimal contract (ADR-0015 §2), exercised by a test module in 2a and by the supervisor in 3; reviewed at the start of Phase 4 when the second module arrives. |
| R-023 | Vulnerability matching, stale feeds or inferred exposure produce misleading priorities or silently hide urgent findings. | Proposed Monitor experiment (Q-017): distro-aware scanner evidence, explicit unknown states, dated provenance, independent review of excluded/lower-ranked findings, and post-remediation verification; see [startup assessment](docs/startup-assessment.md). |


## 5. Target repository layout

```
cmd/sentineld/                 daemon entrypoint                                        (2c)
cmd/sentinelctl/               CLI entrypoint; module commands under the module name    (2c)
internal/daemon/               module registry, wiring, lifecycle, signals, reload       (2c)
internal/core/config/          central file, module directories, strict decoding, ${VAR} (2a)
internal/core/module/          module contract and status                                (2a)
internal/core/clock/           Clock interface + fake                                    (2a)
internal/core/logging/         slog text/json/journal + redaction                        (2a, ported)
internal/core/redact/          secret redaction                                          (2a, ported)
internal/core/cronexpr/        cron parser; Next() in 3c                                 (2a, ported)
internal/core/events/          event bus, bounded queues                                 (2b)
internal/core/state/           atomic per-module state stores                            (2b, ported)
internal/core/notify/          dispatcher + webhook provider                             (2b)
internal/core/transport/       Unix socket server/client                                 (2c)
internal/core/authz/           SO_PEERCRED tiers read/operate/admin                      (2c)
internal/core/audit/           hash-chained audit log                                    (2c)
internal/core/provider/        provider/capability status, gated clients                 (4a)
internal/core/netspec/         IP/CIDR/port/protocol parsing                             (4a)
internal/platform/executor/    process spawning, creds, output sinks, gated runner       (3b)
internal/platform/privilege/   uid/gid resolution, capability checks                     (3b)
internal/platform/systemd/     systemctl wrapper                                         (3c)
internal/platform/logsource/   journald and file log reading                             (3c)
internal/platform/{docker,podman}/  container runtime APIs (GET-only)                    (4d)
internal/platform/kubernetes/  client-go, read-only gate                                 (4d)
internal/modules/supervisor/   module; services/{http,process,systemd}; jobs; recovery; scheduler   (3)
internal/modules/firewall/     module; model, planner, transaction, nftables, iptables, blocklist, crowdsec, waf, networkpolicy (4)
internal/modules/remote/       (5)
internal/modules/monitor/      (6+ candidate: created only if selected, D-067)
pkg/model/                     public types: events, states, module and provider status  (2b)
pkg/api/                       versioned socket protocol, command tiers                  (2c)
configs/                       central file and per-module examples, profiles (D-058)
deploy/{systemd,openrc,kubernetes}/                                                      (3d, 4d)
tests/integration/<area>/      real-software tests, tags integration[,privileged]       (from 3b)
.github/workflows/             ci, codeql, scorecard (1); release (3d); integration-* (3b+)
docs/                          architecture, configuration, decisions, adr/, threat-model, development (1), operations/security (2c), per module
```

## 6. Key interfaces (sketches, refined when implemented)

```go
// internal/core/module — implemented by each module (ADR-0015).
type Module interface {
    Name() string
    Configure(c config.ModuleConfig) (Configured, []config.Problem)
}
type Configured interface {
    Start(ctx context.Context, rt Runtime) error
    Stop(ctx context.Context) error
    Status(ctx context.Context) model.ModuleStatus
    Commands() []api.Command
}

// internal/core/clock — injected wherever timing matters.
type Clock interface { Now() time.Time; NewTimer(d time.Duration) Timer }

// internal/core/notify
type Notifier interface { Notify(ctx context.Context, ev model.Event) error }

// internal/core/audit — synchronous; failure aborts enforcement (ADR-0011).
type Recorder interface { Record(ctx context.Context, r audit.Record) error }

// internal/platform/executor — abstracts exec for tests (systemctl, nft, iptables).
type Runner interface { Run(ctx context.Context, cmd Command) (Output, error) }

// internal/modules/supervisor — one probe vs long-running.
type Checker interface { Check(ctx context.Context) Result }                     // http, systemd
type Watcher interface { Run(ctx context.Context, report func(Result)) error }   // process, cron

// internal/modules/supervisor/recovery
type Action interface { Name() string; Execute(ctx context.Context, req ActionRequest) error }

// internal/modules/firewall (ADR-0005)
type Observer interface {
    Name() string
    Probe(ctx context.Context) (Capabilities, error)
    Observe(ctx context.Context) (fwmodel.Observed, error)
    Render(p fwmodel.Plan) (Payload, error)
    Check(ctx context.Context, p Payload) error
}
type Committer interface {
    Commit(ctx context.Context, p Payload) error
    Restore(ctx context.Context, s fwmodel.Snapshot) error
}
type SetUpdater interface { // blocklists → firewall sets (ADR-0008)
    UpdateSet(ctx context.Context, set string, add, remove []netip.Prefix, ttl map[netip.Prefix]time.Duration) (UpdateResult, error)
}
```

## 7. Phases

Status legend: `todo` · `in progress` · `done`. Work on the first
sub-phase that is not `done`.

| # | Phase | Status | Ends with |
|---|---|---|---|
| 0 | Prototype and design baseline | `done` | ADRs, threat model, prototype code |
| 1 | Repository, CI and security baseline | `done` | protected repo, green CI |
| 2a | Core: libraries, configuration loader, module framework | `todo` | central file + module dirs validated |
| 2b | Core: events, state, notifications | `todo` | webhook delivery tested |
| 2c | Core: daemon, control socket, authorization, audit, CLI | `todo` | runnable `sentineld` / `sentinelctl` with zero modules |
| 3a | Supervisor: module skeleton, HTTP services | `todo` | v0.1.0 (preview) |
| 3b | Supervisor: executor, processes, recovery | `todo` | v0.2.0 (preview) |
| 3c | Supervisor: systemd services, cron jobs, logs | `todo` | v0.3.0 (preview) |
| 3d | Packaging and release pipeline | `todo` | signed DEB/RPM/tarball |
| 3e | Hardening and stabilisation | `todo` | **v1.0.0** |
| 4a | Firewall: providers, nftables observe/plan/dry-run | `todo` | v1.1.0 (experimental) |
| 4b | Firewall: enforcement, confirm, rollback | `todo` | v1.2.0 (experimental) |
| 4c | Firewall: blocklists, CrowdSec remediation | `todo` | v1.3.0 (firewall stable) |
| 4d | Firewall: container hosts and Kubernetes nodes | `todo` | v1.4.0 |
| 4e | Firewall: NetworkPolicy, WAF adapters | `todo` | v1.5.0 |
| 4f | Firewall: iptables backend | `todo` | v1.6.0 |
| 5 | Remote module: agent side (design first) | `todo` | ADR, then sub-phases |
| 6+ | Future modules: discovery first (Monitor candidate, Q-015/Q-018) | `todo` | Select problem/service, experiment, ADR, then sub-phases |

### Phase 0 — Prototype and design baseline · `done` (2026-10-05 … 2026-10-06)

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

### Phase 1 — Repository, CI and security baseline · `done` (2026-10-06)

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
- [x] `task test-linux` green on `containers01` (glibc + race, musl)
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
  remote `containers01` Docker context by mistake while the task still
  used bind mounts; it pulled `golang:1.27` and `alpine:latest` and
  likely created empty directories under `/Users/ciuffo` on that host.
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
  enforces the same checks).

### Phase 2a — Core: libraries, configuration loader, module framework · `todo`

- [ ] Delete the prototype packages; port with review into
  `internal/core/`: `redact`, `logging`, `cronexpr` (fix `*/N`
  day-of-week), strict YAML decoding (bounded alias walk), `${VAR}`
  expansion, `Duration` / `ByteSize` / `FileMode`, problem collection
- [ ] `internal/core/clock`: `Clock` + fake (maintainer exercise
  candidate, Q-013)
- [ ] `internal/core/config`: central file schema (`version`, `daemon`,
  `notifications`, `modules`), module directories, `ModuleConfig` with a
  strict decode helper, rules 1–9 of ADR-0015 (closed module names,
  disabled directories not read, `settings` once per module, ownership
  and mode checks, size limits)
- [ ] `internal/core/module`: contract, explicit registry, build-tag
  exclusion, "planned" and "not built in" errors; a test-only module
- [ ] Fuzz targets for every parser of untrusted input (configuration
  documents and `Section.Decode`, `${VAR}` expansion, `ParseByteSize`,
  `cronexpr.Parse`), run with `task fuzz` (D-069)
- [ ] Architecture test: ADR-0015 dependency rules via `go list -deps`
- [ ] Tests: loader (central + directories, merge order, duplicates,
  disabled modules, unknown/planned modules, ownership), strict decoding,
  env expansion, scalars
- [ ] docs/configuration.md rewritten; `configs/sentinel.yaml` example;
  docs/development.md patterns (interfaces, generics in the decoder,
  table-driven tests)

### Phase 2b — Core: events, state, notifications · `todo`

- [ ] `pkg/model.Event` per ADR-0002 as amended by ADR-0015: `module`,
  `source`, `source_type`, `event_type`, `severity`, `correlation_id`,
  bounded `attributes`; per-module event type registration
- [ ] `internal/core/events`: in-process bus, bounded per-subscriber
  queues, overflow accounting; dedup windows only when a module needs
  them
- [ ] `internal/core/state`: atomic store (ported), per-module state
  files with their own `schema_version`, quarantine, downgrade refusal
- [ ] `internal/core/notify`: async bounded dispatcher, webhook provider
  (method, headers, timeout, retry + backoff, accepted status codes,
  redaction), routing by module / event type / severity, `repeat_interval`
- [ ] Tests: event validation and limits, bus overflow, state corruption
  and quarantine, webhook retry/timeout (httptest)
- [ ] docs/notifications.md (payload)

### Phase 2c — Core: daemon, control socket, authorization, audit, CLI · `todo`

Goal: `sentineld` and `sentinelctl` run with zero modules.

- [ ] `pkg/api` v1: versioned newline-delimited JSON, structured errors,
  commands `<module>.<command>` with declared tier
- [ ] `internal/core/transport`: Unix socket server and client (mode and
  group from config, stale socket handling, deadlines, request size limit)
- [ ] `internal/core/authz`: `SO_PEERCRED` tiers `read` / `operate` /
  `admin` (ADR-0012), `daemon.access`
- [ ] `internal/core/audit`: hash-chained `audit.jsonl`, fsync, head check
  at start-up; every `operate`/`admin` request audited, denials included
- [ ] `internal/daemon`: registry wiring, module lifecycle, panic
  isolation, SIGTERM/SIGINT shutdown, SIGHUP reload (invalid
  configuration keeps the running one; Q-016), umask, optional
  `sd_notify`
- [ ] GoReleaser configuration and snapshot build in CI (no publishing):
  tarballs, checksums, SBOM (moved from Phase 1)
- [ ] `cmd/sentineld` (`-config`, `-validate`, `-version`);
  `cmd/sentinelctl` (`status`, `modules`, `events`, `validate [--all]`,
  `config show`, `reload`, `audit`, `version`)
- [ ] Tests: CLI against an in-process daemon, tiers, audit chain and
  tamper detection, reload, goroutine leaks
- [ ] docs/operations.md, docs/security.md (first versions)

### Phase 3a — Supervisor: module skeleton, HTTP services · `todo`

- [ ] `internal/modules/supervisor`: module, schema (`settings`,
  `services`, `jobs`), `Checker` / `Watcher`, failure policy, state,
  events (`service_failed`, `service_recovered`, …)
- [ ] HTTP services: method, headers, body, timeout, status codes,
  `body_contains` with a size limit, TLS (CA file, server name, minimum
  version), redirect policy (D-019), redacted errors
- [ ] CLI: `supervisor status [name]`, `supervisor list`
- [ ] Tests; docs/supervisor.md; `configs/supervisor/` examples; tag
  v0.1.0 (preview, binaries only)

### Phase 3b — Supervisor: executor, processes, recovery · `todo`

- [ ] `internal/platform/executor`: direct exec / explicit shell (D-005),
  environment, working directory, uid/gid, process group, `Pdeathsig`,
  stop signal → timeout → SIGKILL to the group, exit code and signal,
  output sinks (D-009), argv-allowlist runner
- [ ] `internal/platform/privilege`: user/group resolution (names and
  numeric, R-004), `CapEff` checks, clear `permission_denied`
- [ ] Process services: startup grace, crash detection; resource limits
  sampled from `/proc` (D-021)
- [ ] Recovery engine: attempts in window, delay, fixed/exponential
  backoff, `stable_after`, cooldown (D-013), exhausted, persisted attempts
  (D-014); action registry
- [ ] CLI: `supervisor start|stop|restart|enable|disable|reset <name>`
  (tier `operate`, audited)
- [ ] `task test-vm HOST=…` with the first integration tests (D-056)
- [ ] Tag v0.2.0 (preview)

### Phase 3c — Supervisor: systemd services, cron jobs, logs · `todo`

- [ ] `internal/platform/systemd`: `systemctl show` / `is-active` /
  `restart` through the Runner; `unavailable` without systemd (R-007)
- [ ] systemd services with recovery
- [ ] `cronexpr.Next()` with time zones and DST; scheduler (concurrency
  and missed-run policies, persisted last slot, D-018); jobs with
  `job_*` events
- [ ] `internal/platform/logsource` (journald reader); `supervisor logs <name>`
- [ ] Integration tests on the VMs (systemd); tag v0.3.0 (preview)

### Phase 3d — Packaging and release pipeline · `todo`

- [ ] `deploy/systemd/sentineld.service` (hardened, capabilities
  documented) and `deploy/openrc/sentineld`
- [ ] GoReleaser publishing: tarball, DEB, RPM for amd64/arm64,
  reproducible builds, checksums, SBOM, keyless signatures, provenance;
  release workflow on semver tags
- [ ] Package scripts: `sentinel` group, directories and modes, config
  `noreplace`, state kept on upgrade and removal
- [ ] Install docs per distribution; decide Q-001

### Phase 3e — Hardening and stabilisation → v1.0.0 · `todo`

- [ ] Fuzz tests (configuration, protocol), gosec review, threat model
  review
- [ ] OpenSSF Best Practices badge (bestpractices.dev, level "passing"),
  linked from README; re-check the Scorecard gaps listed in
  docs/development.md
- [ ] JSON Schema for the central file and supervisor files (D-058);
  example profile `linux-server`; error messages with fixes
- [ ] Compatibility guarantees for 1.x documented: configuration
  `version: 1`, socket API v1, event payload, audit format; deprecation
  policy
- [ ] Documentation pass, CHANGELOG, tag **v1.0.0**

### Phase 4 — Firewall module → v1.x · `todo`

Design: ADR-0003 … ADR-0008, ADR-0010, ADR-0014. The firewall is
`experimental` until 4c. Detailed checklists are written at the start of
each sub-phase from the ADRs.

- 4a — `internal/core/provider` (ADR-0004), `internal/core/netspec`,
  gated clients; firewall module with central gates (`mode`, `dry_run`,
  `protected_access`, `safety_timeout`, `confirm_required`, `allow_*`
  permissions) and directory content (policies, rules); nftables
  observer, planner, diff, fingerprint, conflict detection, dry-run; CLI
  `firewall status|capabilities|plan|diff|validate`; netns integration
  matrix (D-043, R-010) → v1.1.0
- 4b — transactions, lock, backup, commit, verify, confirm, rollback
  (manual, on error, on timeout, at start-up), backend pinning (D-034);
  CLI `firewall apply|confirm|rollback|transactions` (tier `admin`) → v1.2.0
- 4c — blocklists (local, file; caps, breaker, TTL; D-035); CrowdSec LAPI
  client and Sentinel as remediation component (`sync_decisions`, D-036);
  coexistence with other bouncers → v1.3.0, firewall `stable`
- 4d — `platform/{docker,podman}` discovery and the firewall path per
  container network mode; `platform/kubernetes` (client-go, D-055),
  CNI detection, DaemonSet deployment, coexistence with kube-proxy and
  the CNI; profiles `docker-host`, `kubernetes-node` → v1.4.0
- 4e — NetworkPolicy planner, drift, server-side apply of managed objects;
  WAF / reverse-proxy adapters feeding blocklists (ADR-0014) → v1.5.0
- 4f — iptables backend (ADR-0007) → v1.6.0

### Phase 5 — Remote module (agent side) · `todo`

- [ ] ADR before any code: trust model (enrollment, mTLS identity, signed
  configuration bundles, outbound-only connection), what the dashboard
  may change (module directories only; never core settings, module
  switches or safety gates; `command`/`script` only if the central file
  allows it), event and status sync, offline behaviour (R-021)
- [ ] Agree and version the agent–backend contract with the dashboard project
  (D-068); record ownership, compatibility and integration fixtures in the ADR
- [ ] Agent sub-phases defined after the ADR; backend/web delivery belongs
  to the dashboard project(s), with independent plans and releases

### Phase 6+ — Future modules: discovery before delivery · `todo`

- [ ] Complete a discovery stage for each candidate: target operator,
  problem, service, integration boundaries, minimum scope, experiment and
  go/narrow/defer/reject decision ([discovery process](docs/future-modules.md), D-067)
- [ ] Resolve Q-017 for the agent: review product validation evidence and
  [agent implications](docs/startup-assessment.md); revise D-065 through a
  decision and PR only if an agent roadmap change is justified
- [ ] Decide Q-015: select Monitor scope from candidate host integrity,
  package inventory, vulnerability triage and dashboard sync capabilities
- [ ] Decide Q-018: evaluate OpenObserve, Prometheus and Grafana integrations
  and whether the chosen capability needs a module, adapter or exporter
- [ ] For each selected direction: ADR, security/dependency review,
  acceptance criteria and delivery sub-phases; no release date before selection

### Product coordination — outside agent delivery (D-068)

The proposed commercial experiment, interviews, report-import research and
priced pilots are tracked in the workspace parent's `ROADMAP.md` and
`docs/startup-assessment.md`. They are not agent phase checkboxes or agent
release requirements. The local [assessment summary](docs/startup-assessment.md)
records only the constraints and evidence needed for Q-017 and R-023.

### Repository scope review · completed (2026-10-07)

- [x] Agent ownership and cross-project boundaries documented (D-068).
- [x] Global roadmap, commercial assessment and discovery process prepared
  in the workspace parent; agent summaries stay self-contained.
- [x] Phase 5 limited to the Remote module and contract/integration work.
- Runtime phases are unchanged; Phase 2a remains `todo` and is next.

### Backlog (unscheduled)

Supervisor: port, mount, log, advanced resource and `process_group`
services; OpenRC service type; cgroups v2 limits; recovery action
`execute`; container restart actions.

Core: Slack and Teams notification providers; Prometheus metrics; local
HTTP API; APK package.

Future directions: OpenObserve, Prometheus and Grafana integrations;
host integrity; inventory/vulnerability triage; operational/security
correlation. All require discovery (D-067, [candidate roles](docs/future-modules.md)).
The core Prometheus metrics item above is limited to Sentinel telemetry.

Firewall: HTTPS blocklists with checksum/signature (Q-011);
threat-intelligence feeds; Fail2ban integration; containerd adapter; more
WAF/proxy adapters (HAProxy, Caddy, Envoy, generic); admission
integration; eBPF/CNI-specific policies; `DOCKER-USER` / forward rules
(explicit opt-in); advanced remediation workflows; out-of-process
firewall agent (R-015).

### Phase mapping

Phase numbers before D-065, as used in ADR-0001…0014 and D-001…D-062:

| Old | New |
|---|---|
| 1 Foundation, 2 Design baseline | 0 |
| 3 CI, dev loop, event model, event bus | 1 (CI, dev loop), 2b (events, state) |
| 4 Recovery, notifications, cron `Next()` | 3b (recovery), 2b (notifications), 3c (cron) |
| 5 Walking skeleton (daemon, CLI, HTTP) | 2c (daemon, CLI), 3a (HTTP) |
| 6 Executor, process monitor | 3b |
| 7 systemd, cron | 3c |
| 8 Authorization, audit, reload | 2c |
| 9 Packaging | 1 (snapshot), 3d |
| 10 Hardening (v0.1.0) | 3e (v1.0.0) |
| 11 Provider framework, netspec | 4a |
| 12, 13 nftables observe/plan, enforcement | 4a, 4b |
| 14, 15 Blocklists, CrowdSec | 4c |
| 16, 17 Docker/Podman, Kubernetes read-only | 4d |
| 18, 19 NetworkPolicy planner, WAF | 4e |
| 20 Kubernetes enforcement | 4e |
| 21 iptables | 4f |
| 22 Stabilisation (v1.0.0) | 3e (supervisor); later releases per module |

## 8. Open questions

- Q-001: Default service user for sentineld (root + sandbox vs dedicated
  user + polkit). Proposed default in R-001; confirm in Phase 3d.
- Q-006: `managed_only: false` — what would it mean (attach to
  operator-defined chains?). Rejected as unsupported until a use case is
  designed.
- Q-011: Signature format for remote blocklists (sha256 file, minisign,
  …). Decide when HTTPS sources are scheduled.
- Q-013: How hands-on the maintainer is per phase (D-060): **decided
  phase by phase**. At the start of each phase the agent proposes one
  small, well-bounded package the maintainer could write against tests
  provided first; the maintainer chooses.
- Q-014: arm64 test machines **may become available**. Until then arm64 is
  covered by cross-compilation and CI (QEMU on GitHub runners); when a
  machine exists, add it to `task test-vm` (D-056).
- Q-015: Monitor module (D-062): scope versus Wazuh/OSSEC (agent only, no
  manager/SIEM; rootcheck and log analysis in or out?), whether the
  planned `log` service type belongs there instead, vulnerability feed
  sources (OSV, distribution trackers) and their licensing. Decide during
  Phase 6+ discovery (D-067).
- Q-016: Reload granularity: restart only the modules whose configuration
  changed, or reconfigure in place (keeping unchanged services running)?
  Decide in Phase 2c.
- Q-017: Agent roadmap impact of product validation: does reviewed operator
  evidence justify reprioritising agent capabilities? Commercial research
  is owned by product coordination (D-068); [agent implications](docs/startup-assessment.md)
  remain local. Open source and Go learning remain first (D-066). Resolve
  before reordering D-065; phase statuses and v1.0.0 scope stay unchanged.
- Q-018: Observability service: what concrete operator problem should
  Sentinel solve with OpenObserve, Prometheus or Grafana? Decide export,
  collection/query/correlation boundaries, reuse of existing tools and
  module versus adapter/exporter ownership during discovery (D-067).

Closed: Q-002 (D-040), Q-003 (D-057), Q-004 (D-047), Q-005 (D-048),
Q-007 (D-054), Q-008 (D-055), Q-009 (D-049), Q-010 (D-063), Q-012 (D-057).
