# Product research — implications for the agent

Status: agent scope summary, 2026-10-07 (D-066, D-068).

The full contextual vulnerability triage assessment and proposed commercial
experiment belong to product coordination, versioned in the maintainer's
private coordination repository (D-073); they are not part of this agent
checkout or its release documentation. No commercial experiment
has been completed and no implementation or roadmap replacement is approved.

## Binding agent implications

- Open source and Go learning remain the primary objective. Parallel product
  validation does not reorder Supervisor → Firewall → Remote or change v1.0.0.
- Monitor is a candidate. Inventory, applicability and exposure must retain
  evidence, provenance, freshness and explicit unknown states if selected.
- Do not add speculative inventory/triage interfaces or dependencies in
  Phase 2a. An imported-report experiment may be a separate tool or service.
- Fleet storage, multi-tenancy, billing and dashboard implementation are
  outside this repository. Any selected local collection capability needs
  an agent ADR, privilege review, acceptance criteria and delivery phase.
- Future remote sync is outbound-only; configuration delivery must preserve
  local module switches, safety gates and command permissions (R-021).

Q-017 tracks whether reviewed product evidence warrants an agent roadmap
change. R-023 tracks the correctness risks of any selected agent triage
capability. Both remain open. Changes require a decision and PR updating
PLAN, architecture, threat model and release criteria before implementation.

Agent-specific selection criteria: [future modules](future-modules.md).
Repository ownership: [project boundaries](project-layout.md).
