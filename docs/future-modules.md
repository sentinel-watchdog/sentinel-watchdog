# Future agent modules: selection and delivery boundaries

Status: exploratory agent directions, 2026-10-07 (D-067, D-068).
Supervisor → Firewall → Remote remains the planned agent sequence. Monitor
is a candidate, not a commitment to implement host integrity, inventory,
vulnerability triage and remediation together.

The product discovery process, operator research and service experiments are
owned by product coordination, in the maintainer's private coordination
repository (D-073). This checkout retains the criteria required to accept
agent work.

## Accepting an agent capability

Before defining an implementation phase, record:

1. The operator problem, evidence and product decision to proceed or narrow.
2. What must run locally, versus what belongs to a backend, dashboard,
   existing collector or exporter. Create a module only when its local
   responsibility and lifecycle justify one under ADR-0015.
3. Minimum agent scope, non-goals, input/output contracts, supported versions,
   privileges, bounded resource use, retention and offline/failure behaviour.
4. An agent ADR, security/dependency review, acceptance criteria and delivery
   sub-phases in PLAN.md. Product discovery alone does not approve code.

Do not reserve packages, expand the core contract or introduce dependencies
for every product candidate. Modules must remain independent of each other
and the agent must build without a dashboard checkout.

## Candidate agent integrations

| Candidate | Local responsibility to evaluate | Outside the agent |
|---|---|---|
| Prometheus | Export agent health/check/recovery telemetry; optionally consume bounded external signals | Metrics storage and fleet dashboards |
| Grafana | Supply integration metadata or links where justified | Dashboard implementation and hosting |
| OpenObserve | Export selected redacted events/logs, or query bounded evidence through an adapter | Logs/metrics/traces storage and fleet correlation service |
| Monitor | Selected local integrity, inventory or vulnerability evidence collection | Fleet graph, multi-tenancy, commercial workflow and web interface |

The existing Prometheus [backlog](backlog.md) item covers the agent's own telemetry only.
Q-015 selects Monitor scope; Q-018 determines whether an observability
capability needs a module, adapter or exporter. None of these entries commits
to a dependency, backend compatibility or release date. Operational and
security correlation is another future direction that requires discovery
(D-067).

Define queue bounds, retry/drop policy, cardinality and payload limits,
redaction, authentication and backend-unavailable behaviour. The local
control interface is a Unix socket: a scrape endpoint needs an explicit
transport/security decision. External signals cannot implicitly authorise
restarts or firewall changes.

## Agent implications of product research

These points come from the product research of 2026-10-07 (D-066, D-068).
The research and the commercial experiment belong to product coordination
(D-073).

- Open source and Go learning come first. Product validation does not
  reorder Supervisor, Firewall and Remote, and does not change the v1.0.0
  scope.
- If Monitor is selected, its inventory, applicability and exposure data
  keep their evidence, provenance, freshness and explicit unknown states.
- Do not add interfaces or dependencies for speculative inventory or
  triage to the agent. An imported-report experiment can be a separate tool or
  service.
- Fleet storage, multi-tenancy, billing and the dashboard are outside this
  repository.
- Remote sync is outbound only. Configuration delivery keeps the local
  module switches, safety gates and command permissions (R-021).

Q-017 asks whether product evidence justifies an agent roadmap change, and
R-023 covers the correctness risks of an agent triage capability; both are
open. A change needs a decision and a pull request that updates PLAN,
architecture, threat model and release criteria before any code. The
summary of 2026-10-07 is archived in
[startup-assessment.md](archive/startup-assessment.md).

Shared ownership and Remote contract: [project boundaries](project-layout.md).
