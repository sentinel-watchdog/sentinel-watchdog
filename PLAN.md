# Sentinel — Development Plan

This file is the single source of truth for multi-session development.
Every session MUST:

1. read this file first (then `CLAUDE.md` / `AGENTS.md` for conventions);
2. pick the first phase in §8 whose status is not `done`;
3. keep the **Status** checkboxes, the **Decision log** and the
   **Open questions** up to date before ending the session;
4. finish every phase with: `task check`, `GOOS=linux GOARCH=arm64 go build ./...`,
   documentation and example YAML updates, a short entry in `CHANGELOG.md`
   (Unreleased), and phase notes listing risks and what is still not
   implemented.

Never mark a feature as done when only a stub exists. Planned-but-missing
features must fail loudly (configuration error or `unsupported` status).

Team: two developers (the maintainer and an AI agent). Phases are sized
for 1–3 working sessions each and end with green CI.

Project goals (D-060): a complete tool covering Linux-only, container
and Kubernetes environments with WAF integrations; **simple to configure**
wherever safety allows (D-058); a showcase-quality open-source project;
and a way for the maintainer to learn Go — every phase explains the Go
patterns it introduces in `docs/development.md`. Detailed design lives
in [docs/adr/](docs/adr/README.md); the starting-point audit in
[docs/project-audit.md](docs/project-audit.md); threats in
[docs/threat-model.md](docs/threat-model.md).

---

## 1. Product summary

Sentinel is a Linux service, infrastructure and network-security
watchdog written in Go.

- **Supervision:** it watches systemd services, supervised processes,
  scheduled jobs and HTTP(S) endpoints (MVP), and later ports, mounts,
  logs and resources. On failure it records the event, applies a bounded
  recovery policy (restart with delay/backoff/window/max attempts),
  escalates through webhook notifications when recovery is exhausted, and
  notifies again when the target becomes healthy.
- **Network and security orchestration:** it observes and — only when
  explicitly armed — manages a dedicated host firewall table (nftables,
  iptables as legacy fallback) with plan/apply/rollback, feeds dynamic
  blocklists (local, CrowdSec decisions, later threat-intel feeds) and
  discovers Docker/Podman containers, observes Kubernetes clusters and
  plans/applies NetworkPolicies, and integrates with WAF/reverse-proxy
  tools.
  Sentinel orchestrates; it does not replace CrowdSec, a WAF, a CNI or the
  container runtime.

Binaries: `sentineld` (daemon) and `sentinelctl` (CLI over a Unix socket).

## 2. Binding decisions (from the specification)

