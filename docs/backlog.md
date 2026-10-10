# Backlog

Work that is known but not scheduled in a phase of [PLAN.md](../PLAN.md).
An item moves to a phase (and leaves this list) when it is planned; a
finding that is real but outside the change that found it lands here as
**Deferred** ([development workflow](development-workflow.md), section 3).

Deferred findings and follow-ups have an ID (`B-xxx`, never reused),
their origin and a severity that guides priority. Features are grouped by
area; a feature gets an ID when it is picked for planning.

## Deferred findings and follow-ups

| ID | Area | Item | Origin | Severity |
|---|---|---|---|---|
| B-001 | config | Line numbers for semantic configuration problems. YAML-level problems already carry `line N:`; semantic ones (validation of values) carry file and dotted path only. Add a line to `config.Problem` and locate findings from the parsed nodes. | Phase 2c-1 planning, 2026-10-09 | low |
| B-002 | CI | Fuzzing in CI (nightly job), once the fuzz targets' run time is known. | Agent workflow, D-069 | low |
| B-003 | tooling | A Claude Code hook running `task check` (slow; CI enforces the same checks). | Agent workflow, D-069 | low |

## Features

**Core:** Slack and Teams notification providers; Prometheus metrics
(Sentinel's own telemetry only); local HTTP API; APK package.

**Supervisor:** port, mount, log, advanced resource and `process_group`
services; OpenRC service type; cgroups v2 limits; recovery action
`execute`; container restart actions.

**Firewall:** HTTPS blocklists with checksum/signature (Q-011);
threat-intelligence feeds; Fail2ban integration; containerd adapter; more
WAF/proxy adapters (HAProxy, Caddy, Envoy, generic); admission
integration; eBPF/CNI-specific policies; `DOCKER-USER` / forward rules
(explicit opt-in); advanced remediation workflows; out-of-process
firewall agent (R-015).

**Future directions:** OpenObserve, Prometheus and Grafana integrations;
host integrity; inventory/vulnerability triage; operational/security
correlation. All require discovery (D-067,
[candidate roles](future-modules.md)).
