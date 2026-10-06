# Threat model

Status: design baseline (Phase 2, 2026-10-06). Mitigations are tagged
with the phase that implements them; nothing here is implemented unless
the README feature table says so. Review this document at the end of every
phase that adds an external integration or an enforcement path.

## 1. Scope and assumptions

In scope: `sentineld`, `sentinelctl`, their configuration, state, audit
and transaction files, the control socket, and every integration:
systemd, supervised processes, cron jobs, HTTP checks, webhooks, nftables,
iptables, blocklists, CrowdSec LAPI, Docker/Podman sockets, the Kubernetes
API and future WAF/proxy sources.

Assumptions:

- The kernel, init system, package manager and root account of the host
  are trusted. An attacker with root can defeat every control below; the
  goal then is *evidence* (audit chain, off-host logs), not prevention.
- Operators who can edit `/etc/sentinel` are administrators.
- External systems (CrowdSec, container runtimes, Kubernetes, remote
  feeds, webhook receivers) may be compromised or return malformed data.

## 2. Assets

| Asset | Why it matters |
|---|---|
| Host reachability (firewall state) | Lockout = loss of management; wrong accept/drop = exposure or outage |
| Cluster network policy | Workload isolation or outage |
| Supervised services and jobs | Availability; commands run as configured users |
| Secrets in the environment (webhook tokens, LAPI key, kube tokens) | Lateral access to other systems |
| Configuration | Defines commands executed as root and network policy |
| State, transactions, backups | Drive rollback and restart-loop protection |
| Audit log | Accountability and forensic evidence |
| Control socket | Entry point for operate/admin actions |

## 3. Actors

| Actor | Capability |
|---|---|
| Local unprivileged user | Can run `sentinelctl` if in the socket group; can create files in world-writable dirs (`/tmp`) |
| Member of `sentinel` group | `read` tier on the socket |
| Operator (`operator_group`) | `operate` tier |
| Administrator (root / `admin_group`) | `admin` tier; edits configuration |
| Compromised supervised process / container | Runs as its configured user; may control its own output, labels, exit codes |
| Compromised CrowdSec LAPI or feed | Can send arbitrary decisions/entries |
| Compromised Kubernetes credentials holder / cluster component | Can alter API responses within its RBAC |
| Network attacker | Can reach HTTP checks, webhooks, remote LAPI, remote feeds in transit |
| Malicious webhook receiver | Receives event payloads |

## 4. Trust boundaries

```
           ┌────────── host (root) ───────────────────────────────────────────────┐
 user ──▶ [B1 control socket, SO_PEERCRED, tiers] ──▶ sentineld (root)            │
           │      ▲ config files [B2 ownership/mode check]                         │
           │      │ state/audit/tx files [B3 os.Root, O_NOFOLLOW, 0600]            │
           │      ├──▶ child processes / systemctl  [B4 execve, no shell, creds]   │
           │      ├──▶ nft / iptables-restore       [B5 owned objects only]        │
           │      ├──▶ Docker/Podman socket         [B6 GET-only gate]             │
           └──────┼───────────────────────────────────────────────────────────────┘
                  ├──▶ Kubernetes API   [B7 TLS, GET-only gate, RBAC review]
                  ├──▶ CrowdSec LAPI    [B8 API key, origin filter, caps]
                  ├──▶ webhook receivers [B9 TLS, redaction]
                  └──▶ HTTP check targets / remote feeds [B10 size limits, TLS]
```

## 5. Privileges required per feature

| Feature | Privilege | Notes |
|---|---|---|
| systemd monitor (read) | none (D-Bus/`systemctl show` readable) | |
| systemd restart | root or polkit rule | R-001 |
| process/cron with `user:` | root (`setuid`/`setgid`) | |
| nftables read (`list`) | `CAP_NET_ADMIN` | even listing the ruleset needs it |
| nftables check/commit | `CAP_NET_ADMIN` | |
| iptables read/write | `CAP_NET_ADMIN` (+ `CAP_NET_RAW` for some modules) | lock file `/run/xtables.lock` |
| Docker / rootful Podman socket | socket group or root | **root-equivalent**: anyone who can talk to the socket can start a privileged container |
| Kubernetes | RBAC of the configured identity | kubeconfig may grant cluster-admin; Sentinel reports over-privilege |
| CrowdSec decisions | bouncer API key | read decisions only |
| CrowdSec alerts | machine credentials | can also *write* alerts/decisions — opt-in |

