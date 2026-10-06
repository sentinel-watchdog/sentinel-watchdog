# ADR-0003: Observation vs enforcement — operating modes and safety gates

- Status: Accepted, amended by ADR-0015
- Date: 2026-10-06
- Phases: 11 (model, config), 12 (read_only, dry_run), 13 (enforce)

## Context

Supervision changes the state of services Sentinel was told to watch
(restart). The new domains can change **network reachability of the whole
host or cluster**: a wrong firewall rule can lock administrators out; a
wrong NetworkPolicy can isolate workloads. The specification requires
`read_only`, `dry_run`, `plan`, `confirm_required`, `managed_only`,
transaction IDs, audit, rollback, backups, safety timeout and post-apply
verification, and that read-only is *really* read-only.

## Decision

### Three effective modes, two configuration keys

Every integration that can change external state (firewall, Kubernetes
NetworkPolicy, later container actions) has:

```yaml
mode: read_only     # read_only | enforce
dry_run: true       # only meaningful with mode: enforce
```

| `mode` | `dry_run` | Effective mode | Allowed |
|---|---|---|---|
| `read_only` | any | **read_only** | status, capabilities, list, validate, plan, diff. External calls limited to reads and non-mutating checks (ADR-0004). |
| `enforce` | `true` | **dry_run** | everything above + full apply pipeline up to the backend's check step (`nft -c`, `iptables-restore --test`, Kubernetes `dryRun=All`). Nothing is committed. Audited as `dry_run`. |
| `enforce` | `false` | **enforce** | commit, confirm, rollback. |

Arming enforcement needs **two explicit edits** (`mode: enforce` and
`dry_run: false`); both defaults are safe. The effective mode is shown by
every status command and included in every event of that domain.

`enabled: false` (default for all new domains) means the provider is not
constructed: no binary is executed and no socket is opened.

### Safety gates (firewall; Kubernetes reuses the same pattern)

| Gate | Default | Meaning |
|---|---|---|
| `managed_only` | `true` | Sentinel touches only objects it owns (dedicated table/chains/sets, or Kubernetes objects labelled as managed). `false` is rejected as not implemented until a concrete use case is designed (Q-006). |
| plan before apply | always | Apply takes a plan ID and fingerprint; a plan computed against a different observed state is refused as stale. |
| `confirm_required` | `true` | `sentinelctl … apply` shows the plan and the affected resources and asks for confirmation (or `--yes` when no TTY). |
| `safety_timeout` | `60s` | After commit the transaction is `pending_confirmation`; without `sentinelctl firewall confirm <tx>` before the deadline it is rolled back automatically. `0s` disables it. |
| `rollback_on_error` | `true` | A failed commit or a failed post-apply verification restores the pre-apply snapshot. |
| protected access | on | Plans that could drop/reject traffic to protected management access fail (ADR-0005). |
| backup | always | A snapshot of the managed objects is stored before every commit. |
| audit | always | Intent and result are written to the audit log; failure to write the intent aborts the operation. |
| no global flush | always | No code path can produce `flush ruleset`, delete foreign tables or flush foreign chains (enforced by construction and by tests). |

A refused operation fails with a message naming the gate and the exact
configuration change or flag that would allow it, e.g.
`firewall is in read_only mode; set firewall.mode: enforce and firewall.dry_run: false to apply`.

### Recovery actions that change external state

The recovery engine (Phase 4) gains a generic action registry. Actions
`block`, `unblock`, `apply_firewall_policy`, `rollback_firewall_policy`
exist only when the firewall domain is constructed in effective mode
`enforce`; otherwise a configuration that references them is a
validation error (not a runtime surprise). They go through the same plan,
audit and protected-access gates as CLI operations; the actor in the
audit log is `recovery:<supervisor>`.

## Consequences

- One mental model for every enforcing domain; one set of tests per gate.
- Operators can run Sentinel for months in `read_only`/`dry_run` to see
  plans and drift before arming enforcement.
- `safety_timeout` relies on the daemon being alive; persistence of the
  pending transaction plus rollback at startup covers daemon restarts
  (ADR-0011). Sentinel will **not** create a systemd timer as a dead-man
  switch, because creating units is forbidden by a binding decision.
