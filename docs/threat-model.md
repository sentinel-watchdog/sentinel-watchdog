# Threat model

Status: written 2026-10-06, reviewed at the end of Phase 3c (2026-11-16).
Each mitigation names the decision or the phase that delivers it. A phase
not reached yet (3d, 3e, 4a to 4f, 5) marks a planned mitigation; the
status of every phase is in [PLAN.md §7](../PLAN.md#7-phases). Review this
document at the end of every phase that adds an external integration or an
enforcement path. The operator's summary of what is in place today is
[security.md](security.md).

## 1. Scope and assumptions

In scope: `sentineld`, `sentinelctl`, their configuration, state, audit
and transaction files, the control socket, and every integration:
systemd, supervised processes, cron jobs, HTTP checks, the journal,
webhooks, nftables, iptables, blocklists, CrowdSec LAPI, Docker/Podman
sockets, the Kubernetes API and future WAF/proxy sources.

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
| Host reachability (firewall state) | Lockout means loss of management; a wrong accept or drop means exposure or outage |
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
| Local unprivileged user | Can run `sentinelctl` if in the socket group; can create files in world-writable directories (`/tmp`); can write journal entries |
| Member of `daemon.socket_group` | `read` tier on the socket |
| Operator (`access.operator_group`) | `operate` tier |
| Administrator (root, `access.admin_group`) | `admin` tier; edits configuration |
| Compromised supervised process or container | Runs as its configured user; controls its own output, labels and exit codes |
| Compromised CrowdSec LAPI or feed | Can send arbitrary decisions or entries |
| Compromised Kubernetes credentials holder or cluster component | Can alter API responses within its RBAC |
| Network attacker | Can reach HTTP checks, webhooks, remote LAPI and remote feeds in transit |
| Malicious webhook receiver | Receives event payloads |

## 4. Trust boundaries

```
           ┌────────── host (root) ───────────────────────────────────────────────┐
 user ──▶ [B1 control socket, SO_PEERCRED, tiers] ──▶ sentineld (root)            │
           │      ▲ config files [B2 ownership/mode check]                        │
           │      │ state/audit/tx files [B3 os.Root, O_NOFOLLOW, 0600]           │
           │      ├──▶ child processes  [B4 execve, sh only for script:, creds]   │
           │      ├──▶ systemd, private socket      [B11 root only, bounded]      │
           │      ├──▶ journal, read and write [B12 fixed argv, uid on output]    │
           │      ├──▶ nft / iptables-restore       [B5 owned objects only]       │
           │      ├──▶ Docker/Podman socket         [B6 GET-only gate]            │
           └──────┼───────────────────────────────────────────────────────────────┘
                  ├──▶ Kubernetes API   [B7 TLS, GET-only gate, RBAC review]
                  ├──▶ CrowdSec LAPI    [B8 API key, origin filter, caps]
                  ├──▶ webhook receivers [B9 TLS, redaction]
                  └──▶ HTTP check targets / remote feeds [B10 size limits, TLS]
```

B12: program output is read back only from entries journald recorded
under sentineld's uid; a systemd service's logs are read by unit and
hold whatever its processes wrote (T-21, T-22). B5 to B8 and the remote
feeds belong to the firewall and its integrations (Phase 4).

## 5. Privileges required per feature

| Feature | Privilege | Notes |
|---|---|---|
| systemd services, read | root: systemd's private socket admits root only (the system bus, open to anyone, would also let anyone send sentineld signals) | D-093, D-094 |
| systemd restart | root: `RestartUnit` over the same private socket, mode `fail` (D-095); armed by `modules.supervisor.systemd_restart`; an automatic restart also needs the service's `recovery`, an operator's `supervisor restart` does not | R-001 |
| Process services and jobs with `user:` | root (`setuid`, `setgid`) | D-084 |
| Memory limit | reads `/proc/<pid>` of the process it started, through a handle bound to that process | D-090 |
| Reading logs (`supervisor logs`) | `journalctl` run as uid 65534 (`nobody`) with group `systemd-journal` | D-099 |
| Program output to the journal | journald's native socket, as sentineld | D-100 |
| nftables read (`list`) | `CAP_NET_ADMIN` | even listing the ruleset needs it; Phase 4a |
| nftables check and commit | `CAP_NET_ADMIN` | Phase 4b |
| iptables read and write | `CAP_NET_ADMIN` (and `CAP_NET_RAW` for some modules) | lock file `/run/xtables.lock`; Phase 4f |
| Docker or rootful Podman socket | socket group or root | **root-equivalent**: anyone who can talk to the socket can start a privileged container; Phase 4d |
| Kubernetes | RBAC of the configured identity | kubeconfig may grant cluster-admin; Sentinel reports over-privilege; Phase 4d |
| CrowdSec decisions | bouncer API key | read decisions only; Phase 4c |
| CrowdSec alerts | machine credentials | can also *write* alerts and decisions; opt-in; Phase 4c |

`read_only` mode still needs read privileges (for example `CAP_NET_ADMIN`
to list nftables): in that mode Sentinel only reads, and the gates of
ADR-0004 enforce it.

## 6. Threats and mitigations

| ID | Threat | Impact | Mitigations | Residual risk |
|---|---|---|---|---|
| T-01 | Sentinel runs as root; any bug is a root bug | Host compromise | Two runtime dependencies (ADR-0013); no implicit shell: `command:` runs through `execve`, and only an explicit `script:` runs as `<shell> -c` (D-005); disabled modules are not constructed (ADR-0001, ADR-0015); gosec in the linter, a fuzz target for every parser of untrusted input (fuzzing in CI is B-002); package scripts that never start sentineld and refuse an unsafe `/etc/sentinel` or `/var/lib/sentinel` (D-102) | Inherent; the unit has no sandboxing, because supervised processes would inherit it (D-102, R-002, B-048); documented |
| T-02 | YAML compromise (write to `/etc/sentinel`) | Root command execution through process services and jobs; arbitrary firewall policy | Configuration files, module directories and every parent refused unless owned by root or the daemon user and not writable by group or others (D-070, 2a, like sshd `StrictModes`; a parent writable by others only with the sticky bit); only the central file configures the core and arms enforcing modules (ADR-0015 rule 1); process services and jobs run only with the central `modules.supervisor.processes` (D-084 (1)), and systemd units are restarted only with the central `modules.supervisor.systemd_restart` (D-095); the program, every directory on its path and every link to it must be owned by root or sentineld and not writable by group or others (a parent writable by others only with the sticky bit), checked at validation and at every launch (D-086); every reload audited: its intent with the running digest, an applied reload with the old and new digests (D-078) | Root-owned configuration edited by root is trusted by design |
| T-03 | Unix socket abuse | Unauthorised restart or stop, firewall apply | World-writable `socket_mode` refused; socket group and mode applied at bind (D-076, 2c-2); tiers `read`, `operate`, `admin` from `SO_PEERCRED` and `SO_PEERGROUPS` (ADR-0012, D-076); request size limits and deadlines; per-user connection limits; every operate/admin request audited before it runs, denials included, with the validated name of the service it acts on (D-077, D-089); supervisor operator commands take only a service name and cannot override `enabled: false`, the central switches or the recovery budget (D-089) | Members of `admin_group` are root-equivalent by definition |
| T-04 | Command injection | Root execution | `execve` with argv, absolute paths, no `$PATH` (D-005); `script:` is the operator's own shell text from the configuration, run as one argument of `<shell> -c`, and nothing from a request, an event or a program's output is ever added to it; `journalctl` runs with a fixed argv chosen from configured names only (D-099); nft/iptables payloads through stdin in structured form, argv built only by gates (ADR-0004, ADR-0006, Phase 4); rule fields validated by `netspec` and strict enums; comments in a restricted charset; labels, annotations and decision fields never interpolated into commands | Bugs in the parsers of `nft` and `iptables` |
| T-05 | Firewall lockout | Loss of management access | Defaults `enabled: false`, `read_only`, `dry_run: true`; plan and fingerprint; protected access including the caller's SSH source; `safety_timeout` automatic rollback; rollback at start-up for an expired pending transaction; a drop is final but a Sentinel accept is not, and this is documented (ADR-0003, ADR-0005, Phases 4a, 4b) | Host or daemon dying during the confirmation window before restart; out-of-band console recommended |
| T-06 | Webhook secret leakage | Third-party access | Secrets only from the environment (D-006); redaction in logs, events, errors and `config show` (2a to 2c); an `http` webhook warns; redirects never followed (D-072); no secrets in state or audit (ADR-0011) | The receiver is trusted with event content |
| T-07 | State file tampering | Reset restart-loop protection, fake a pending transaction, unpin a backend | Files `0600` in `0750` or `0700` directories owned by the daemon user; ownership and mode checked after open; strict validation and quarantine (D-015, 2b); audit head comparison at start-up (ADR-0011, 2c-3a) | Root can rewrite; detection through the audit chain and off-host logs |
| T-08 | Rollback or backup tampering | Rollback installs attacker rules | Backup SHA-256 recorded in the transaction metadata and the audit; restore validates the hash and owned-objects-only content; restore uses the renderer, not a raw replay (ADR-0006, ADR-0011, Phase 4b) | Root can rewrite both |
| T-09 | CrowdSec LAPI compromise | Mass blocking (DoS), allowlisted addresses targeted | Origin allowlist; scope and type filter; Sentinel allowlist and protected access subtracted; caps on active decisions; rate-of-change circuit breaker; fail-static; audit with provenance (ADR-0008, Phase 4c) | Blocking of addresses outside the allowlist, within the caps |
| T-10 | kubeconfig and tokens | Cluster changed with Sentinel's identity | Read from file at use, never logged or persisted; redacted errors; GET-only gate in `read_only`; RBAC self-review flags over-privilege; example read-only Role and ClusterRole; enforcement opt-in (ADR-0010, Phase 4e) | Over-privileged credentials supplied by the operator |
| T-11 | Container socket with elevated privileges | Docker or Podman socket is root | Opt-in; GET-only gate; the socket never exposed over the control socket; documented as root-equivalent; no container actions before an explicit later phase (ADR-0009, Phase 4d) | A Sentinel compromise means runtime control when enabled |
| T-12 | Race conditions in firewall changes (concurrent Sentinel operations, Docker or firewalld changing rules between plan and apply) | Wrong or partial state | One firewall lock (mutex and `flock`); one atomic backend transaction per change; plan fingerprint checked again under the lock; post-apply verification; owned objects only (ADR-0005, ADR-0006, Phase 4b) | iptables: IPv4 and IPv6 not atomic together (ADR-0007) |
| T-13 | TOCTOU on files (configuration, output files, state) | Writes redirected, wrong content read | Operations relative to `os.Root` handles; `O_NOFOLLOW` and `O_EXCL` temporary files; atomic rename (2b); ownership checked on the opened file, not before (D-070, 2a; output files 3b-1) | Paths outside Sentinel's directories chosen by the operator (output files) |
| T-14 | Symlink attacks (output files, state directory, `/tmp`) | Overwrite arbitrary files as root | Output files opened with `O_NOFOLLOW`, created with an explicit mode, refused when they have more than one hard link or sit in a directory writable by others without the sticky bit (3b-1); state through `os.Root` | Paths chosen by the operator |
| T-15 | Privilege escalation through supervised processes, the environment or the socket | Unprivileged to root | Children get only the configured environment (never sentineld's), plus a default `PATH` when it sets none, `/dev/null` as stdin and no supplementary groups unless configured; their own process group and `Pdeathsig`; absolute paths, no `$PATH` lookup (D-084, 3b-1); child output masked in the log and the journal with the configuration's secrets and the children's environment values except `PATH` (D-086, D-100); a later run kills what a killed sentineld left in a recorded group, identified by pgid, session, start time and boot (D-086); socket tiers | Processes configured with `user: root`; a process that leaves its group (`setsid`) escapes the stop; no `NoNewPrivileges` or capability limits from the unit (D-102, B-048) |
| T-16 | Audit integrity | Hiding actions | Hash chain and head file checked at start-up, fsync of every record, privileged requests refused while the chain is broken (D-077, 2c-3a); every record logged at `info`, so it reaches the journal under systemd unless `log.level` is above `info`; audit events routable to webhooks through `notifications.core`; integrity reported, never repaired automatically (ADR-0011) | Root can rewrite local copies |
| T-17 | Remote blocklist poisoning | Blocking legitimate users | HTTPS only, explicit configuration, checksum or signature, size limits, fail-static, change-ratio breaker, allowlists (ADR-0008, Phase 4c) | Compromised signer |
| T-18 | Untrusted strings in events (CrowdSec scenarios, labels, HTTP bodies) | Log or terminal injection, a confused webhook consumer | Control characters stripped, length limits, attribute schema and size cap at the bus (ADR-0002, 2b); JSON encoding only | The parsing of downstream consumers |
| T-19 | Resource exhaustion (huge lists, event floods, slow APIs, cron runs piling up, a supervised process that eats the host's memory) | Daemon unresponsive; host out of memory | Size caps (configuration 4 MiB, state, response bodies), bounded queues, timeouts on external calls; per-user connection rate on the socket (D-077); program output to the log or the journal cut at 4 KiB, each stream (stdout, stderr) of each launch limited to 100 lines per second with bursts of 1000 (3b-1); set size limits (Phase 4); a job's runs bounded by `concurrency_policy`, `settings.max_concurrent_runs` and `timeout` (D-097, D-098); `limits.memory` stops a supervised process whose leader is over its limit, sampled every 5 s from `/proc` through a handle bound to that process (never another at a reused pid), and recovery bounds the restarts (D-090); a sample that cannot be read never reads as zero | Default tuning; the memory limit is sampled, not a ceiling: a fast allocation or a process's children go past it (cgroups post-MVP, D-021); `file` output has no size limit, rate limit or rotation, so a program can fill its file system; a stalled audit write or log destination can block past the timeouts (B-004, B-021) |
| T-20 | Silent degradation (a feature assumed to work but not active) | False sense of security | `unsupported`, `unavailable` and `permission_denied` shown in status and events; planned features fail loudly; dropped journal lines counted in status (D-100), a process's lost output file shown in its status when it ends; NetworkPolicy never reported as enforced from heuristics and a Sentinel accept never presented as guaranteed (Phase 4) | A fallback from the journal to the log, and a job's lost output, are reported only in the log |
| T-21 | Service logs read for an operator (`supervisor logs`, D-099) | A local user or a service writes the journal or an output file: terminal sequences for the operator, a flood, input for a program sentineld runs. An operator-tier peer reads secrets or other files, or passes options | Names only pick configured sources, so no request text reaches an argv or a path; fixed `journalctl` path and options, as uid 65534 (`nobody`) with group `systemd-journal`; output bounded and redacted before any cut; terminal sequences quoted by the CLI; output files refused when they are links, hard-linked or writable by others; tier `operate`, audited (3c-5a) | A secret removed from the configuration, shorter than 4 bytes or split by the 4 KiB line cut shows in older lines; a systemd service's logs are read by unit and hold whatever its processes wrote |
| T-22 | Program output written to the journal (D-100) | A supervised program writes text meant to add journal fields or pass for another program's output; a local user writes entries naming a service; journald stops reading | Every field in the protocol's binary form, so a line is one value; names `[A-Z][A-Z0-9_]`; reads match sentineld's `_UID`, which journald sets from the sender's credentials, and `_TRANSPORT=journal`; a run id kept only in its exact form; sends do not block (`MSG_DONTWAIT`): a full socket is retried for at most 20 ms per line, then the line is dropped and counted, and a congested stream stops waiting; `MESSAGE` cut to 4087 bytes so it reads back (3c-5b) | A program supervised as root can forge fields under uid 0; the fallback to the logger keeps its blocking risk (B-004); delivery is best effort |

## 7. Security invariants

Invariants 4 to 7 hold in every release today; 1 to 3 bind the firewall
from Phase 4.

1. `read_only` never produces a mutating command or request (allowlisted
   argv and HTTP verbs; ADR-0004).
2. Sentinel never runs `flush ruleset`, never deletes or flushes a table,
   chain or set it does not own, and never edits Docker, firewalld,
   netavark, kube-proxy or CrowdSec objects.
3. No enforcement without: effective mode `enforce`, a fresh plan, an
   audit intent record, a backup.
4. Secrets are never written to state, audit, events or plans, and
   sentineld's own log records mask them. Masking matches known values of
   at least 4 bytes, so a shorter secret can show in configuration
   problems and logs. The output of supervised programs is masked the same
   way before it reaches the log or the journal, except literal secrets in
   `args` or `script`, a value cut by the 4 KiB line limit, and `file`
   output (D-086, D-100).
5. Errors and degradations are reported, never swallowed: in status, as
   events, in command answers or in the log, depending on the failure.
   From Phase 4, every provider failure shows in status and as an event.
6. Sentinel never creates, edits, enables or disables systemd units. The
   systemd adapter calls `LoadUnit`, `Properties.Get` and `RestartUnit`
   only (D-094, D-095).
7. Enabling a planned module or using a planned feature is rejected, not
   ignored. A planned module may be listed disabled; its directory is not
   read.

## 8. Non-goals

- Protecting against a malicious root user on the same host.
- Acting as an IDS, WAF or detection engine (CrowdSec and WAFs do that).
- Guaranteeing traffic isolation that the firewall backend, container
  runtime or CNI does not provide.
