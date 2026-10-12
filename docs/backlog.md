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
| B-004 | daemon | Logging that cannot block sentineld: a log destination that stops reading (a full stderr pipe) blocks every log write, so shutdown is bounded only for blocked routers and the event logger. Needs asynchronous logging with a bounded queue and dropped-record accounting (a logging design change). | Codex review of Phase 2c-1, 2026-10-09 (D-075) | medium |
| B-005 | notify | Flaky `TestRetriesWithBackoff`: it reads `Dispatcher.Stats` right after the fake sender reports the delivery, before the dispatcher counts it; failed once in `task test-linux` (glibc, race) on 2026-10-12, not reproduced in 300 local runs. Wait for the counter instead of reading it once. | Phase 2c-3a test runs, 2026-10-12 | low |
| B-006 | events | `clean` cuts a long string back to a character boundary without a bound: a run of stray UTF-8 continuation bytes walks the cut to 0 and empties the whole string, valid prefix included, so an event whose `source` comes from outside is rejected (`source is empty`). Stop after `utf8.UTFMax-1` bytes; keep the fuzz input as a seed. | `task fuzz` (FuzzNormalize) during Phase 2c-3a, 2026-10-12 | medium |

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
