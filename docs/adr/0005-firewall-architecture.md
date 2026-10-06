# ADR-0005: Backend-agnostic firewall service

- Status: Accepted
- Date: 2026-10-06
- Phases: 12 (model, planner, read-only), 13 (transactions, enforce), 21 (iptables)

## Context

Sentinel must manage host firewall rules through nftables (primary) and
iptables (legacy/fallback) with identical safety guarantees: plan,
dry-run, read-only, transaction IDs, backup, rollback, safety timeout,
audit, duplicate and conflict detection, protected administrative access,
post-apply verification and no global flush.

If each backend implemented these guarantees itself, they would drift
apart and be tested twice. The guarantees therefore live in one place and
backends stay small.

## Decision

### Package layout

```
internal/netspec/              CIDR/IP/port/protocol/family parsing and normalisation (shared with blocklist, crowdsec, kubernetes)
internal/firewall/             Service facade (implements the spec's FirewallProvider), backend selection and pinning
internal/firewall/model/       Policy, Rule, Set, Observed, Plan, Change, ApplyResult, Status — backend-agnostic
internal/firewall/planner/     validation, normalisation, duplicate/conflict/shadow detection, protected-access guard, diff, fingerprint
internal/firewall/transaction/ tx IDs, lock, backup, check, commit, verify, confirm timer, rollback, persistence, audit hooks
internal/firewall/nftables/    backend (ADR-0006)
internal/firewall/iptables/    backend (ADR-0007)
```

### Interfaces

The facade keeps the shape requested by the specification so the daemon
and CLI have one entry point:

```go
type FirewallProvider interface {
    Name() string
    Capabilities(ctx context.Context) (Capabilities, error)
    Plan(ctx context.Context, desired Policy) (Plan, error)
    Apply(ctx context.Context, plan Plan) (ApplyResult, error)
    Rollback(ctx context.Context, transactionID string) error
    Status(ctx context.Context) (FirewallStatus, error)
    ListManagedRules(ctx context.Context) ([]Rule, error)
}
```

Backends implement two narrow interfaces consumed by the service:

```go
// Observer: everything a read_only service needs.
type Observer interface {
    Name() string
    Probe(ctx context.Context) (Capabilities, error)       // presence, permission, features, conflicts
    Observe(ctx context.Context) (model.Observed, error)    // managed objects only, normalised + raw snapshot
    Render(p model.Plan) (Payload, error)                  // deterministic, owned objects only
    Check(ctx context.Context, p Payload) error            // nft --check / iptables-restore --test
}

// Committer: only constructed in effective mode dry_run/enforce.
type Committer interface {
    Commit(ctx context.Context, p Payload) error            // exactly one atomic backend transaction
    Restore(ctx context.Context, s model.Snapshot) error    // atomic restore of managed objects
}
```

`Plan(ctx, desired)` = `Observe` → `planner.Diff(observed, desired)` →
`Render` → `Check` (when allowed). The `Plan` value carries the abstract
changes (add / remove / replace by rule ID), the rendered payload for
display, the observed-state fingerprint and warnings.

`Apply(ctx, plan)`: lock → re-observe and compare fingerprint (stale plan
⇒ refuse) → audit intent → backup snapshot → `Commit` → re-observe and
verify against the expected model → `pending_confirmation` (if
`safety_timeout > 0`) or `committed` → audit result → events. Any failure
after commit with `rollback_on_error: true` ⇒ `Restore(backup)`.

### Rule model

```go
type Policy struct {
    Name          string
    Enabled       bool
    Priority      int     // ascending; ties broken by name
    DefaultAction string  // accept | drop   (YAML: default_action; D-048)
    Rules         []Rule
}

type Rule struct {
    ID          string        // name format (D-011), unique across all policies
    Family      string        // inet | ip | ip6 — inferred from CIDRs when omitted; mismatch is an error
    Direction   string        // input | output | forward
    Action      string        // accept | drop | reject
    Protocol    string        // tcp | udp | icmp | icmpv6 | any (default any)
    SourceCIDRs []string      // YAML: source_cidrs; bare IPs normalised to /32 or /128; host bits set ⇒ error
    DestCIDRs   []string      // YAML: dest_cidrs
    SourcePorts []string      // YAML: source_ports; "22", "8000-8100"; only with tcp/udp
    DestPorts   []string      // YAML: dest_ports
    Interface   string        // YAML: interface; input/forward = iif name, output = oif name
    Comment     string        // printable ASCII, ≤ 64 bytes, no quotes/backslashes
    TTL         time.Duration // see "TTL" below
}
```

