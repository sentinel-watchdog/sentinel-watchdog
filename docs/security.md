# Security

What protects a host running Sentinel Watchdog today (the core and the
supervisor, at the end of Phase 3c), what is not in place yet, and where
each guarantee is specified. The design-level analysis is the
[threat model](threat-model.md). To report a vulnerability, see
[SECURITY.md](../SECURITY.md), never a public issue.

## Privilege model

- `sentineld` runs as root, because the supervisor starts programs as
  other users and restarts systemd units, and the firewall will change the
  network. Every bug is potentially a root bug (T-01). The runtime
  dependencies are therefore two (the YAML library, D-001, and the D-Bus
  client of the systemd adapter, D-094; ADR-0013). A program runs through
  a shell only when its configuration says `script:` (D-005), and planned
  features fail loudly instead of half-working.
- `sentinelctl` needs no privilege. It reads configuration files itself
  only for `validate`; everything else goes through the control socket,
  where the daemon decides.
- sentineld opens no network listener. The core writes the state
  directory, serves the control socket, sends webhook requests and
  sd_notify datagrams. An enabled supervisor also:
  - sends HTTP requests to the endpoints it checks;
  - starts process services and cron jobs as their configured user, with
    an argv (`command:`) or as `<shell> -c <script>` (`script:`), only
    while `modules.supervisor.processes: true`;
  - reads properties of systemd units, and calls `RestartUnit` only while
    `modules.supervisor.systemd_restart: true`: automatically for a
    service with a `recovery` mapping, or on an operator's
    `supervisor restart`, which needs no `recovery`;
  - reads `/proc` for `limits.memory`, runs `journalctl` for
    `supervisor logs`, and writes program output to the journal or to the
    output files the configuration names.

## Trust boundaries