| Topic | Decision |
|---|---|
| Language | Go, minimum **1.27** |
| License | Apache License 2.0 |
| Name | **Sentinel Watchdog** (short form "Sentinel" in prose); see D-057 |
| Repository | `github.com/sentinel-watchdog/sentinel-watchdog` (organisation `sentinel-watchdog`) |
| Domain | `sentinel-watchdog.io` (docs site, label prefix) |
| Configuration | YAML only, `version: 1`, main file + optional `conf.d/` |
| State | JSON file, atomic writes, versioned schema |
| Scheduler | Traditional 5-field cron syntax |
| Notifications | Generic JSON webhook only (Slack/Teams later) |
| systemd | Monitor and restart **existing** units only; never create/modify/enable/disable units |
| Daemon ↔ CLI | Unix domain socket, default `/run/sentinel/sentinel.sock`, no TCP |
| CI | GitHub Actions |
| Targets | RHEL 8+ and compatibles (Rocky, Alma), Debian 12+, Ubuntu 22.04+ (systemd), Alpine (OpenRC); amd64 + arm64 |
| Packages | tarball, DEB, RPM (APK post-MVP) |
| Architecture | Not a monolith in code: core, monitors, scheduler, recovery, events, state, notifications, firewall, containers, Kubernetes, security integrations, CLI and packaging stay separate; small Go interfaces, dependency injection, explicit providers |
| Integrations | Every external integration replaceable, mockable, testable without the real software in standard CI |
| Firewall backends | nftables preferred; iptables only if nftables is unavailable or explicitly configured; never both on the same policy without explicit configuration |
| Firewall safety | dry_run, plan, read_only, confirm_required, managed_only, transaction IDs, audit, rollback, backup, validation, duplicate blocking, admin-access protection, safety timeout, post-apply verification; **never a global flush** |
| Enforcement | No firewall, container or Kubernetes change without an explicit enforcing mode |
| CrowdSec | Detects; Sentinel observes and orchestrates; firewall or remediation component blocks. No re-implementation of scenarios |
| Kubernetes | No full controller in the first deliveries; NetworkPolicy never assumed enforced |
| Delivery | Incremental phases; implement only what the current phase includes |

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
| D-021 | Resource limits (`limits`) are modelled and validated now, enforced only for Sentinel-supervised processes (sampling `/proc`) in Phase 6. Anything not enforced reports `unsupported`. cgroups v2 is post-MVP. | "Do not claim support for a limit that is not applied." |
| D-022 | Code, docs and commit messages are in English. | Open-source audience. |
| D-023 | Executables built with `CGO_ENABLED=0` (static). | One binary for RHEL 8 / Ubuntu / Alpine (musl). Consequence: see R-004. |
| D-024 | Packaging (Phase 9) uses **GoReleaser** + its embedded **nFPM** for tarball/DEB/RPM, driven from Taskfile and the release workflow. | Maintained, reproducible (`-trimpath`, `mod_timestamp`), one config for all formats; APK is a one-line addition later. |
| D-025 | *Superseded by D-047.* Core phases 1–7 plus a separate S0–S11 track. | Replaced by a single numbered sequence once the team was confirmed as two developers. |
| D-026 | **Modular monolith** with in-process providers; domains never import each other or `daemon`; explicit factories, no `init()` registration. | ADR-0001. One binary and state store; seams allow a later out-of-process firewall agent. |
| D-027 | Event model generalised **before v0.1.0**: `source`, `source_type`, `severity`, `correlation_id`, `attributes` (bounded, typed); monitor counters move to `attributes`. Notification throttling is called `repeat_interval` (not `cooldown`, which is taken by D-013). Enforcement audit never depends on the async bus. | ADR-0002. No compatibility burden yet. (C-02, C-05) |
| D-028 | Enforcing domains use `mode: read_only \| enforce` + `dry_run: bool` (default `true`); effective modes `read_only`, `dry_run`, `enforce`. Arming needs two explicit edits. `--dry-run` on a request can only lower the mode. | ADR-0003. Safe defaults, one model for firewall and Kubernetes. |
| D-029 | Provider model: `ProviderState` (`disabled, ok, degraded, unavailable, permission_denied, unsupported, error`), `Capability`, `Conflict`, `ProviderStatus` in `pkg/model`; **no `pkg/provider`**; providers get gated clients so read-only is enforced by construction (allowlisted argv / HTTP verbs). | ADR-0004. (C-13) |
| D-030 | Firewall safety logic (planner, transactions, verification, rollback, audit) is backend-agnostic; backends implement only `Observer` (probe, observe, render, check) and `Committer` (commit, restore). The spec's `FirewallProvider` is the service facade. | ADR-0005. Same guarantees for nftables and iptables, tested once. |
| D-031 | Firewall field names: `source_cidrs`, `dest_cidrs`, `source_ports`, `dest_ports`, `interface`, `ttl`; `family` always means address-family scope `inet \| ip \| ip6`; bare IPs normalised to /32 or /128, host bits set = error; rule IDs use the D-011 name format and are unique across policies. Policy evaluation: ascending `priority`, first match wins. | ADR-0005. One spelling per concept. (C-07) |
| D-032 | nftables backend uses the `nft` binary with JSON (`-j`) for reads **and** writes, `--check` for dry runs, one invocation per transaction, table `inet sentinel`, ownership marker, rule comments `sentinel:<rule-id>`, flush only Sentinel's own chains, set elements updated by diff. No `google/nftables`, no text parsing; missing JSON support = `unsupported`. | ADR-0006. |
| D-033 | iptables backend: `SENTINEL-*` chains in `filter`, one commented jump rule per built-in chain, reads via `iptables-save` (strict parser for owned chains only), writes via `iptables-restore --noflush --wait`, `ipset` for sets/TTL when present. | ADR-0007. |
| D-034 | `backend: auto` = nftables if supported, else iptables; explicit backend = no fallback. The backend of the first commit is **pinned** in state; a different detected backend blocks enforcement until resolved. | ADR-0005. One policy never spans two backends. |
| D-035 | Dynamic entries (blocklists, CrowdSec, future feeds, WAF actions) reach the firewall **only** through `internal/blocklist` → `SetUpdater`; the static set-referencing rule goes through plan/apply once (two-step authorisation); sources fail static; empty results and large changes trip a circuit breaker. | ADR-0008. |
| D-036 | CrowdSec: stdlib LAPI client; bouncer key for decisions; alerts need machine credentials and are opt-in; only `ban` with scope `Ip`/`Range`; `origins` allowlist; `sync_decisions` makes CrowdSec a blocklist source; spec key `firewall_provider` dropped. | ADR-0008. (C-09) |
| D-037 | Docker and Podman: distinct adapters over stdlib HTTP on Unix sockets, GET-only in Phase 16; containerd deferred (gRPC dependency). | ADR-0009, ADR-0013. |
| D-038 | *Client choice superseded by D-055.* Kubernetes: separate provider (no shared backend with the host firewall); proposed stdlib REST client with kubeconfig/in-cluster/exec-credential support, confirmed by a spike at the start of candidate Phase 18; NetworkPolicy enforcement reported as `likely / unlikely / unknown`, never `supported` from heuristics. | ADR-0010. |
| D-039 | State schema v2 (providers, firewall, security, `audit_head`) with v1 migration; `audit.jsonl` append-only, hash-chained, fail-closed for enforcement; `firewall/transactions/` and `firewall/backups/`; tx ID `tx-<UTC>-<12 hex>`; file operations through `os.Root`. Individual decisions are not stored in state. | ADR-0011. (C-04) |
| D-040 | Control-plane tiers `read` / `operate` / `admin` from `SO_PEERCRED`; `admin` = root (or explicit `admin_group`); all `operate`/`admin` requests audited, including denials. | ADR-0012. Closes Q-002. (C-10) |
| D-041 | Dependency policy: stdlib first; any new module recorded here with license, version, purpose, maintenance and size impact. Evaluated and not adopted: google/nftables, go-iptables, Docker/Podman SDKs, go-cs-bouncer; deferred: client-go, containerd client, prometheus client. | ADR-0013. |
| D-042 | New top-level sections `firewall`, `blocklists`, `containers`, `kubernetes`, `security` are **main-file only**; until implemented they are rejected with "planned but not implemented" (Phase 11). | One owner for security policy; consistent with D-008. (C-12) |
| D-043 | Tests needing real nft/iptables/containers/Kubernetes/CrowdSec live in `tests/integration/<domain>/` with build tags `integration` (+ `privileged` when root/`CAP_NET_ADMIN` is needed). Privileged CI jobs run only inside disposable containers with their own network namespace, never against the runner's host network, and never in the standard job. | Spec §20–21. (C-14) |
| D-044 | CLI additions beyond the specification: `sentinelctl firewall confirm <tx-id>` (required by `safety_timeout`), `sentinelctl firewall transactions`, `sentinelctl blocklists status`, `sentinelctl audit`. | Safety timeout needs an explicit confirmation step; audit and blocklists need a read path. |
| D-045 | No systemd timer or transient unit as firewall dead-man switch; safety timeout = in-daemon timer + persisted pending transaction + rollback at start-up. | Binding decision "never create units". (C-15) |
| D-046 | Protected access default: `tcp/22` from any source, plus the caller's `SSH_CONNECTION` source for CLI-initiated plans (hint can only add protection). Plans that could drop it fail unless the rule is explicitly marked; marked plans cannot disable `safety_timeout`. Refined by D-049. | ADR-0005; lockout is the top risk (R-009). |
| D-047 | *Superseded by D-053.* **One numbered sequence of phases** (§8) replaces core + S track. Order: CI and event model → recovery and notifications → **walking skeleton** (runnable `sentineld`/`sentinelctl` with the HTTP monitor) → remaining monitors → control plane → packaging → hardening = **v0.1.0**; then provider framework → nftables observe/plan = **v0.2.0** → enforcement → blocklists + CrowdSec = **v0.3.0** → iptables → containers = **v0.4.0**. Kubernetes and WAF phases are **candidates** behind a written go/no-go review after v0.4.0. | Two developers: small phases, something runnable early to catch integration problems, releases as checkpoints, and no commitment to the largest scope items before real use. Supervision first because enforcement needs executor, authorization tiers and audit (closes Q-004). |
| D-048 | Configuration naming (closes Q-005): policy `default_action: accept \| drop` (not `default_mode: allow`), same vocabulary as rule `action`; no `kubernetes.network_policy.enforcement` (domain `mode` + `network_policy.dry_run` instead); no `security.crowdsec.firewall_provider` (`sync_decisions` feeds a blocklist; one firewall backend per host); `kubernetes.kubeconfig` defaults to empty (never `${KUBECONFIG}` in examples). General rules: one key per concept; every enforcing domain uses `enabled`, `mode`, `dry_run`; plural snake_case lists (`source_cidrs`); durations as strings. | Consistency across domains; no aliases that can disagree; D-006 makes `${VAR}` in disabled blocks a load error. |
| D-049 | Protected access (closes Q-009): `firewall.protected_access` defaults to `[{protocol: tcp, dest_ports: ["22"]}]`; an explicit list **replaces** the default; an explicit empty list is allowed with a validation warning. CLI-initiated plans also protect the caller's current session from `SSH_CONNECTION` (client address **and** the server port, so a non-standard sshd port is covered). Override per rule: `allow_protected_access_block: true`; such plans require `safety_timeout > 0`. Naming convention: explicit opt-ins to risky behaviour start with `allow_` (`allow_protected_access_block`, `allow_empty`); `insecure_skip_verify` keeps the established TLS name. | Lockout is the top risk (R-009); reading sshd config would be fragile parsing, the session hint covers the common case without it. |
| D-050 | *Superseded by D-054.* Label/annotation prefix for keys Sentinel itself defines (closes Q-007): `sentinel-watchdog.github.io/` (e.g. `sentinel-watchdog.github.io/enabled`). Valid as a Kubernetes label prefix and as a Docker/Podman label key. Kubernetes objects created by Sentinel also carry the standard `app.kubernetes.io/managed-by: sentinel`. Operator-chosen `monitor_labels` keys are free. Depends on creating the GitHub organisation `sentinel-watchdog` (Q-003); if a custom domain is bought later, the prefix still stays as is (label keys are a compatibility contract). | `sentinel.io` is not ours; `<org>.github.io` is a domain the project controls at zero cost. |
| D-051 | *Extended by D-056.* Linux dev loop from macOS: `task test-linux` runs unit and race tests in `golang:1.27` (glibc) and `golang:1.27-alpine` (musl) containers; privileged tests (nft, iptables, systemd) run in disposable containers or a local Linux VM, never on the developer's host network. | Most of Sentinel is Linux-specific (`Pdeathsig`, `SO_PEERCRED`, `/proc`, nft); macOS-only testing would hide failures until CI. |
| D-052 | Supply chain: GitHub Actions pinned by commit SHA, Dependabot for Go modules and actions, govulncheck in CI (Phase 3); release artifacts with checksums, SBOM and keyless signatures produced by GoReleaser in the release workflow (Phase 9). Tools are CI-only, not runtime dependencies. | A root daemon that manages the firewall must have verifiable releases. |
| D-053 | **All domains are committed** (Kubernetes and WAF included; no go/no-go gate). Order after v0.3.0: Docker/Podman discovery (16) → Kubernetes read-only + CNI (17) = **v0.4.0** → NetworkPolicy planner + drift (18) → WAF/reverse proxy (19) = **v0.5.0** → Kubernetes enforcement (20) → iptables backend (21) = **v0.6.0** → stabilisation (22) = **v1.0.0**. | The maintainer runs Linux-only, container and Kubernetes environments and wants one tool for all of them. iptables moves last because every target (RHEL/Rocky 8+, Debian 12+, Ubuntu 22.04+, Alpine) ships nftables; until Phase 21, `backend: auto` without nftables reports `unavailable`. |
| D-054 | Label/annotation prefix for keys Sentinel defines: **`sentinel-watchdog.io/`** (e.g. `sentinel-watchdog.io/enabled`), on the domain the project owns. Kubernetes objects also carry `app.kubernetes.io/managed-by: sentinel-watchdog`. Label keys are a compatibility contract: they never change, even if the domain were to lapse. | Owned domain, shorter than the github.io fallback. |
| D-055 | Kubernetes client: **`k8s.io/client-go`** (Apache-2.0), confined to `internal/kubernetes/client`, adopted in Phase 17 with version, module count and binary size recorded here; read-only via `WrapTransport` allowlist; fake clientset in tests; `nokubernetes` build tag available for a slim binary. | Kubernetes is now a core goal: full kubeconfig/exec-credential auth (EKS/GKE/AKS) and long-term maintenance outweigh dependency weight; the specification asks for fake-client tests. ADR-0010 updated. |
| D-056 | Test infrastructure: unit tests on macOS and in containers (`task test-linux`); integration/privileged tests on **the maintainer's Debian, Ubuntu and Rocky VMs** via `task test-vm HOST=…` (cross-compiled `go test -c` binaries copied over SSH and run with sudo — no Go toolchain on the VMs) and on GitHub-hosted runners (ephemeral VMs, tests inside a private netns). No self-hosted runner for workflows triggered by pull requests. | Real distro coverage for systemd, nftables, Docker and k3s/kind without exposing the VMs to untrusted code. |
| D-057 | Naming: project **Sentinel Watchdog**; Go module `github.com/sentinel-watchdog/sentinel-watchdog`; DEB/RPM/APK package name `sentinel-watchdog`; binaries stay **`sentineld`** and **`sentinelctl`**; unit `sentineld.service`; paths `/etc/sentinel`, `/var/lib/sentinel`, `/run/sentinel`. No vanity import path on the domain. | The longer name avoids package and search collisions (Microsoft Sentinel, HashiCorp Sentinel, `redis-sentinel`); short binaries and paths keep daily use pleasant. A vanity import path would need permanent hosting for no real gain. |
| D-058 | **Configuration simplicity** as a design rule: every key has a safe default; a working monitor or provider needs only its identifying keys; example profiles `configs/examples/{linux-server,docker-host,kubernetes-node}.yaml` grow with each phase; a JSON Schema for the YAML is generated from the Go types (editor completion and validation) before v0.1.0; validation errors say how to fix the problem. Safety gates (D-028) are the only accepted extra steps. | Main goal "complete and simple to configure"; strictness (D-008) stays, but must come with good defaults and messages. |
| D-059 | Repository automation and hygiene (GitHub Actions): CI, Dependabot, govulncheck, CodeQL, PR/issue templates, `CONTRIBUTING.md`, `SECURITY.md` (private vulnerability reporting), labels, release workflow (GoReleaser, signed artifacts), integration workflows (manual/nightly), docs site on GitHub Pages with custom domain `sentinel-watchdog.io` (MkDocs Material, CI-only tool). | Manage the project through GitHub; showcase quality for the maintainer's CV. |
| D-060 | Learning-oriented workflow: each phase adds a section to `docs/development.md` explaining the Go idioms it introduced (with links to the code); code favours clear, idiomatic Go over cleverness. How hands-on the maintainer wants to be per phase is tracked in Q-013. | The maintainer is learning Go; the explanations double as contributor documentation. |

