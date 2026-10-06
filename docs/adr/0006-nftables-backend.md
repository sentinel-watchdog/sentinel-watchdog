# ADR-0006: nftables backend through the `nft` JSON interface

- Status: Accepted
- Date: 2026-10-06
- Phases: 12 (probe, observe, render, check), 13 (commit, restore)

## Context

nftables is the primary backend. The project builds with `CGO_ENABLED=0`
(D-023), so `libnftables` cannot be linked. Human-readable `nft list`
output must not be parsed when a structured format exists.

## Options considered

| Option | Pros | Cons |
|---|---|---|
| **`nft` binary with JSON (`-j`)** for reads and writes | Same interface firewalld uses through libnftables; structured in both directions; `--check` gives a kernel-validated dry run; payload is human-inspectable; no Go dependency | Needs the `nft` binary built with JSON support; JSON schema coverage differs across nftables versions |
| `nft` binary, JSON reads + native script writes (`nft -f -`) | Native syntax is the most complete and readable | Text generation needs strict escaping; two formats to maintain |
| `github.com/google/nftables` (netlink, pure Go, Apache-2.0) | No binary needed, no process spawn | Low-level expression building (we would re-implement nft's compiler), weak support for comments/set flags across kernels, plans are not human-reviewable, larger attack surface in our code |

## Decision

Use the **`nft` binary with JSON for both reading and writing**:

| Purpose | Invocation |
|---|---|
| presence/version | absolute path from a fixed list (`/usr/sbin/nft`, `/sbin/nft`, `/usr/bin/nft`), overridable by `firewall.nft_path`; no `$PATH` lookup (D-005). `nft --version` only for display. |
| JSON support | `nft -j list tables` must return valid JSON; otherwise capability `nftables.json: unsupported` and the backend is not used (no text-parsing fallback). |
| permission | `CAP_NET_ADMIN` bit from `CapEff` in `/proc/self/status` before any kernel call ⇒ `permission_denied` without parsing error text. |
| IPv6 | `/proc/net/if_inet6` exists and `net.ipv6.conf.all.disable_ipv6 = 0`; otherwise `ipv6: unavailable` and IPv6 rules are refused. |
| observe | `nft -j list table inet <table>` (managed table only); `nft -j list tables` for conflict detection (names only). |
| feature probes | `nft --check -j -f -` with a payload that would create a temporary set with `timeout`/`interval` flags inside the Sentinel table; nothing is committed. |
| check | `nft --check -j -f -` with the rendered payload. |
| commit | `nft -j -f -` with the rendered payload: one invocation = one kernel transaction (all-or-nothing). |

The payload goes through stdin. Arguments are never built from user data.

### Ownership

- Table `inet sentinel` by default (`firewall.family: inet`,
  `firewall.table: sentinel`; only `inet` is accepted in Phases 12–13).
- Base chains `input`, `forward`, `output` (only those used by enabled
  policies), `type filter`, policy `accept`, priority
  `firewall.chain_priority` (default `-10`, i.e. before `filter`).
- One regular chain per policy and direction (`p_<policy>_<direction>`),
  jumped from the base chain in priority order; named sets for blocklists
  and TTL rules (`bl_<name>_v4`, `bl_<name>_v6`, `ttl_v4`, `ttl_v6`).
- Ownership marker: table comment `managed-by=sentinel;id=<install-id>`
  where supported, otherwise a marker set `sentinel_owner` holding the
  install ID. A pre-existing table with the configured name **without**
  the marker is a conflict: Sentinel refuses to touch it (no adoption in
  Phases 12–13).
- Rule identity: each rule carries `comment "sentinel:<rule-id>"`;
  the mapping rule ID ⇄ handle is rebuilt from `Observe` and recorded in
  the transaction metadata. Rule IDs fit nftables' comment length limit
  by construction (D-011).

### Transaction shape

Commit = one JSON command batch that:

1. `add` table/chains/sets (idempotent creation);
2. `flush chain` **only for Sentinel's own regular and base chains** that
   change, then `add rule` for the complete desired content of those
   chains (deterministic order);
3. updates set elements by diff (`add element` / `delete element`), so
   dynamic blocklist entries and their timeouts survive policy changes;
4. `delete chain`/`delete set` for Sentinel-owned objects no longer
   desired.

The renderer validates every command object: family and table must equal
the owned table; commands `flush ruleset`, `delete table` for any other
table, and any object outside the owned table are impossible to emit
(unit-tested invariant). Removing the whole Sentinel table is a separate,
explicit operation (`sentinelctl firewall rollback` to the empty state or
uninstall).

### Verification and rollback

- After commit: `Observe` again and compare the normalised observed model
  with the expected one (rules, order, set definitions, element counts).
  Mismatch ⇒ `firewall_apply_failed` + `Restore` when
  `rollback_on_error`.
- Snapshot before commit: the JSON of `nft -j list table inet <table>`
  plus the previous desired model. `Restore` re-applies the previous
  desired model through the same renderer (deterministic); the raw JSON is
  kept as forensic backup and for verification. Rollback of the first
  transaction = delete the Sentinel table.

### Versions to verify (Phase 12 integration tests)

Distribution nftables versions differ (RHEL 8 minors, Ubuntu 22.04,
Alpine edge/stable). Phase 12 must verify on each target, in containers with
`CAP_NET_ADMIN` and a private network namespace: JSON input acceptance for
comments, set flags (`interval`, `timeout`), `flush chain` in JSON, and
`--check`. Gaps become capabilities reported as `unsupported` (never
silent fallbacks). If JSON input proves unusable on a target, a follow-up
ADR may switch writes to generated native scripts with a strict escaper.

## Consequences

- Runtime requirement: `nft` with JSON support (RHEL/Ubuntu/Alpine
  packages are expected to have it; verified in Phase 12).
- Unit tests use `executor.FakeRunner` with recorded JSON fixtures per
  distribution; no root in standard CI.
- Rule counters reset when a chain is rewritten.
