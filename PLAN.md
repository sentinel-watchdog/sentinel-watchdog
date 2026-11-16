# Sentinel Watchdog agent: Development Plan

This file is the single source of truth for multi-session **agent** development.
It covers `sentineld`, the local `sentinelctl`, modules and agent releases.
Product coordination lives in the maintainer's private coordination
repository; dashboard, website and design have their own repositories and
delivery plans. This checkout has no dependency on them. Boundaries: [project
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
sized for 1 to 3 working sessions each and end with green CI.

Project goals (D-060): one tool for Linux hosts, container hosts and
Kubernetes nodes; **simple to configure** wherever safety allows (D-058);
**secure by construction**, because Sentinel runs as root and can change
the firewall; a showcase-quality open-source project; and a way for the
maintainer to learn Go; every phase explains the Go patterns it
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
| **Core** | Configuration, events, state, notifications (webhook), audit log, control socket with authorization tiers, CLI; shared platform adapters (process execution, systemd, log reading, Docker/Podman, Kubernetes) | 2 | none |
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

All decisions D-001 … D-081 are in [docs/decisions.md](docs/decisions.md).
The ones that shape the current plan:

- D-063 / ADR-0015: core, platform and modules; configuration layout.
- D-064: clean restart; the old Phase 1 code is a prototype, ported
  piece by piece in Phase 2.
- D-065: phases 1 to 6 and versioning.
- D-066: open source and Go learning first, with parallel commercial
  validation; Q-017 tracks evidence for any later change in delivery order.
- D-067: future modules require product discovery before design/delivery;
  Monitor and observability integrations are candidate directions.
- D-068: this repository owns only the agent; dashboard delivery is separate.
- D-073: product repositories: design, website, dashboard (microservices on
  Kubernetes), private coordination, future public contract repository.

## 4. Risks

| ID | Risk | Mitigation |
|---|---|---|
| R-001 | Restarting systemd units and switching process users needs privileges. A dedicated unprivileged user cannot run `systemctl restart` without polkit/sudo, nor `setuid` children. | Default unit runs as root with a reduced `CapabilityBoundingSet` and strict sandboxing; document an unprivileged mode (polkit rule scoped to listed units, no `user:` in process supervisors). Decide final defaults in Phase 3d. |
| R-002 | `ProtectSystem=strict` may block writes needed by supervised processes (they inherit the sandbox). | Document `ReadWritePaths=` drop-ins; ship conservative defaults. |
| R-003 | Cron jobs across daemon restarts: duplicates or missed runs. | Persist last scheduled slot per job (D-018), re-compute next slot on start. |
| R-004 | With `CGO_ENABLED=0`, `os/user` reads only `/etc/passwd`/`/etc/group` (no SSSD/LDAP). | Accept numeric `uid`/`gid` in `user`/`group`; document. |
| R-005 | Supervised processes must not survive Sentinel (orphans). | Own process group + `Pdeathsig` on Linux, SIGTERM → timeout → SIGKILL to the group on shutdown. |
| R-006 | Secrets may leak through error strings (URLs with tokens, headers). | `internal/core/redact` used by logging, `config show`, events and notifications. |
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
| R-023 | Vulnerability matching, stale feeds or inferred exposure produce misleading priorities or silently hide urgent findings. | Proposed Monitor experiment (Q-017): distro-aware scanner evidence, explicit unknown states, dated provenance, independent review of excluded/lower-ranked findings, and post-remediation verification; see [agent implications](docs/future-modules.md#agent-implications-of-product-research). |
| R-024 | Kernel bugs in LSM mediation of AF_UNIX sockets crash the host: on Ubuntu `7.0.0-1014-aws`, a `write(2)` on a connected datagram socket under AppArmor (Docker's profile) oopsed in `update_sk_ctx` and hung the machine (2026-10-15). A future AppArmor profile for sentineld (Phase 3d) would put its own sockets under the same mediation. | sd_notify uses unconnected `sendto` like libsystemd (D-079); any AppArmor profile is tested on the supported kernels before it ships (3d); the bug is reported to Ubuntu; `test-linux` hosts run a kernel without the bug. |


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
internal/core/cronexpr/        cron parser, Next() across DST                            (2a, ported, 3c-3)
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
internal/platform/systemd/     systemd over D-Bus (godbus, D-094)                        (3c)
internal/platform/logsource/   journald and file log reading                             (3c)
internal/platform/journald/    native journal entries for program output (D-100)         (3c-5b)
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
// internal/core/module: implemented by each module (ADR-0015).
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

// internal/core/clock: injected wherever timing matters.
type Clock interface { Now() time.Time; NewTimer(d time.Duration) Timer }

// internal/core/notify
type Notifier interface { Notify(ctx context.Context, ev model.Event) error }

// internal/core/audit: synchronous; failure aborts enforcement (ADR-0011).
type Recorder interface { Record(ctx context.Context, r audit.Record) error }

// internal/platform/executor: abstracts exec for tests (journalctl, nft, iptables).
type Runner interface { Run(ctx context.Context, cmd Command) (Output, error) }

// internal/modules/supervisor: one probe vs long-running.
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
| 2a | Core: libraries, configuration loader, module framework | `done` | central file + module dirs validated |
| 2b | Core: events, state, notifications | `done` | webhook delivery tested |
| 2c | Core: daemon, control socket, authorization, audit, CLI | `done` | runnable `sentineld` / `sentinelctl` with zero modules |
| 3a | Supervisor: module skeleton, HTTP services | `done` | v0.1.0 (preview) |
| 3b | Supervisor: executor, processes, recovery | `done` | v0.2.0 (preview) |
| 3c | Supervisor: systemd services, cron jobs, logs | `in progress` | v0.3.0 (preview) |
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

The checklists of the done phases 0 to 3b are archived in
[docs/archive/plan-done-phases.md](docs/archive/plan-done-phases.md).

### Phase 3c (Supervisor): systemd services, cron jobs, logs · `in progress` (2026-10-29 …)

Design: D-093 (a brief agreed in a grill, a dual plan by Claude Code and
Codex with cross-critiques and lenses). Delivered in six pull requests,
each green on its own.

- [x] 3c-1 `internal/platform/systemd` over D-Bus (D-094: godbus, the
  private socket at a fixed path, bounded writes, property reads only);
  `type: systemd` services observed end to end: failures, transitions
  within `transition_grace`, restarts by systemd seen, `unavailable`
  without systemd (R-007); state schema 3; VM matrix of ten
  distributions (the minimum targets Debian 12, Ubuntu 22.04, Rocky 8, and
  Alpine 3.22 added)
- [x] 3c-2 systemd recovery (D-095): `systemd_restart` central switch and
  a per-service `recovery`, restarts in mode `fail` driven by the open
  incident, unknown outcomes, restarts by systemd counted apart and
  reported (`service_flapping`), persisted baselines with the boot id,
  aliases, operator `restart` and `reset`
- [x] 3c-3 `cronexpr.Next()` with time zones and DST (D-096: cronie's
  rules by the hour field, every offset change), impossible schedules
  refused by `Parse`; no `Latest()` (not needed by 3c-4, D-096, D-097)
- [x] 3c-4a jobs, tracer (D-097): configuration, the time zone in
  `ModuleConfig`, scheduler with `forbid` and `skip`, timeout, run records
  by run id, audit of every launch, `job_*` events, reload, status
- [x] 3c-4b jobs, completion (D-098): `allow` (with
  `settings.max_concurrent_runs`) and `replace`, `run_once`,
  `enable`/`disable` of jobs, the full reload table
- [x] 3c-5a reading logs (D-099): argv-allowlist runner in
  `internal/platform/executor` (its first consumer is `journalctl`, D-093);
  `internal/platform/logsource`; `supervisor logs <name>` for systemd units
  and `file` output (tier `operate`, ADR-0012 amended); warning for secrets
  too short to mask
- [x] 3c-5b journald fields for owned output (D-100): a native writer that
  waits at most 20 ms per line (D-009 refined), a full socket counted in status;
  `supervisor logs` of `log` output by `SENTINEL_SOURCE` and sentineld's
  `_UID`, with each job line's run id
- [x] 3c-6a integration tests on the VM matrix (D-101; D-085, extended in 3c-1):
  sentineld against real units end to end (restart within the budget,
  not armed, operator restart, flapping) and a cron job's events, audit
  and status; B-045 fixed
- [ ] 3c-6b docs pass (B-023 included)
- [ ] Tag v0.3.0 (preview)

### Phase 3d: Packaging and release pipeline · `todo`

- [ ] `deploy/systemd/sentineld.service` (hardened, capabilities
  documented) and `deploy/openrc/sentineld`
- [ ] GoReleaser publishing: tarball, DEB, RPM for amd64/arm64,
  reproducible builds, checksums, SBOM, keyless signatures, provenance;
  release workflow on semver tags
- [ ] Package scripts: `sentinel` group, directories and modes, config
  `noreplace`, state kept on upgrade and removal
- [ ] Install docs per distribution; decide Q-001

### Phase 3e: Hardening and stabilisation → v1.0.0 · `todo`

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

### Phase 4: Firewall module → v1.x · `todo`

Design: ADR-0003 … ADR-0008, ADR-0010, ADR-0014. The firewall is
`experimental` until 4c. Detailed checklists are written at the start of
each sub-phase from the ADRs.

- 4a: `internal/core/provider` (ADR-0004), `internal/core/netspec`,
  gated clients; firewall module with central gates (`mode`, `dry_run`,
  `protected_access`, `safety_timeout`, `confirm_required`, `allow_*`
  permissions) and directory content (policies, rules); nftables
  observer, planner, diff, fingerprint, conflict detection, dry-run; CLI
  `firewall status|capabilities|plan|diff|validate`; netns integration
  matrix (D-043, R-010) → v1.1.0
- 4b: transactions, lock, backup, commit, verify, confirm, rollback
  (manual, on error, on timeout, at start-up), backend pinning (D-034);
  CLI `firewall apply|confirm|rollback|transactions` (tier `admin`) → v1.2.0
- 4c: blocklists (local, file; caps, breaker, TTL; D-035); CrowdSec LAPI
  client and Sentinel as remediation component (`sync_decisions`, D-036);
  coexistence with other bouncers → v1.3.0, firewall `stable`
- 4d: `platform/{docker,podman}` discovery and the firewall path per
  container network mode; `platform/kubernetes` (client-go, D-055),
  CNI detection, DaemonSet deployment, coexistence with kube-proxy and
  the CNI; profiles `docker-host`, `kubernetes-node` → v1.4.0
- 4e: NetworkPolicy planner, drift, server-side apply of managed objects;
  WAF / reverse-proxy adapters feeding blocklists (ADR-0014) → v1.5.0
- 4f: iptables backend (ADR-0007) → v1.6.0

### Phase 5: Remote module (agent side) · `todo`

- [ ] ADR before any code: trust model (enrollment, mTLS identity, signed
  configuration bundles, outbound-only connection), what the dashboard
  may change (module directories only; never core settings, module
  switches or safety gates; `command`/`script` only if the central file
  allows it), event and status sync, offline behaviour (R-021)
- [ ] Agree and version the agent-backend contract with the dashboard project
  (D-068); record ownership, compatibility and integration fixtures in the ADR
- [ ] Agent sub-phases defined after the ADR; backend/web delivery belongs
  to the dashboard project(s), with independent plans and releases

### Phase 6+ (Future modules): discovery before delivery · `todo`

- [ ] Complete a discovery stage for each candidate: target operator,
  problem, service, integration boundaries, minimum scope, experiment and
  go/narrow/defer/reject decision ([discovery process](docs/future-modules.md), D-067)
- [ ] Resolve Q-017 for the agent: review product validation evidence and
  [agent implications](docs/future-modules.md#agent-implications-of-product-research); revise D-065 through a
  decision and PR only if an agent roadmap change is justified
- [ ] Decide Q-015: select Monitor scope from candidate host integrity,
  package inventory, vulnerability triage and dashboard sync capabilities
- [ ] Decide Q-018: evaluate OpenObserve, Prometheus and Grafana integrations
  and whether the chosen capability needs a module, adapter or exporter
- [ ] For each selected direction: ADR, security/dependency review,
  acceptance criteria and delivery sub-phases; no release date before selection

### Product coordination: outside agent delivery (D-068)

The proposed commercial experiment, interviews, report-import research and
priced pilots are tracked in the private coordination repository (D-073).
They are not agent phase checkboxes or agent release requirements. The [agent implications](docs/future-modules.md#agent-implications-of-product-research)
record only the constraints and evidence needed for Q-017 and R-023.

### Backlog (unscheduled)

Unscheduled features, deferred findings and follow-ups are listed in
[docs/backlog.md](docs/backlog.md).

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
- Q-006: `managed_only: false`: what would it mean (attach to
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
- Q-017: Agent roadmap impact of product validation: does reviewed operator
  evidence justify reprioritising agent capabilities? Commercial research
  is owned by product coordination (D-068); [agent implications](docs/future-modules.md#agent-implications-of-product-research)
  remain local. Open source and Go learning remain first (D-066). Resolve
  before reordering D-065; phase statuses and v1.0.0 scope stay unchanged.
- Q-018: Observability service: what concrete operator problem should
  Sentinel solve with OpenObserve, Prometheus or Grafana? Decide export,
  collection/query/correlation boundaries, reuse of existing tools and
  module versus adapter/exporter ownership during discovery (D-067).

Closed: Q-002 (D-040), Q-016 (D-075), Q-003 (D-057), Q-004 (D-047), Q-005 (D-048),
Q-007 (D-054), Q-008 (D-055), Q-009 (D-049), Q-010 (D-063), Q-012 (D-057).
