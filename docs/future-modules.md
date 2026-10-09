# Future agent modules — selection and delivery boundaries

Status: exploratory agent directions, 2026-10-07 (D-067, D-068).
Supervisor → Firewall → Remote remains the planned agent sequence. Monitor
is a candidate, not a commitment to implement host integrity, inventory,
vulnerability triage and remediation together.

The product discovery process, operator research and service experiments are
owned by coordination, currently `docs/discovery.md` under the workspace
parent. This checkout retains the criteria required to accept agent work.

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
to a dependency, backend compatibility or release date.

Define queue bounds, retry/drop policy, cardinality and payload limits,
redaction, authentication and backend-unavailable behaviour. The local
control interface is a Unix socket: a scrape endpoint needs an explicit
transport/security decision. External signals cannot implicitly authorise
restarts or firewall changes.

Product research implications: [assessment summary](startup-assessment.md).
Shared ownership and Remote contract: [project boundaries](project-layout.md).