Field names are the plural, snake_case forms everywhere (YAML, JSON API,
docs). `family` means the **address family scope** with nftables
vocabulary everywhere it appears (`firewall.family`, `rules[].family`,
`blocklists[].family`): `inet` = IPv4 and IPv6.

### Evaluation semantics (documented for operators)

- Policies are evaluated in ascending `priority`; within a policy, rules
  in list order; **first match wins**.
- A policy with `default_action: drop` drops everything it did not accept;
  later policies never see that traffic. `accept` (default) continues with
  the next policy.
- **Sentinel's `accept` is not global.** In both nftables (multiple
  tables on the same hook) and iptables (other chains after the jump), an
  accept in Sentinel's chain only ends evaluation *inside Sentinel*. Other
  tables, firewalld, Docker or the CrowdSec bouncer may still drop the
  packet. A Sentinel `drop` is final. Sentinel therefore never claims that
  an accept rule *guarantees* access.

### Protected access (anti-lockout)

Decided in D-049.

- `firewall.protected_access` (list of `{protocol, dest_ports, source_cidrs}`),
  default `[{protocol: tcp, dest_ports: ["22"]}]` (any source). An
  explicit list **replaces** the default; an explicit empty list is
  accepted with a validation warning.
- `sentinelctl` sends the caller's `SSH_CONNECTION` (client address and
  server port) as a hint; the daemon adds "tcp from that client to that
  port" to the protected set for that plan only, which also covers sshd on
  a non-standard port. The hint can only *add* protection, never remove
  it. Daemon-initiated changes (recovery actions, blocklist updates) have
  no hint and use the configured list only.
- The planner computes whether any `drop`/`reject` rule or `default_action:
  drop` in `input` could match protected traffic. If so the plan fails
  unless the offending rule sets `allow_protected_access_block: true`,
  and even then the transaction is forced to use `safety_timeout`
  (cannot be `0s`). Risky opt-ins always use the `allow_` prefix.

### Validation (planner)

- duplicate rule IDs or duplicate policy names ⇒ error;
- two rules with the same normalised match and action ⇒ error (duplicate);
- same match with different actions, or a rule fully shadowed by an
  earlier one ⇒ warning;
- ports without tcp/udp, ICMP with ports, CIDR family mismatch,
  `interface` on an impossible direction ⇒ error;
- rule count and set size limits (defaults 1 000 rules, 100 000 set
  elements) ⇒ error.

### TTL

nftables has no per-rule timeout, but set elements do. A rule with `ttl`
must therefore have the "blocklist shape": `direction: input|forward`,
`action: drop|reject`, only `source_cidrs` (optionally `protocol` +
`dest_ports`). It is rendered as timed elements in a Sentinel-owned set
referenced by one static rule. Other shapes with `ttl` are rejected as
`unsupported` with an explanation. The absolute expiry is stored in the
transaction metadata so status can show the remaining time and an iptables
backend (ipset timeout) can reproduce it.

### Backend selection and pinning

- `backend: auto` ⇒ nftables if `Probe` reports `supported`; otherwise
  iptables; otherwise the firewall domain is `unavailable`.
- `backend: nftables` / `iptables` ⇒ that backend or a hard error; no
  fallback.
- The backend used by the first committed transaction is **pinned** in
  state. If detection later selects a different backend, enforcement is
  refused (`backend_changed`) until the operator runs
  `sentinelctl firewall rollback` on the old backend or resets the pin;
  read-only status keeps working. One policy is never applied through two
  backends.

### Conflict detection (informational)

`Probe` reports other managers it can see without modifying them:
firewalld (`inet firewalld` table), ufw (`ufw-*` chains), Docker
(`DOCKER*` chains, `docker0`), Podman/netavark (`netavark` table or
chains), CrowdSec firewall bouncer (`crowdsec`/`crowdsec6` tables or
`CROWDSEC_CHAIN`), kube-proxy (`KUBE-*`, `kube-proxy` table), mixed
iptables-legacy + nftables rules. Conflicts are shown in status and as
`firewall_conflict_detected` events; they never block read-only use.

## Consequences

- Backends are thin (~probe, observe, render, check, commit, restore);
  all safety logic is shared and tested once against a fake backend.
- `ListManagedRules` and drift detection come for free from `Observe` +
  `planner.Diff`.
- Per-rule counters are reset on commit (chains are rewritten atomically,
  see ADR-0006); documented.
