# Architecture Decision Records

Each ADR records one significant decision: its context, the options that
were considered, the choice and its consequences. ADRs are immutable once
`Accepted`; a later ADR supersedes an earlier one instead of rewriting it.
Short refinements live in the [decision log](../decisions.md) (D-xxx)
and point here for detail. Phase numbers inside ADR-0001…0014 refer to
the roadmap before D-065; see the [phase mapping](../../PLAN.md#phase-mapping).

Status values: `Proposed` (direction agreed, details confirmed in the named
phase) · `Accepted` · `Accepted, amended by ADR-NNNN` (still valid; the
named ADR changes part of it) · `Superseded by ADR-NNNN`.

| ADR | Title | Status | Implemented in |
|---|---|---|---|
| [0001](0001-modular-monolith.md) | Modular monolith with in-process providers | Accepted, amended by 0015 | all phases |
| [0002](0002-event-model-and-bus.md) | Unified event model and in-process event bus | Accepted, amended by 0015 | 2b |
| [0003](0003-observation-vs-enforcement.md) | Observation vs enforcement: operating modes and safety gates | Accepted, amended by 0015 | 4a, 4b |
| [0004](0004-provider-and-capability-model.md) | Provider and capability model; read-only by construction | Accepted | 4a |
| [0005](0005-firewall-architecture.md) | Backend-agnostic firewall service | Accepted | 4a, 4b |
| [0006](0006-nftables-backend.md) | nftables backend through the `nft` JSON interface | Accepted | 4a, 4b |
| [0007](0007-iptables-backend.md) | iptables backend (legacy and nf_tables variants) | Accepted | 4f |
| [0008](0008-blocklists-and-crowdsec.md) | Dynamic blocklists and CrowdSec integration | Accepted | 4c |
| [0009](0009-container-runtimes.md) | Container runtime adapters (Docker, Podman, containerd) | Accepted | 4d |
| [0010](0010-kubernetes-integration.md) | Kubernetes integration and NetworkPolicy | Accepted | 4d, 4e |
| [0011](0011-state-audit-transactions.md) | State schema v2, audit log and firewall transactions | Accepted, amended by 0015 | 2b, 2c, 4b |
| [0012](0012-control-plane-authorization.md) | Control-plane authorization tiers | Accepted | 2c |
| [0013](0013-dependency-policy.md) | Dependency policy and evaluated libraries | Accepted | all phases |
| [0014](0014-security-providers-waf.md) | Security provider abstraction (WAF, reverse proxy) | Proposed | 4e |
| [0015](0015-modules-and-configuration-layout.md) | Core, platform and modules; configuration layout | Accepted | 2a, 2b, 2c and every module |

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