`read_only` mode still needs read privileges (e.g. `CAP_NET_ADMIN` to list
nftables). Read-only describes what Sentinel *does*, not what it *could*
do; the gates in ADR-0004 make the difference enforceable.

## 6. Threats and mitigations

| ID | Threat | Impact | Mitigations (phase) | Residual risk |
|---|---|---|---|---|
| T-01 | **Sentinel runs as root**; any bug is a root bug | Host compromise | Minimal dependencies (ADR-0013); no shell (D-005); hardened unit with `CapabilityBoundingSet`, `NoNewPrivileges` where compatible, `ProtectSystem` (Phase 9); disabled domains not constructed (ADR-0001); gosec + fuzzing (Phase 10) | Inherent; documented |
| T-02 | **YAML compromise** (write to `/etc/sentinel`) | Root command execution via process/cron monitors; arbitrary firewall policy | Refuse to load config files or `conf.d` that are not owned by root or are group/world-writable (Phase 8, like sshd `StrictModes`); enforcement-domain sections main-file only (C-12); reload audited with config hash (Phase 8) | Root-owned config edited by root is trusted by design |
| T-03 | **Unix socket abuse** | Unauthorised restart/stop, firewall apply | Socket mode/group, world-writable rejected (Phase 1); `SO_PEERCRED` tiers `read/operate/admin` (ADR-0012, Phase 8); request size limits and deadlines; audit of denials | Members of `admin_group` are root-equivalent by definition |
| T-04 | **Command injection** | Root execution | `execve` with argv, absolute paths, no `$PATH` (D-005); nft/iptables payloads via stdin in structured form, argv built only by gates (ADR-0004/0006); rule fields validated by `netspec` + strict enums; comments restricted charset; labels/annotations/decision fields never interpolated into commands | Bugs in `nft`/`iptables` parsers themselves |
| T-05 | **Firewall lockout** | Loss of management access | Defaults `enabled: false`, `read_only`, `dry_run: true`; plan + fingerprint; protected access incl. caller's SSH source; `safety_timeout` auto-rollback; rollback at start-up for expired pending tx; drop is final but Sentinel accept is not — documented (ADR-0003/0005, Phases 12–13) | Host or daemon dying during the confirmation window before restart; out-of-band console recommended |
| T-06 | **Webhook secret leakage** | Third-party access | Secrets only from env (D-006); redaction in logs, events, errors, `config show` (Phases 1–5); HTTP webhook warns; no secrets in state/audit (ADR-0011) | Receiver itself is trusted with event content |
| T-07 | **State file tampering** | Reset restart-loop protection, fake pending tx, unpin backend | Files 0600 in 0750/0700 dirs owned by daemon uid; ownership/mode check before enforcement; strict validation + quarantine (D-015); audit head comparison (ADR-0011) | Root can rewrite; detection via audit chain/off-host logs |
| T-08 | **Rollback/backup tampering** | Rollback installs attacker rules | Backup SHA-256 recorded in tx metadata and audit; restore validates hash and owned-objects-only content; restore uses the renderer, not raw replay (ADR-0006/0011, Phase 13) | Root can rewrite both |
| T-09 | **CrowdSec LAPI compromise** | Mass blocking (DoS), allowlisted IPs targeted | Origin allowlist; scope/type filter; Sentinel allowlist + protected access subtracted; caps on active decisions; rate-of-change circuit breaker; fail-static; audit with provenance (ADR-0008, Phases 14–15) | Blocking of non-allowlisted addresses within caps |
| T-10 | **kubeconfig and tokens** | Cluster modification with Sentinel's identity | Read from file at use, never logged or persisted; redacted errors; GET-only gate in read_only; RBAC self-review flags over-privilege; example read-only Role/ClusterRole; enforcement Phase 20 opt-in (ADR-0010) | Over-privileged credentials supplied by the operator |
| T-11 | **Container socket with elevated privileges** | Docker/Podman socket = root | Opt-in; GET-only gate; never expose the socket over the control socket; documented as root-equivalent; no container actions before an explicit later phase (ADR-0009) | A Sentinel compromise implies runtime control if enabled |
| T-12 | **Race conditions in firewall changes** (concurrent Sentinel ops, Docker/firewalld changing rules between plan and apply) | Wrong or partial state | Single firewall lock (mutex + `flock`); one atomic backend transaction per change; plan fingerprint re-checked under the lock; post-apply verification; owned objects only (ADR-0005/0006) | iptables: IPv4/IPv6 not atomic together (ADR-0007) |
| T-13 | **TOCTOU on files** (config, output files, state) | Writes redirected, wrong content read | Operations relative to `os.Root` handles; `O_NOFOLLOW`/`O_EXCL` temp files; atomic rename (Phase 1); ownership checks after open (fstat), not before (Phases 6–8) | Paths outside Sentinel's directories configured by the operator (output files) |
| T-14 | **Symlink attacks** (output files, state dir, `/tmp`) | Overwrite arbitrary files as root | Output files opened with `O_NOFOLLOW` and created with explicit mode; refuse output paths in world-writable directories without sticky-bit protection (Phase 6); state via `os.Root` | Operator-chosen paths |
| T-15 | **Privilege escalation** via supervised processes, environment or socket | Unprivileged → root | Children get only configured env (no daemon secrets inherited unless listed); drop supplementary groups; `Pdeathsig`; no `$PATH`; socket tiers; `NoNewPrivileges` for children where compatible (Phase 6) | Misconfigured `user: root` processes |
| T-16 | **Audit integrity** | Hiding actions | Hash chain + `audit_head` in state; fsync on enforcement records; mirror to journald; optional webhook routing; integrity status reported, never auto-repaired (ADR-0011) | Root can rewrite local copies |
| T-17 | **Remote blocklist poisoning** (future) | Blocking legitimate users | HTTPS only, explicit config, checksum/signature, size limits, fail-static, change-ratio breaker, allowlists (ADR-0008) | Compromised signer |
| T-18 | **Untrusted strings in events** (CrowdSec scenarios, labels, HTTP bodies) | Log/terminal injection, webhook consumer confusion | Control-character stripping, length limits, attribute schema and size cap at the bus (ADR-0002); JSON encoding only | Downstream consumers' own parsing |
| T-19 | **Resource exhaustion** (huge lists, event floods, slow APIs) | Daemon unresponsive | Size caps (config 4 MiB, state, response bodies), bounded queues, timeouts on every call, rate limits, set size limits (all phases) | Tuning of defaults |
| T-20 | **Silent degradation** (feature assumed to work but not active) | False sense of security | `unsupported`/`unavailable`/`permission_denied` surfaced in status and events; planned features fail loudly; NetworkPolicy never reported as enforced from heuristics; Sentinel accept never presented as guaranteed (all phases) | — |

## 7. Security invariants

These hold in every release and are covered by tests:

1. `read_only` never produces a mutating command or request (allowlisted
   argv/HTTP verbs; ADR-0004).
2. Sentinel never runs `flush ruleset`, never deletes or flushes a table,
   chain or set it does not own, and never edits Docker, firewalld,
   netavark, kube-proxy or CrowdSec objects.
3. No enforcement without: effective mode `enforce`, a fresh plan, an
   audit intent record, a backup.
4. Secrets are never written to state, audit, events, logs or plans.
5. Errors and degradations are reported, never swallowed: every provider
   failure is visible in status and as an event.
6. Sentinel never creates, edits, enables or disables systemd units.
7. Configuration that references a planned feature is rejected, not
   ignored.

## 8. Non-goals

- Protecting against a malicious root user on the same host.
- Acting as an IDS/WAF/detection engine (CrowdSec and WAFs do that).
- Guaranteeing traffic isolation that the firewall backend, container
  runtime or CNI does not provide.