## 4. Risks and ambiguities

| ID | Risk | Mitigation |
|---|---|---|
| R-001 | Restarting systemd units and switching process users needs privileges. A dedicated unprivileged user cannot run `systemctl restart` without polkit/sudo, nor `setuid` children. | Default unit runs as root with a reduced `CapabilityBoundingSet` and strict sandboxing; document an unprivileged mode (polkit rule scoped to listed units, no `user:` in process monitors). Decide final defaults in Phase 9. |
| R-002 | `ProtectSystem=strict` may block writes needed by supervised processes (they inherit the sandbox). | Document `ReadWritePaths=` drop-ins; ship conservative defaults. |
| R-003 | Cron jobs across daemon restarts: duplicates or missed runs. | Persist last scheduled slot per job (D-018), re-compute next slot on start. |
| R-004 | With `CGO_ENABLED=0`, `os/user` reads only `/etc/passwd`/`/etc/group` (no SSSD/LDAP). | Accept numeric `uid`/`gid` in `user`/`group`; document. |
| R-005 | Supervised processes must not survive Sentinel (orphans). | Own process group + `Pdeathsig` on Linux, SIGTERM → timeout → SIGKILL to the group on shutdown. |
| R-006 | Secrets may leak through error strings (URLs with tokens, headers). | `internal/redact` used by logging, `config show`, events and notifications. |
| R-007 | systemd is absent on Alpine. | systemd monitors report `unavailable` at runtime (not a config error, so one config can be shared); OpenRC monitor post-MVP. |
| R-008 | Webhook outages could block monitors. | Async bounded notification queue, retries with backoff, drop + log when full. |
| R-009 | **Firewall lockout** of administrators. | Safe defaults, plan + fingerprint, protected access (D-046), confirmation, `safety_timeout` auto-rollback, rollback at start-up, docs recommending out-of-band console (ADR-0003/0005). |
| R-010 | `nft` JSON input/output coverage differs between distro versions (RHEL 8 minors, Ubuntu 22.04, Alpine). | Phase 12 verification matrix in containers per target; gaps reported as `unsupported`; fallback to native-script writes only via a new ADR (ADR-0006). |
| R-011 | Docker, firewalld, kube-proxy, netavark, CrowdSec bouncer change rules concurrently; Sentinel's `accept` is not final across tables/chains. | Owned objects only, conflict detection, fingerprint re-check under lock, post-apply verification, explicit documentation of precedence. |
| R-012 | Container sockets and kubeconfig grant elevated privileges. | Opt-in, GET-only gates, RBAC self-review, documentation (threat model T-10/T-11). |
| R-013 | Scope creep turns Sentinel into a monolith or a WAF/IDS. | ADR-0001 dependency rules + architecture test (Phase 3); non-goals in the threat model; each provider read-only first. |
| R-014 | Poisoned decisions or feeds cause mass blocking. | Allowlists, protected access, origin filter, caps, change-ratio circuit breaker, fail-static (D-035, D-036). |
| R-015 | One process holds every privilege of every enabled domain. | Disabled domains not constructed; seams for an out-of-process firewall agent if needed (ADR-0001). |
| R-016 | Privileged integration tests could modify the CI runner's network. | Run only in disposable containers with their own netns; separate, opt-in jobs (D-043). |
| R-017 | NetworkPolicy objects give a false sense of isolation without a policy-capable CNI. | Never report enforcement as `supported` from heuristics; docs and status state the CNI dependency (D-038). |
| R-018 | The full scope (supervision, firewall, containers, Kubernetes, WAF) is large for two developers working as a hobby. | Small phases with green CI, a release every few phases, design-first ADRs, and strict "planned features fail loudly" so partial releases are honest (D-053). |
| R-019 | client-go adds a large dependency graph and follows the Kubernetes release cadence. | Confined to one package, pinned, Dependabot-updated one minor at a time, size tracked, `nokubernetes` build tag (D-055). |
| R-020 | Integration tests on personal VMs are not reproducible by contributors. | Same tests also run on GitHub-hosted runners where possible; VM runs are documented in `docs/development.md` (D-056). |

