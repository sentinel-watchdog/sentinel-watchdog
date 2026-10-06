# ADR-0007: iptables backend (legacy and nf_tables variants)

- Status: Accepted
- Date: 2026-10-06
- Phases: 21

## Context

Some hosts still rely on iptables (older tooling, Docker's default
integration, ufw). Two implementations ship under the same command name:
`iptables-legacy` (xtables kernel API) and `iptables-nft` (the iptables
syntax on top of nf_tables). iptables has no JSON interface;
`iptables-save` output is the stable, documented machine format and the
input format of `iptables-restore`.

## Decision

- **Role**: legacy backend, selected only when `backend: iptables` or when
  `backend: auto` finds nftables unusable (ADR-0005). Never used together
  with nftables for the same policy.
- **Detection**: resolve `iptables`, `iptables-legacy`, `iptables-nft`,
  `ip6tables*`, `*-save`, `*-restore` from fixed absolute paths; the
  variant is read from `iptables -V` (`(legacy)` / `(nf_tables)` suffix —
  a version banner, not rule output). Report both variants if both have
  rules (`iptables_mixed_backends` conflict).
- **Ownership**: chains `SENTINEL-INPUT`, `SENTINEL-FORWARD`,
  `SENTINEL-OUTPUT` (+ one per policy, `SNTL-<policy>-<dir>`, ≤ 28 chars)
  in table `filter`. One jump rule per used built-in chain, inserted at
  position 1 with comment `sentinel:jump`. The jump rule is the **only**
  change to a chain Sentinel does not own; it is documented and removed on
  rollback-to-empty.
- **Reads**: `iptables-save -t filter` / `ip6tables-save -t filter`. A
  strict parser accepts only the lines of Sentinel-owned chains and the
  jump rules; everything else is counted for conflict detection, never
  interpreted.
- **Writes**: `iptables-restore --noflush --wait` with a payload that
  declares (`:CHAIN - [0:0]`, which also flushes) only Sentinel chains,
  re-adds their rules and ensures the jump rule. One `COMMIT` per table ⇒
  atomic per table and family. IPv4 and IPv6 are two transactions; a
  failure of the second triggers restore of the first (documented
  limitation: not a single atomic step).
- **Check**: `iptables-restore --test`.
- **Sets / TTL**: via `ipset` when present (`hash:net` with `timeout`);
  without `ipset`, blocklists are rendered as rules with a lower size limit
  (default 5 000) and TTL is enforced by Sentinel's scheduler. Capability
  `iptables.ipset` reports which path is in use.
- **Snapshots**: the owned-chain subset of `iptables-save`, stored as text
  with its SHA-256 in transaction metadata.

## Limitations compared with nftables (to document in docs/iptables.md)

- No single transaction across IPv4 and IPv6, nor across `iptables` and
  `ipset`.
- No native set timeouts without `ipset`; slower rule-based blocklists.
- Rule identity via `-m comment` (requires the comment match).
- Coexistence with Docker: Docker owns `DOCKER*` chains and expects users
  to filter forwarded container traffic in `DOCKER-USER`; Sentinel does
  not write there unless a future, explicit option allows it (Phase 16+).
- `--wait` lock contention with other tools (Docker, kube-proxy) is
  reported as `degraded`, never retried indefinitely.

## Consequences

- One strict parser for a narrow subset of the `iptables-save` format,
  tested with fixtures from legacy and nf_tables variants.
- Shared planner/transaction code (ADR-0005) gives the same plan, audit
  and rollback guarantees as nftables, with the documented atomicity gaps.
