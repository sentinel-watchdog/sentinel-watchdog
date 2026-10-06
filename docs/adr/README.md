# Architecture Decision Records

Each ADR records one significant decision: its context, the options that
were considered, the choice and its consequences. ADRs are immutable once
`Accepted`; a later ADR supersedes an earlier one instead of rewriting it.
Short refinements live in the decision log of [PLAN.md](../../PLAN.md)
(D-xxx) and point here for detail.

Status values: `Proposed` (direction agreed, details confirmed in the named
phase) · `Accepted` · `Superseded by ADR-NNNN`.

| ADR | Title | Status | Implemented in |
|---|---|---|---|
| [0001](0001-modular-monolith.md) | Modular monolith with in-process providers | Accepted | all phases |
| [0002](0002-event-model-and-bus.md) | Unified event model and in-process event bus | Accepted | 3 |
| [0003](0003-monitoring-vs-enforcement.md) | Monitoring vs enforcement: operating modes and safety gates | Accepted | 11, 12, 13 |
| [0004](0004-provider-and-capability-model.md) | Provider and capability model; read-only by construction | Accepted | 11 |
| [0005](0005-firewall-architecture.md) | Backend-agnostic firewall service | Accepted | 12, 13 |
| [0006](0006-nftables-backend.md) | nftables backend through the `nft` JSON interface | Accepted | 12, 13 |
| [0007](0007-iptables-backend.md) | iptables backend (legacy and nf_tables variants) | Accepted | 21 |
| [0008](0008-blocklists-and-crowdsec.md) | Dynamic blocklists and CrowdSec integration | Accepted | 14, 15 |
| [0009](0009-container-runtimes.md) | Container runtime adapters (Docker, Podman, containerd) | Accepted | 16 |
| [0010](0010-kubernetes-integration.md) | Kubernetes integration and NetworkPolicy | Accepted | 17, 18, 20 |
| [0011](0011-state-audit-transactions.md) | State schema v2, audit log and firewall transactions | Accepted | 3, 8, 13 |
| [0012](0012-control-plane-authorization.md) | Control-plane authorization tiers | Accepted | 5, 8 |
| [0013](0013-dependency-policy.md) | Dependency policy and evaluated libraries | Accepted | all phases |
| [0014](0014-security-providers-waf.md) | Security provider abstraction (WAF, reverse proxy) | Proposed | 19 |

## Template

```markdown
# ADR-NNNN: Title

- Status: Proposed | Accepted | Superseded by ADR-NNNN
- Date: YYYY-MM-DD
- Phases: …

## Context
## Options considered
## Decision
## Consequences
## Follow-ups
```