## 5. Target repository layout

```
cmd/sentineld/                daemon entrypoint                                   (Phase 5)
cmd/sentinelctl/              CLI entrypoint                                      (Phase 5; domain commands in their phases)
internal/config/              YAML model, loader, env expansion, validation       ✔ Phase 1 (+ sections in their phases)
internal/logging/             slog setup, journal handler, redaction hook         ✔ Phase 1
internal/redact/              secret redaction helpers                            ✔ Phase 1
internal/state/               JSON state model + atomic store                     ✔ Phase 1 (v2: Phase 3)
internal/scheduler/cronexpr/  cron expression parser (Next(): Phase 4)            ✔ parse
internal/version/             build metadata (ldflags)                            ✔ Phase 1
internal/clock/               Clock interface + fake                              (Phase 3)
internal/events/              event bus, dedup, correlation                       (Phase 3)
internal/recovery/            recovery engine + action registry                   (Phase 4)
internal/notification/        provider interface + webhook                        (Phase 4)
internal/monitor/common/      Result, Checker/Supervisor, failure policy          (Phase 5)
internal/monitor/http/        HTTP(S) monitor                                     (Phase 5)
internal/daemon/              orchestration, reload, provider wiring              (Phase 5, 8)
internal/lifecycle/           signals, shutdown                                   (Phase 5, 8)
internal/transport/           Unix socket server/client, SO_PEERCRED tiers        (Phase 5, 8)
internal/executor/            process spawning, creds, output sinks, gated runner (Phase 6)
internal/privilege/           uid/gid resolution, capability checks (CapEff)      (Phase 6)
internal/health/              resource sampling (/proc)                           (Phase 6)
internal/monitor/process/     supervised processes                                (Phase 6)
internal/monitor/systemd/     systemd units                                       (Phase 7)
internal/scheduler/           cron runner                                         (Phase 7)
internal/monitor/cron/        scheduled jobs                                      (Phase 7)
internal/audit/               hash-chained audit log                              (Phase 8)
internal/netspec/             CIDR/IP/port/protocol/family parsing                (Phase 11)
internal/provider/            provider status aggregation, capability refresh     (Phase 11)
internal/firewall/            service facade, backend selection/pinning           (Phase 12)
internal/firewall/model/      backend-agnostic policy/rule/plan model             (Phase 12)
internal/firewall/planner/    validation, diff, protected access, fingerprint     (Phase 12)
internal/firewall/nftables/   nft JSON backend                                    (Phases 12–13)
internal/firewall/transaction/ tx IDs, lock, backup, commit, confirm, rollback    (Phase 13)
internal/blocklist/           sources → validated set updates                     (Phase 14)
internal/security/crowdsec/   LAPI client, decisions, sync source                 (Phases 14–15)
internal/firewall/iptables/   iptables-save/restore backend                       (Phase 21)
internal/container/{model,unixhttp,docker,podman}/                                (Phase 16)
internal/kubernetes/{client,discovery,networkpolicy,planner}/                     (Phases 17–18, 20)
internal/security/waf/        WAF / reverse-proxy adapters                        (Phase 19)
internal/security/threatintel/ remote feeds                                       (backlog)
pkg/model/                    public types: events, states, provider status       ✔ Phase 1 (extended Phases 3, 11)
pkg/api/                      versioned socket protocol, command tiers            (Phases 5, 8)
configs/                      example configuration                               ✔ Phase 1 (profiles in examples/, D-058)
deploy/kubernetes/            read-only RBAC and DaemonSet examples               (Phase 17)
deploy/systemd, deploy/openrc                                                     (Phase 9)
packaging/                    nFPM/GoReleaser assets, scripts                     (Phase 9)
docs/                         architecture ✔, configuration ✔, threat-model ✔, project-audit ✔, adr/ ✔, development (Phase 3), others by phase; published to sentinel-watchdog.io (Phase 9)
tests/integration/<domain>/   real-software tests, tags integration[,privileged]  (from Phase 6)
.github/workflows/            ci.yml (Phase 3), release.yml (Phase 9), integration-*.yml (Phase 10+)
```

## 6. Core data model (summary)

Full detail: `docs/architecture.md` and `docs/configuration.md`.

- **Config** → `Settings`, `[]NotificationChannel`, `[]Monitor`.
  `Monitor` = `MonitorCommon` (name, type, enabled, description,
  notifications) + exactly one spec (`Systemd`, `Process`, `HTTP`, `Cron`).
  Later phases add main-file-only sections `firewall`, `blocklists`,
  `containers`, `kubernetes`, `security`
  (D-042).
- **Event** (`pkg/model`): Phase 1 shape is monitor-centric; Phase 3
  generalises it (D-027, ADR-0002): id, timestamp, hostname, version,
  source, source_type, event_type, severity, state, previous state,
  message, correlation_id, metadata, attributes. Also the webhook payload.
- **Monitor states**: `unknown, starting, running, healthy, failing,
  failed, recovering, exhausted, stopped, disabled`.
- **Capability status**: `supported, unsupported, unavailable,
  permission_denied, error`. **Provider state** (Phase 11): `disabled, ok,
  degraded, unavailable, permission_denied, unsupported, error`.