| Input | Trusted? | Protection |
|---|---|---|
| Configuration files (`/etc/sentinel`) | as much as their owner | owned by root or the daemon user and not writable by group or others, on the whole path (a parent writable by others is accepted only with the sticky bit); opened once through `os.Root`, size-limited, strict parser (unknown and duplicate keys rejected), every problem reported ([configuration](configuration.md#file-ownership-and-modes), D-070, T-02, T-13) |
| Environment (`${NAME}` values, `NOTIFY_SOCKET`) | no | names validated, values never quoted in errors (`[REDACTED]`); `NOTIFY_SOCKET` validated, never logged, removed from the environment after it is read (D-079) |
| Control socket peers | no | identity from the kernel, tiers, limits, audit (below; T-03) |
| State and audit files from a previous run | no | `os.Root`, `O_NOFOLLOW`, regular files only, mode `0600`, ownership checked after open, strict parsers with fuzz targets (T-07, T-16) |
| Webhook receivers | with event content only | TLS verified unless explicitly disabled (warns), secrets only from the environment, redirects never followed ([notifications](notifications.md#security), D-072, T-06) |
| Checked HTTP endpoints | no | no proxy and no keep-alive, response headers and body reads bounded, redirects followed only when `follow_redirects` is set and only within the origin; results reduced to fixed failure codes (D-080, D-082) |
| Supervised programs and their output | no | the configured environment only, plus a default `PATH` when it sets none; `/dev/null` as stdin, their own process group, a parent-death signal, stopped with the group (D-084). Lines sent to the log or the journal are cut at 4 KiB, limited to 100 per second with bursts of 1000 for each stream (stdout, stderr) of each launch, and masked with the configuration's secrets and every `environment` value except `PATH` (D-086, D-100). `file` output is written as the program wrote it, with no size or rate limit, and redacted only when `supervisor logs` reads it; an output file that is a link, has more than one hard link or could be changed by another user is refused (T-14, T-15) |
| The journal and output files read by `supervisor logs` | no | only configured sources, picked by name; `journalctl` at a fixed path with fixed options, as uid 65534 with group `systemd-journal` when sentineld is root. Program output is read only from entries journald recorded under sentineld's uid with `_TRANSPORT=journal`; a systemd service's logs are read by unit and hold whatever that unit's processes wrote. Lines bounded and redacted before any cut; terminal sequences quoted by the CLI (D-099, D-100, T-21, T-22) |
| systemd's notify socket | with readiness only | fixed `KEY=value` lines; the status text carries module counts, no configuration value |
| systemd (systemd services, D-093) | as root's | reached over systemd's private socket `/run/systemd/private` at a fixed path, which systemd opens to root only, so no other local user can send sentineld anything (the system bus would let any user send it signals that the D-Bus client decodes whole). Every write is bounded (5 s) and every call by the service's `timeout`; after a call is cancelled or times out, the connection is dropped. Single properties are read, never `GetAll` (a unit's `ExecStart` and `Environment` may hold secrets); each value is checked for type, length and vocabulary (`FuzzDecodeState`), and a reply that fails the checks decides nothing. One write, `RestartUnit` in mode `fail` (it never replaces a queued job), only behind `modules.supervisor.systemd_restart`, audited before it is sent, its job read back from the `Job` property (no signal subscribed); when its outcome is unknown, sentineld reads the unit and never sends the restart again (D-095). Residual: systemd's replies are read whole before they are checked (it is trusted) |

## Control socket authorization

ADR-0012, D-076. The daemon reads the caller's uid, gid and groups from
the kernel (`SO_PEERCRED`, `SO_PEERGROUPS`), which nothing in the request
can change, and maps them to a tier: `read` (anyone who can open the
socket), `operate` (`access.operator_group`), `admin`
(`access.admin_group` and root). Each tier includes the ones below it, and
each command declares its tier in `pkg/api`. Members of `admin_group` are
root-equivalent by design, and configuring one warns.

Limits: request 64 KiB, response 16 MiB, 32 connections, and per user 10
connections at once then one per second, root excepted (D-077). An
exchange has a 5 s deadline. Once the request is authorized,
`supervisor logs` gets 15 s and `reload` up to 20 min 5 s (twice the
largest allowed `shutdown_timeout`, plus 5 s); the reload itself is
bounded by twice the running `shutdown_timeout`. A deadline does not undo
an action already started: a supervisor action may go on, and a late
outcome record can lose the answer (B-044), so check `status` and `audit`
before retrying. The request decoder is strict and
fuzzed. Outside Linux there are no peer credentials, and every request is
refused (`unavailable`).

## Audit

ADR-0011, D-077. Every `operate` or `admin` request is recorded before it
runs (`intent`, then `ok` or `failed`), denied ones too; a request that
acts on a service or job carries its validated name as `target` (D-089).
If the record cannot be written, the request is refused (fail closed).
Automatic restarts and job launches need their audit intent the same way
(D-087, D-095, D-097). Two protective actions go ahead when the audit
fails, and the failure is logged: stopping a process over its memory
limit (D-090) and cleaning up what a previous run left (D-087).

Records are SHA-256 chained, synced, rotated (5 × 10 MiB) and checked at
every start against `audit_head.json`. Each record is also logged at
`info`, so under systemd it reaches the journal unless `log.level` is
above `info`.

The audit is evidence. It detects edits, deletions, reordering and
truncation by anyone without root; root can rewrite every local copy
(T-16), so ship the journal off the host when that matters. A broken
chain is reported and never repaired, and privileged requests stay
refused until an operator acts ([operations](operations.md#audit-log)).

## Secrets and redaction

Secrets belong in the environment (a root-only `EnvironmentFile`), never
in YAML (D-006). sentineld never writes them to state or audit, and masks
them in its log (secret-looking attribute keys), in configuration errors,
in `sentinelctl config show` and in module failure reports (a panic is
logged by its frames, never its value). Masking matches known values of
at least 4 bytes, so a shorter secret can show in configuration problems
and logs; `sentinelctl validate` warns about such values (threat model,
invariant 4).

Program output is masked with the secrets the configuration knows before
it reaches the log or the journal, except a literal secret in `args` or
`script`, a secret cut by the 4 KiB line limit, a value shorter than 4
bytes, and `file` output. Pass secrets through `environment` (D-086,
D-100).
`supervisor logs` redacts what it reads, whole values and each line of a
multi-line one, but only what the configuration knows now, so it needs
the `operate` tier and is audited (D-099).

## Supply chain

| Control | Status |
|---|---|
| Runtime dependencies | two (`go.yaml.in/yaml/v3`; `github.com/godbus/dbus/v5`, confined to the systemd adapter by the architecture test, D-094); a new module needs a decision entry and approval |
| CI | GitHub Actions pinned by commit SHA, least-privilege tokens, workflow lint (actionlint, zizmor) |
| Scanning | govulncheck in CI, CodeQL, OpenSSF Scorecard, Dependabot for Go modules and actions |
| Builds | static, `-trimpath`, reproducible from the commit (same commit, same bytes) |
| Snapshot artifacts | every CI run: tarballs, DEB, RPM and APK packages, SHA-256 checksums, CycloneDX SBOM per binary (D-079, D-102); packages checked for contents, modes and scripts (`task package-verify`) |
| Signatures, provenance | release workflow (D-103): `checksums.txt` signed keyless with cosign, build provenance for it and every file it lists, in jobs that run no repository code; packages carry no signature of their own and are verified through the checksums (D-102, [install](install.md#verify-a-release)) |
| Published releases | from v0.3.1 (Phase 3d), each checked by the maintainer as a draft first; the tags before it have no files |

A snapshot checksum shows that a file is the one CI produced, not who
produced it: use a signed release; otherwise build from source or take the
CI artifact of a commit you have reviewed.

## Not yet in place

- Restrictions in the systemd unit (capability bounding,
  `NoNewPrivileges`, `ProtectSystem`): the packaged unit runs sentineld
  as root without them, because the programs it supervises would inherit
  them (D-102, [operations](operations.md#run-under-systemd)); evaluation
  in backlog B-048.
- A process that leaves its process group (`setsid`) escapes the stop of
  its group ([supervisor](supervisor.md#process-services), T-15).
- A size limit, rate limit or rotation for `file` output: a supervised
  program can fill the file system that holds its output file (T-19).
- Logging that cannot block the daemon when its destination stops reading
  (B-004).
- Fuzzing in CI (B-002); the fuzz targets run with `task fuzz`.
- The firewall and its gates (T-04, T-05): Phase 4.
