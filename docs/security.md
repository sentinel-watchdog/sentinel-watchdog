# Security

What protects a host running Sentinel Watchdog **today** (the core,
Phase 2c), what does not yet, and where each guarantee is specified. The
design-level analysis is the [threat model](threat-model.md); this page
is its operator-facing summary for the code as built. To report a
vulnerability, see [SECURITY.md](../SECURITY.md) — never a public issue.

> First version (Phase 2c-4). Mitigations that later phases add (module
> sandboxing, the hardened unit, signed releases) are listed as such, not
> as present.

## Privilege model

- `sentineld` runs as **root**: later modules restart services and change
  the firewall. Every bug is potentially a root bug (T-01), which is why
  dependencies are minimal (two: the YAML library, D-001, and the D-Bus
  client of the systemd adapter, D-094; ADR-0013),
  nothing runs through a shell, and planned features fail loudly instead
  of half-working.
- `sentinelctl` needs no privilege. It reads configuration files itself
  only for `validate`; everything else goes through the control socket,
  where the daemon decides.
- Today the core starts no process, opens no network listener and
  changes nothing on the system. Its effects are: the state directory,
  the control socket, outgoing webhook requests, and sd_notify datagrams
  to systemd.

## Trust boundaries

| Input | Trusted? | Protection |
|---|---|---|
| Configuration files (`/etc/sentinel`) | as much as their owner | owned by root (or the daemon user), not writable by group/others on the whole path, opened once through `os.Root`, size-limited, strict parser (unknown and duplicate keys rejected), every problem reported ([configuration](configuration.md#file-ownership-and-modes), D-070, T-02, T-13) |
| Environment (`${NAME}` values, `NOTIFY_SOCKET`) | no | names validated, values never quoted in errors (`[REDACTED]`); `NOTIFY_SOCKET` validated, never logged, removed from the environment after it is read (D-079) |
| Control socket peers | no | identity from the kernel, tiers, limits, audit (below; T-03) |
| State and audit files from a previous run | no | `os.Root`, `O_NOFOLLOW`, regular files only, mode `0600`, ownership checked after open, strict parsers with fuzz targets (T-07, T-16) |
| Webhook receivers | with event content only | TLS verified unless explicitly disabled (warns), secrets only from the environment, redirects off by default ([notifications](notifications.md#security), T-06) |
| systemd's notify socket | with readiness only | fixed `KEY=value` lines; the status text carries module counts, no configuration value |
| systemd (systemd services, D-093) | as root's | reached over systemd's private socket `/run/systemd/private` at a fixed path, which systemd opens to root only, so no other local user can send sentineld anything (the system bus would let any user send it signals that the D-Bus client decodes whole); every write bounded (5 s) and every call by the service's `timeout`, a connection dropped after a call its context ended; single properties read, never `GetAll` (a unit's `ExecStart` and `Environment` may hold secrets); each value type-, length- and vocabulary-checked (`FuzzDecodeState`), and a reply that fails the checks decides nothing. Residual: systemd's replies are read whole before they are checked (it is trusted) |

## Control socket authorization

ADR-0012, D-076. The daemon reads the caller's uid, gid and groups from
the kernel (`SO_PEERCRED`, `SO_PEERGROUPS`) — nothing in the request can
raise them — and maps them to a tier: `read` (anyone who can open the
socket), `operate` (`access.operator_group`, root), `admin`
(`access.admin_group`, root). Each command declares its tier in
`pkg/api`. Members of `admin_group` are root-equivalent by design and
configuring one warns.

Limits: request 64 KiB, response 16 MiB, 5 s per exchange (reload
longer), 32 connections, a per-user rate for connections with a slot
kept for root. The request decoder is strict and fuzzed. Outside Linux
there are no peer credentials, and every request is refused
(`unavailable`).

## Audit

ADR-0011, D-077. Every `operate`/`admin` request is recorded **before**
it runs (`intent`, then `ok` or `failed`), and denied ones too; if the
record cannot be written the request is refused (fail closed). Records
are SHA-256 chained, synced, rotated (5 × 10 MiB), copied to the journal,
and checked at every start against `audit_head.json`.

What it gives: **evidence**, not prevention. It detects edits, deletions,
reordering and truncation by anyone without root; root can rewrite every
local copy (T-16), so ship the journal off the host when that matters. A
broken chain is reported and never repaired, and privileged requests stay
refused until an operator acts ([operations](operations.md#audit-log)).

## Secrets and redaction

Secrets belong in the environment (a root-only `EnvironmentFile`), never
in YAML (D-006). They are masked in logs (secret-looking attribute keys),
in configuration errors, in `sentinelctl config show`, in module failure
reports (a panic is logged by its frames, never its value) and never
written to state or audit (invariant 4).

## Supply chain

| Control | Status |
|---|---|
| Runtime dependencies | two (`go.yaml.in/yaml/v3`; `github.com/godbus/dbus/v5`, confined to the systemd adapter by archtest, D-094); a new module needs a decision entry and approval |
| CI | GitHub Actions pinned by commit SHA, least-privilege tokens, workflow lint (actionlint, zizmor) |
| Scanning | govulncheck in CI, CodeQL, OpenSSF Scorecard, Dependabot for Go modules and actions |
| Builds | static, `-trimpath`, reproducible from the commit (same commit, same bytes) |
| Snapshot artifacts | every CI run: tarballs, SHA-256 checksums, CycloneDX SBOM per binary (D-079) |
| Signatures, provenance, published releases | **not yet** — Phase 3d (D-052) |

A snapshot checksum shows that a file is the one CI produced, not who
produced it; until releases are signed, build from source or from a CI
artifact of a commit you have reviewed.

## Not yet in place

- Hardened systemd unit (capability bounding, `NoNewPrivileges`,
  `ProtectSystem`): Phase 3d. The unit in [operations](operations.md) is
  an example.
- Logging that cannot block the daemon when its destination stops
  reading (backlog B-004).
- Controls for supervised processes and the firewall (T-04, T-05, T-15):
  with those modules, Phases 3b and 4.