- **State file**: v1 = `schema_version`, `updated_at`, `monitors{name →
  MonitorState}`, `events` (bounded ring). `MonitorState` holds counters
  (restart_count, failure_count, consecutive_failures,
  consecutive_successes, total_recoveries), restart attempt timestamps,
  timestamps (last_check, last_transition, last_failure, last_recovery),
  last error/event/exit code, job info for cron, bounded history.
  v2 (Phase 3, ADR-0011) adds `providers`, `firewall`, `security`,
  `audit_head`.

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

// internal/recovery — action registry (Phase 4); firewall actions registered only in enforce mode.
type Action interface {
    Name() string
    Execute(ctx context.Context, req ActionRequest) error
}

// internal/notification
type Notifier interface {
    Notify(ctx context.Context, ev model.Event) error
}

// internal/executor — abstracts exec for tests (systemctl, nft, iptables mocking).
type Runner interface {
    Run(ctx context.Context, cmd Command) (Output, error)
}

// internal/state — consumed by the daemon.
type Store interface {
    Load() (*state.File, LoadReport, error)
    Save(*state.File) error
}

// internal/clock (Phase 3) — time source injected everywhere timing matters (recovery, cron, safety timeout, TTL).
type Clock interface { Now() time.Time; NewTimer(d time.Duration) Timer }

// internal/provider — status aggregation (Phase 11, ADR-0004).
type Describer interface {
    Name() string
    Status(ctx context.Context) (model.ProviderStatus, error)
}

// internal/firewall — consumed by the service (Phases 12–13, ADR-0005).
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

// internal/blocklist — consumed by blocklist, implemented by the firewall service (Phase 14, ADR-0008).
type SetUpdater interface {
    UpdateSet(ctx context.Context, set string, add, remove []netip.Prefix, ttl map[netip.Prefix]time.Duration) (UpdateResult, error)
}

