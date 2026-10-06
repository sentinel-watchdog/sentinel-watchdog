# ADR-0008: Dynamic blocklists and CrowdSec integration

- Status: Accepted
- Date: 2026-10-06
- Phases: 14 (local/file blocklists, CrowdSec read-only), 15 (CrowdSec → firewall sync, safety policies)

## Context

Blocklists change often (minutes); running a full plan / confirm cycle for
every refresh is impractical, but entries must still pass the same safety
checks. CrowdSec already detects attacks and distributes decisions;
Sentinel must not duplicate its detection engine or replace its
remediation components.

Principle: **CrowdSec detects; Sentinel observes and orchestrates; the
firewall (or a CrowdSec remediation component) applies the block.**

## Decision

### Blocklists (`internal/blocklist`)

A blocklist is a *source of entries* bound to one Sentinel-owned set.

```
source (local | file | crowdsec | https* | threatintel*)
  → parse → validate (netspec) → normalise / aggregate CIDRs
  → subtract allowlists (global firewall allowlist + protected_access + blocklist allowlist)
  → cap (max_entries) → diff against current set elements
  → firewall SetUpdater (element-level transaction) → verify → events + audit
(* = later phases, explicit configuration required)
```

- **Two-step authorisation.** The static rule referencing the set
  (`drop … ip saddr @bl_<name>_v4`) is part of the firewall policy and goes
  through the normal plan/apply/confirm flow once. Afterwards element
  updates are automatic **inside that pre-authorised envelope**: only
  `add element` / `delete element` on that set, only in effective mode
  `enforce`, always after allowlist subtraction. The consumer-defined
  interface is:

  ```go
  type SetUpdater interface {
      UpdateSet(ctx context.Context, set string, add, remove []netip.Prefix, ttl map[netip.Prefix]time.Duration) (UpdateResult, error)
  }
  ```

- **Fail static.** A source error keeps the last good content and emits
  `blocklist_update_failed`. An empty result is treated as an error unless
  `allow_empty: true`. A change larger than `max_change_ratio` (default
  50 % of current entries) pauses updates and emits a `critical` event
  (circuit breaker against poisoned or truncated sources).
- **TTL**: per entry (CrowdSec decision duration) or per list; rendered as
  set element timeouts (ADR-0005).
- **Remote sources** (`https`) are never fetched without explicit
  configuration, require HTTPS, a size limit and either a checksum or a
  signature (`sha256` file or minisign-style signature, decided in its
  phase). Not part of Phase 14.
- **Audit**: one record per update with counts, the SHA-256 of the sorted
  added/removed lists and the first N entries; full lists for small
  deltas (≤ 100 entries).

### CrowdSec (`internal/security/crowdsec`)

- **Client**: stdlib `net/http` against the Local API (LAPI).
  - Decisions: bouncer API key (`X-Api-Key`) — `GET /v1/decisions/stream`
    (incremental, `startup=true` on first poll) or `GET /v1/decisions`.
  - Alerts: require *machine* (watcher) credentials and a JWT from
    `POST /v1/watchers/login`. Machine credentials can also *create*
    alerts/decisions, so alerts are **off by default** and need separate,
    explicitly configured credentials; documented as a privilege increase.
- **Detection**: LAPI reachable and authenticated ⇒ `ok`; connection
  refused ⇒ `unavailable`; 401/403 ⇒ `permission_denied`. Local hints
  (`/etc/crowdsec/` present, `cscli` binary) are reported read-only.
  Sentinel never runs `cscli` write commands and never registers itself as
  a bouncer: the operator runs `cscli bouncers add sentinel` and provides
  the key through the environment.
- **Filtering and provenance**: only `type: ban` with scope `Ip` or
  `Range` is imported; other scopes/types are counted and reported as
  `unsupported`. `origins` allowlist (default `crowdsec`, `cscli`;
  `CAPI` and `lists:*` opt-in) — decisions from other origins are ignored
  with a counter. Sentinel allowlist CIDRs are never imported.
- **Events**: `security_decision_added` / `security_decision_removed`
  (dedup key = decision value + origin), correlation ID
  `crowdsec:<decision id>`, attributes `value`, `scope`, `origin`,
  `scenario`, `duration`, `expires_at`.
- **Sync (Phase 15)**: CrowdSec becomes a blocklist *source* (`source:
  crowdsec`) feeding a dedicated set. No separate firewall path exists.
  Additional safety: rate limit of new decisions per minute (circuit
  breaker), maximum active decisions, IPv4/IPv6 handled separately, every
  sync audited with provenance.
- **Coexistence**: if the CrowdSec firewall bouncer's tables/chains are
  detected, status warns that remediation is duplicated; Sentinel never
  modifies them.
- **Transport security**: loopback HTTP allowed; non-loopback `http://`
  produces a validation warning; `https` uses the shared `tls` block.

### Configuration shape (naming accepted in D-048; added in Phases 14–15)

```yaml
blocklists:
  - name: local-abuse
    enabled: true
    source: local            # local | file (Phase 14); crowdsec (Phase 15); https (later)
    action: drop             # drop | reject
    family: inet
    refresh_interval: 15m
    max_entries: 100000
    entries:
      - 203.0.113.10
      - 2001:db8::10

security:
  crowdsec:
    enabled: false
    mode: read_only          # read_only only until Phase 15
    lapi_url: http://127.0.0.1:8080
    api_key: ${CROWDSEC_LAPI_KEY}
    poll_interval: 30s
    origins: [crowdsec, cscli]
    allowlist: []
    sync_decisions: false    # Phase 15: feeds blocklist `crowdsec` (replaces `firewall_provider`)
```

The specification's `firewall_provider` key is dropped: a host has exactly
one active firewall backend (ADR-0005), so naming it again would be an
alias that can disagree.

## Consequences

- One path from any source to the firewall, so allowlists, protected
  access, caps, audit and TTL apply uniformly to local lists, CrowdSec and
  future threat-intelligence feeds.
- A compromised source can at worst block addresses outside the
  allowlists and within the caps and circuit breaker (threat model T-09).