// internal/audit (Phase 8, ADR-0011).
type Recorder interface {
    Record(ctx context.Context, r audit.Record) error // fsync'd; failure aborts enforcement
}
```

## 8. Phases

Status legend: `todo` · `in progress` · `done`. One sequence; work on the
first phase that is not `done`.

### 8.0 Overview

| # | Phase | Status | Ends with |
|---|---|---|---|
| 1 | Foundation | `done` | config, logging, state, model |
| 2 | Design baseline (audit, ADRs, threat model, roadmap) | `done` | docs |
| 3 | CI, Linux dev loop, event model v2, state v2, event bus | `todo` | green CI on Linux |
| 4 | Recovery engine, action registry, cron `Next()`, webhook notifications | `todo` | engine tested end-to-end in-process |
| 5 | **Walking skeleton**: `sentineld` + `sentinelctl` with HTTP monitor | `todo` | first runnable binaries |
| 6 | Executor, privileges, process monitor, resource limits | `todo` | supervised processes |
| 7 | systemd monitor, cron scheduler and jobs | `todo` | all MVP monitors |
| 8 | Control plane: authorization tiers, audit log, reload, full CLI | `todo` | production-grade control plane |
| 9 | Packaging and release pipeline | `todo` | DEB/RPM/tarball from a tag |
| 10 | Hardening | `todo` | **v0.1.0** — supervision MVP |
| 11 | Provider framework, netspec, reserved config | `todo` | provider status in CLI |
| 12 | nftables read-only, rule model, plan, dry-run | `todo` | **v0.2.0** — firewall observe/plan |
| 13 | nftables enforcement: apply, confirm, rollback, safety timeout | `todo` | managed table |
| 14 | Dynamic blocklists, CrowdSec read-only | `todo` | decisions as events |
| 15 | CrowdSec → firewall synchronisation | `todo` | **v0.3.0** — firewall + CrowdSec |
| 16 | Docker and Podman discovery | `todo` | containers observed |
| 17 | Kubernetes read-only, CNI detection | `todo` | **v0.4.0** — containers + Kubernetes observed |
| 18 | NetworkPolicy planner, drift detection | `todo` | policies planned |
| 19 | WAF / reverse-proxy integrations | `todo` | **v0.5.0** — policy planning + WAF |
| 20 | Kubernetes NetworkPolicy enforcement | `todo` | policies applied |
| 21 | iptables backend | `todo` | **v0.6.0** — legacy hosts |
| 22 | Stabilisation | `todo` | **v1.0.0** — stable schema, API, payload |

Traceability to the network/security specification: its "Fase 0" is
Phase 2; "Fase 1" is split over Phases 3 (event bus), 11 (providers,
capabilities, status) and 5/8 (CLI); "Fase 2–5" are Phases 12–15;
"Fase 6" (iptables) is Phase 21; "Fase 7–10" are Phases 16–19;
Kubernetes enforcement is Phase 20. Its milestones: A complete after 14,
B after 15, C after 21 (iptables moved last, D-053), D after 18, E after
20 + backlog (metrics, threat intel, advanced remediation).

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
  `install`, `uninstall`, `package*`, `release` are added in Phase 9.
- `config.Load` returns `*ValidationError` (problems + warnings);
  `Config.Redacted()` is ready for `sentinelctl config show`.
- `cronexpr` only parses; `Next()` with DST handling is a Phase 4 task.
- `limits` are validated but not enforced; the runtime must report
  `unsupported` until Phase 6 lands.
- `logging.Format` values equal `config.LogFormat` values; convert with
  `logging.Format(cfg.Settings.LogFormat)`.
- Verified: tests pass on macOS (race) and in `golang:1.27-alpine` as a
  non-root user; cross-builds for linux/amd64 and linux/arm64.

### Phase 2 — Design baseline · `done` (2026-10-06)

Documentation only; no Go code changed.

- [x] Repository audit: inventory, strengths, gaps, collisions C-01…C-16, required code changes ([docs/project-audit.md](docs/project-audit.md))
- [x] ADR index and ADR-0001…0014 ([docs/adr/](docs/adr/README.md))
- [x] Target architecture: domains, layering, provider model, monitoring vs enforcement, event flow, firewall pipeline ([docs/architecture.md](docs/architecture.md))
- [x] Threat model ([docs/threat-model.md](docs/threat-model.md))
- [x] Provider decisions: nftables via `nft -j`, iptables via save/restore, stdlib clients for Docker/Podman/CrowdSec, Kubernetes client proposed, dependency evaluation (ADR-0006…0010, 0013)
- [x] Roadmap as one numbered sequence with releases (D-047, revised by D-053); naming harmonisation (D-048); protected-access defaults (D-049); label prefix (D-050)
- [x] README feature table and roadmap; CHANGELOG

Phase 2 notes (carry forward):

- `task check` and `GOOS=linux GOARCH=arm64 go build ./...` pass
  unchanged (no code touched).
- The example configuration intentionally contains no `firewall`,
  `blocklists`, `containers`, `kubernetes` or `security` section: they
  are rejected as unknown keys today, and as "planned" after Phase 11.
- Follow-up decisions on 2026-10-06: Kubernetes and WAF committed
  (D-053), owned label domain (D-054), client-go (D-055), VMs (D-056),
  naming and module path `github.com/sentinel-watchdog/sentinel-watchdog`
  (D-057, code updated, `task check` green), configuration simplicity
  (D-058), GitHub automation (D-059), learning workflow (D-060).

### Phase 3 — CI, Linux dev loop, event model v2, event bus · `todo`

- [ ] `.github/workflows/ci.yml`: gofmt check, vet, golangci-lint, test, race, build matrix amd64/arm64, govulncheck; actions pinned by commit SHA; Dependabot for Go modules and actions (D-052)
- [ ] Taskfile `test-linux`: unit + race tests inside `golang:1.27` and `golang:1.27-alpine` containers from macOS (D-051)
- [ ] Taskfile `test-vm HOST=…`: cross-compile test binaries (`go test -c`, tags `integration[,privileged]`) and run them over SSH on the Debian/Ubuntu/Rocky VMs (D-056)
- [ ] Repository hygiene (D-059): CodeQL workflow, PR/issue templates, `CONTRIBUTING.md`, `SECURITY.md`, label set, branch protection notes for `main`
- [ ] `docs/development.md`: dev setup, Taskfile, test levels, VM usage, and the first "Go patterns used here" section (D-060)
- [ ] `internal/clock`: `Clock` interface + fake clock for tests
- [ ] `pkg/model.Event` generalised per ADR-0002: `source`, `source_type`, `severity`, `correlation_id`, `attributes` (key format, value types, size cap, control-char stripping); new scopes; severity defaults table
- [ ] `internal/state` schema v2 + v1→v2 migration; `AppendEvent` keyed on `source` (ADR-0011); decide Q-010
- [ ] `internal/events`: in-process bus, per-subscriber bounded queues, state-based and window-based dedup, `suppressed_count`, correlation ID propagation
- [ ] Architecture test: domain packages must not import `internal/daemon` or each other (ADR-0001)
- [ ] Tests: event validation and limits, dedup, correlation, state migration
- [ ] docs/architecture.md event section

### Phase 4 — Recovery engine, notifications · `todo`

- [ ] `internal/recovery`: policy evaluation (max_attempts in window, delay, fixed/exponential backoff with max_delay, stable_after reset, cooldown, exhausted), restart-loop protection, state transitions, emits `recovery_started` / `recovery_exhausted` / `monitor_recovered`; **generic action registry** (actions validated by registry; firewall actions absent until Phase 13)
- [ ] `cronexpr.Schedule.Next(time.Time)` with timezone and DST tests
- [ ] `internal/notification`: Notifier interface, async dispatcher (bounded queue), webhook provider (method, headers, timeout, retry + backoff, accepted status codes, redaction, never blocks monitors), `repeat_interval`
- [ ] Tests: max attempts, backoff, stable_after, cooldown, webhook retry/timeout (httptest), dispatcher overflow
- [ ] docs/notifications.md (payload v2)

### Phase 5 — Walking skeleton: daemon, CLI, HTTP monitor · `todo`

Goal: the smallest end-to-end product — `sentineld` loads the config,
runs HTTP monitors, persists state, sends webhooks, and `sentinelctl`
shows it. No root needed.

- [ ] `internal/monitor/common`: Result, Checker/Supervisor contracts, failure-policy counter
- [ ] `internal/monitor/http`: method/headers/body, timeout, status codes, body_contains with `body_max_bytes` limit, TLS (CA file, server name, min version), redirect policy, consecutive failures / successes, redacted errors
- [ ] `pkg/api` v1: versioned newline-delimited JSON, `protocol_version`, structured errors (`unknown_command`, `not_found`, `permission_denied`, `invalid_request`, `unsupported_version`, `internal`), a `tier` field per command (enforced in Phase 8)
- [ ] `internal/transport`: Unix socket server (mode/group from config, stale socket handling, deadlines, request size limit) and client
- [ ] `internal/daemon` (minimal): builds monitors from config, wires events/recovery/notifications/state, periodic + on-change state saves; monitors of types not yet runnable (process, systemd, cron) report `unsupported` at runtime — never silently skipped
- [ ] `internal/lifecycle` (minimal): SIGTERM/SIGINT graceful shutdown, umask 027
- [ ] `cmd/sentineld`: flags (`-config`, `-config-dir`, `-validate`), version
- [ ] `cmd/sentinelctl`: `status [name]`, `list`, `events`, `validate` (offline), `config show` (redacted), `version`
- [ ] Tests: HTTP monitor (status, timeout, body match, oversize body, TLS), protocol, CLI against in-process server, shutdown, goroutine-leak checks
- [ ] docs/monitors.md (http), docs/operations.md (first version)

### Phase 6 — Executor, privileges, process monitor · `todo`

- [ ] `internal/executor`: direct exec / explicit shell, env, working dir, uid/gid (`SysProcAttr.Credential`), process group, `Pdeathsig`, stop signal → timeout → SIGKILL to group, exit code + signal capture, output sinks (log/file/discard) with secure file modes (`O_NOFOLLOW`), stdin payload support, gated command builder primitives (argv allowlist)
- [ ] `internal/privilege`: user/group resolution (names + numeric), `CapEff` capability checks, "requires root" checks with clear `permission_denied`
- [ ] `internal/monitor/process`: supervised foreground process, startup grace, crash detection, recovery integration
- [ ] `internal/health`: /proc CPU & RSS sampling for supervised processes; limit actions log/notify/restart/stop/kill with `sustained_for`
- [ ] Tests: lifecycle, exit codes, signals, SIGKILL escalation, no orphans (Linux-only tests behind build tag, run via `task test-linux` and CI)
- [ ] docs/monitors.md (process)

### Phase 7 — systemd monitor, cron · `todo`

- [ ] `internal/monitor/systemd`: `systemctl show -p LoadState,ActiveState,SubState,Result,ExecMainStatus,NRestarts --value`, `is-active`; not-found / inactive / failed / unknown; restart with timeout; journal excerpt via `journalctl -u … -n N -o cat`; `unavailable` when systemd absent. Tests with mocked Runner.
- [ ] `internal/scheduler`: cron runner with timezone, overlap policy, missed-run policy, persisted last slot
- [ ] `internal/monitor/cron`: executes via executor; duration, exit code; success/failure/timeout events
- [ ] CLI: `start|stop|restart|enable|disable|reset <name>`, `logs <name>` (authorization still socket-level until Phase 8)
- [ ] Tests: systemd states with fake runner, scheduling, overlap, restart without duplicates
- [ ] docs/monitors.md (systemd, cron)

### Phase 8 — Control plane: authorization, audit, reload · `todo`

- [ ] `SO_PEERCRED` tiers `read` / `operate` / `admin` (ADR-0012); `settings.operator_group`, `settings.admin_group`
- [ ] `internal/audit`: hash-chained `audit.jsonl`, fsync, rotation, `audit_head` check at start-up, journald mirror (ADR-0011); audit of all `operate`/`admin` requests incl. denials, and of reloads
- [ ] Config/`conf.d` ownership and mode check (refuse group/world-writable or non-root-owned files; threat model T-02)
- [ ] `internal/lifecycle` complete: SIGHUP reload (invalid config keeps old; unchanged monitors keep running; diff-based start/stop), SIGUSR1 diagnostic dump, `sd_notify` readiness (optional, no dependency)
- [ ] CLI: `reload`, `audit`
- [ ] Tests: authorization per tier, audit chain and tamper detection, reload, config permission checks
- [ ] docs/security.md (tiers, audit format, privileges), docs/troubleshooting.md

### Phase 9 — Packaging and release pipeline · `todo`

- [ ] `deploy/systemd/sentineld.service` (hardened; capabilities documented; drop-in examples adding `CAP_NET_ADMIN` only for firewall use)
- [ ] `deploy/openrc/sentineld` (supervise-daemon, `need localmount`, `after net`)
- [ ] GoReleaser config: tarballs + DEB + RPM for amd64/arm64, ldflags version, reproducible builds, checksums, SBOM, keyless signing (D-052)
- [ ] Package scripts: create `sentinel` group, dirs with correct modes (state 0750, audit/firewall 0700), config as `config|noreplace`, keep state on upgrade/uninstall
- [ ] Taskfile `install`, `uninstall`, `package*`, `release`
- [ ] `.github/workflows/release.yml` on semver tags; tarball content verification; artifact upload
- [ ] Install docs per distro (Debian/Ubuntu, RHEL/Rocky/Alma, Alpine)
- [ ] Docs site: MkDocs Material built from `docs/`, published by a workflow to GitHub Pages on `sentinel-watchdog.io` (D-059)

### Phase 10 — Hardening → v0.1.0 · `todo`

- [ ] Integration tests (`//go:build integration`) in Linux containers: real systemd (Ubuntu), OpenRC (Alpine), under `tests/integration/{systemd,process}`; CI job `integration` (separate, opt-in)
- [ ] Security review (gosec, path validation, socket auth), fuzz tests for config and protocol; threat model review
- [ ] Configuration simplicity pass (D-058): JSON Schema generated from the config types, example profile `linux-server`, error messages with fixes
- [ ] Documentation pass, README limits/roadmap, CHANGELOG `0.1.0`, tag

### Phase 11 — Provider framework, netspec, reserved config · `todo`

- [ ] `pkg/model`: `ProviderState`, `Capability`, `Conflict`, `ProviderStatus`, effective modes (ADR-0004)
- [ ] `internal/provider`: aggregation of `Describer`s, capability refresh (start, reload, interval), `provider_state_changed` events
- [ ] `internal/netspec`: IPv4/IPv6 address and CIDR parsing (`net/netip`), canonicalisation, host-bits check, family inference (`inet|ip|ip6`), port and port-range parsing, protocol enum, aggregation helpers
- [ ] Gated HTTP client: verb/path allowlist `RoundTripper` (used by later providers)
- [ ] Config: reserve `firewall`, `blocklists`, `containers`, `kubernetes`, `security` as main-file-only sections rejected with "planned but not implemented" (D-042)
- [ ] CLI: `firewall|containers|security status` report `disabled` / `unsupported` honestly
- [ ] Tests: CIDR validation IPv4/IPv6, ports, read-only gate allowlists, provider unavailable / permission denied classification with fakes, reserved sections
- [ ] docs/architecture.md provider section from 📐 to ✅ where implemented

### Phase 12 — nftables read-only, rule model, plan, dry-run → v0.2.0 · `todo`

- [ ] Config `firewall` section (main file only, D-048/D-049): `enabled`, `backend` (`auto|nftables`; `iptables` rejected until Phase 21), `mode`, `dry_run`, `family` (`inet`), `table`, `chain_priority`, `managed_only` (only `true`), `confirm_required`, `rollback_on_error`, `safety_timeout`, `protected_access`, `nft_path`, `policies[]` with `default_action` and rules (ADR-0005, D-031); `mode: enforce` with `dry_run: false` rejected as "planned (Phase 13)"
- [ ] `internal/firewall/model`, `internal/firewall/planner`: validation, normalisation, duplicates, conflicts/shadowing warnings, protected-access guard, limits, TTL shape check, diff, fingerprint
- [ ] `internal/firewall/nftables` Observer: probe (binary, JSON, `CapEff`, IPv6, feature probes via `--check`), observe owned table, render JSON payload (owned-objects-only invariant), check
- [ ] Conflict detection (firewalld, ufw, Docker, netavark, CrowdSec bouncer, kube-proxy, mixed iptables) — informational
- [ ] `internal/firewall` service: read_only and dry_run flows; backend selection `auto|nftables`
- [ ] CLI: `firewall status|capabilities|list|plan|diff|validate`; `firewall apply --dry-run` (audited)
- [ ] Events: `firewall_plan_created`, `firewall_conflict_detected`, `firewall_drift_detected`, provider state changes
- [ ] Tests: rule validation, CIDR IPv4/IPv6, nftables payload generation (golden JSON), plan, dry-run, duplicate rules, conflict detection, managed-only invariant (no foreign object, no `flush ruleset`), read-only argv allowlist, unavailable/permission-denied/unsupported classification
- [ ] `tests/integration/firewall` (tags `integration,privileged`): nft JSON matrix on RHEL 8/9 UBI, Ubuntu 22.04/24.04, Alpine in containers with `CAP_NET_ADMIN` + private netns; CI job `integration-firewall` (opt-in) (D-043, R-010)
- [ ] docs/firewall.md (safety model, precedence: drop final / accept local, Docker caveats), docs/nftables.md; configuration.md; example YAML; release v0.2.0

### Phase 13 — nftables enforcement · `todo`

- [ ] `internal/firewall/transaction`: tx IDs, lock (mutex + flock), fingerprint re-check, audit intent/result, backup, commit, verify, `pending_confirmation` + safety timer, confirm, rollback (manual, on error, on timeout, at start-up), retention (ADR-0011)
- [ ] nftables Committer: commit, restore, ownership marker, rollback-to-empty (delete own table)
- [ ] Backend pinning (D-034)
- [ ] Recovery actions `apply_firewall_policy`, `rollback_firewall_policy` (enforce only)
- [ ] CLI: `firewall apply [--dry-run] [--yes]`, `firewall confirm <tx>`, `firewall rollback <tx>`, `firewall transactions` (tier `admin` for mutations)
- [ ] Tests: apply with fake backend and fake runner, rollback, transaction IDs, safety timeout with fake clock, start-up rollback, stale plan, verify mismatch, audit events and fail-closed audit, protected-access refusal
- [ ] Integration: apply/verify/rollback in disposable netns containers
- [ ] docs/firewall.md (operations, recovery from lockout), docs/security.md update; threat model review

### Phase 14 — Dynamic blocklists, CrowdSec read-only · `todo`

- [ ] `internal/blocklist`: sources `local` and `file`, validation, allowlist subtraction, caps, change-ratio breaker, `allow_empty`, fail-static, TTL, `SetUpdater` integration (dry_run/enforce per firewall mode), audit summaries
- [ ] Config `blocklists[]` (main file only)
- [ ] `internal/security/crowdsec`: LAPI client (stdlib), bouncer key, decisions stream/snapshot, optional alerts with machine credentials, origin/scope/type filters, provenance, status
- [ ] Events: `blocklist_updated`, `blocklist_update_failed`, `security_decision_added`, `security_decision_removed`; webhook routing for security events
- [ ] CLI: `blocklists status`, `security status|crowdsec|decisions`
- [ ] Tests: blocklist expiration, CIDR validation, allowlist, breaker, CrowdSec mock API (httptest), decisions, provenance filter, webhook security events
- [ ] docs/crowdsec.md, docs/firewall.md (blocklists), configuration.md, example YAML

### Phase 15 — CrowdSec → firewall synchronisation → v0.3.0 · `todo`

- [ ] `source: crowdsec` blocklist; `sync_decisions`; decision TTL → set timeouts; IPv4/IPv6 sets
- [ ] Safety: rate limit of new decisions, maximum active decisions, allowlists, protected access, coexistence warning with the CrowdSec firewall bouncer
- [ ] Rollback of a sync batch; audit with provenance
- [ ] Tests: sync with fake LAPI + fake firewall, poisoning scenarios (mass decisions, allowlisted IPs, unknown origins), expiry
- [ ] docs/crowdsec.md (sync), threat model review; release v0.3.0

### Phase 16 — Docker and Podman discovery · `todo`

- [ ] `internal/container/{model,unixhttp,docker,podman}`: detection, version negotiation, list, inspect, events stream, GET-only gate (ADR-0009)
- [ ] Config `containers` (main file only): `enabled`, `runtime`, `socket`, `discovery_interval`, `monitor_labels`; `restart_failed`/`firewall_integration` rejected as planned; Sentinel-defined labels use the `sentinel-watchdog.io/` prefix (D-054)
- [ ] Networking mode per container and the firewall path that applies (INPUT vs FORWARD/DNAT, host network, rootless)
- [ ] Security event correlation consumer (IP ↔ container ↔ published port)
- [ ] CLI: `containers status|list|inspect <id>`
- [ ] Tests: fixtures per runtime/API version, socket missing / permission denied, label filters; integration on the VMs with Docker and Podman
- [ ] docs/containers.md (Docker firewall facts, precedence, no isolation promises); example profile `docker-host`

### Phase 17 — Kubernetes read-only, CNI detection → v0.4.0 · `todo`

- [ ] Adopt client-go (D-055): record version, license, module count, binary size delta; `nokubernetes` build tag
- [ ] `internal/kubernetes/client`: in-cluster, kubeconfig, context, exec credentials via client-go; read-only `WrapTransport` gate
- [ ] RBAC self-review, `over_privileged` status; `deploy/kubernetes/` read-only Role/ClusterRole (+ optional DaemonSet) examples
- [ ] `internal/kubernetes/discovery`: version, namespaces, pods, services, nodes, NetworkPolicies, Kubernetes events, container restarts
- [ ] CNI detection with confidence; `network_policy_enforcement: likely|unlikely|unknown`
- [ ] Config `kubernetes` (main file only, D-048 naming)
- [ ] CLI: `kubernetes status|capabilities|policies`
- [ ] Tests: fake clientset, unsupported CNI, read-only enforcement, auth errors; integration with k3s/kind on a VM or GitHub-hosted runner
- [ ] docs/kubernetes.md (CNI dependency of NetworkPolicy); example profile `kubernetes-node`; release v0.4.0

### Phase 18 — NetworkPolicy planner, drift detection · `todo`

- [ ] `internal/kubernetes/networkpolicy`, `planner`: templates (default deny, ingress/egress, namespace and pod labels), generation, validation, managed-by labels, diff, drift events, server-side `dryRun=All` in dry_run
- [ ] CLI: `kubernetes plan` (+ diff)
- [ ] Tests: NetworkPolicy generation (golden manifests), drift, unsupported CNI warnings
- [ ] CI: manifest verification (kubeconform, pinned); `kind` integration workflow
- [ ] docs/kubernetes.md (planner)

### Phase 19 — WAF / reverse-proxy integrations → v0.5.0 · `todo`

- [ ] Confirm ADR-0014; first adapters: CrowdSec AppSec (via LAPI), one proxy access-log adapter (Nginx or Traefik), Coraza/ModSecurity audit log; others in the backlog
- [ ] `internal/security/waf`: read-only `SecurityProvider` adapters, event conversion, correlation, remediation proposals executed only through the blocklist path
- [ ] Config `security.waf` (main file only)
- [ ] Tests with recorded logs/API fixtures
- [ ] docs/waf.md; release v0.5.0

### Phase 20 — Kubernetes NetworkPolicy enforcement · `todo`

- [ ] Server-side apply (field manager `sentinel-watchdog`), managed objects only, audit, read-back verification, rollback to previous manifests
- [ ] CLI `kubernetes apply` (tier `admin`)
- [ ] Optional active enforcement verification (explicit opt-in)
- [ ] Tests: fake clientset apply/rollback, managed-only invariant; kind integration
- [ ] docs/kubernetes.md (enforcement), threat model review

### Phase 21 — iptables backend → v0.6.0 · `todo`

- [ ] `internal/firewall/iptables`: detection (legacy vs nf_tables, binaries, ipset), owned chains + jump rules, strict `iptables-save` parser, `iptables-restore --noflush --wait`, `--test`, ipset sets/TTL, snapshots
- [ ] `backend: iptables` and `auto` fallback; pinning
- [ ] Tests: detection fixtures (legacy/nft variants), parser, payload generation, conflicts (Docker chains, mixed backends), IPv4/IPv6 two-step rollback
- [ ] Integration: iptables-legacy and iptables-nft containers
- [ ] docs/iptables.md with limitations vs nftables; release v0.6.0

### Phase 22 — Stabilisation → v1.0.0 · `todo`

- [ ] Freeze and document compatibility guarantees: configuration schema `version: 1`, socket protocol, webhook/event payload, audit format, label keys
- [ ] Upgrade guide and deprecation policy; migration tests from every 0.x state schema
- [ ] Full threat model review; fuzzing campaign; performance limits documented (rules, set sizes, monitors)
- [ ] Docs site complete (install, configuration per profile, operations, security, troubleshooting, development)
- [ ] Release v1.0.0

### 8.3 Backlog (unscheduled)

Supervision: port, mount, log, advanced resource and `process_group`
monitors; OpenRC service monitor; cgroups v2 limits; recovery action
`execute`; Slack and Teams providers; Prometheus metrics; local HTTP API;
APK package; dashboard.

Network/security: HTTPS blocklists with checksum/signature;
threat-intelligence feeds; Fail2ban integration; containerd adapter;
container monitors and restart actions; more WAF/proxy adapters (HAProxy, Caddy, Envoy, generic); admission integration; eBPF/CNI-specific policies; container firewall integration
(`DOCKER-USER` / forward rules, explicit opt-in); advanced remediation
workflows; out-of-process firewall agent (R-015).

## 9. Open questions

- Q-001: Default service user for sentineld (root + sandbox vs dedicated
  user + polkit). Proposed default in R-001; confirm in Phase 9.
- Q-002: **Closed** by D-040 / ADR-0012.
- Q-003: **Closed** by D-057 (organisation `sentinel-watchdog`, repository `sentinel-watchdog`).
- Q-004: **Closed** by D-047 (supervision MVP first).
- Q-005: **Closed** by D-048.
- Q-006: `managed_only: false` — what would it mean (attach to
  operator-defined chains?). Rejected as unsupported until a use case is
  designed.
- Q-007: **Closed** by D-054.
- Q-008: **Closed** by D-055 (client-go).
- Q-009: **Closed** by D-049.
- Q-010: Key for the state directory (`settings.state_dir` derived from
  `state_file`?). Decide in Phase 3.
- Q-011: Signature format for remote blocklists (sha256 file, minisign,
  …). Decide when HTTPS sources are scheduled.
- Q-012: **Closed** by D-057 (Sentinel Watchdog).
- Q-013: How hands-on the maintainer is per phase (D-060): **decided
  phase by phase**. At the start of each phase the agent proposes one
  small, well-bounded package the maintainer could write against tests
  provided first; the maintainer chooses.
- Q-014: arm64 test machines **may become available**. Until then arm64 is
  covered by cross-compilation and CI (QEMU on GitHub runners); when a
  machine exists, add it to `task test-vm` (D-056).
