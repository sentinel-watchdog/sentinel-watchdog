# Sentinel Watchdog

[![CI](https://github.com/sentinel-watchdog/sentinel-watchdog/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/sentinel-watchdog/sentinel-watchdog/actions/workflows/ci.yml)
[![CodeQL](https://github.com/sentinel-watchdog/sentinel-watchdog/actions/workflows/codeql.yml/badge.svg?branch=main)](https://github.com/sentinel-watchdog/sentinel-watchdog/actions/workflows/codeql.yml)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/sentinel-watchdog/sentinel-watchdog/badge)](https://scorecard.dev/viewer/?uri=github.com/sentinel-watchdog/sentinel-watchdog)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)

<img src="assets/branding/sentinel-watchdog.png" alt="Sentinel Watchdog: watchdog head inside a shield" width="240">

Logo and usage notes: [branding](docs/branding.md).
Project responsibilities: [agent, dashboard, design and website](docs/project-layout.md).

**Sentinel Watchdog** ("Sentinel" for short) is a Linux agent for
service supervision and host network security, written in Go. One tool for
Linux hosts, container hosts and Kubernetes nodes, simple to configure and
safe by default. This repository contains the agent and its local CLI.
Fleet backend, dashboard and public website belong to separate projects.
Documentation publishing at `sentinel-watchdog.io` is planned in the website
project; no published docs site is claimed here.

Sentinel is a shared **core** (configuration, events, state,
notifications, audit log, control socket and CLI) plus **modules** that
the configuration enables:

| Module | Purpose | Release |
|---|---|---|
| **Supervisor** | Watch systemd units, supervised processes, HTTP endpoints and cron jobs; restart with bounded recovery; notify through webhooks | v1.0.0 |
| **Firewall** | Manage a dedicated nftables table (iptables fallback) with plan / apply / confirm / rollback; blocklists; act as a CrowdSec remediation component; WAF integrations; container hosts and Kubernetes nodes | v1.1 … v1.6 |
| **Remote** | Agent-side outbound connection to a fleet backend for configuration and status | later |
| **Future modules** | Exploratory: Monitor (host integrity, inventory, vulnerabilities) and observability integrations; discovery determines services and scope | after Remote |

Sentinel orchestrates; it does not replace CrowdSec, a WAF, a CNI or the
container runtime.

> **Project status: pre-release, the Supervisor first.** The tags v0.1.0,
> v0.2.0 and v0.3.0 are source previews of Phases 3a, 3b and 3c.
> The core runs: `sentineld` and `sentinelctl` load and validate the
> configuration, serve the control socket with authorization tiers, keep
> the audit log, reload and notify systemd. The Supervisor checks HTTP
> services, runs process services and cron jobs, observes systemd units
> and restarts failed ones, within a bounded recovery budget, with
> operator commands, a memory limit and `sentinelctl supervisor logs`. No
> binaries or packages are published yet: the first signed release is
> v0.3.1 (Phase 3d-3). [Install](docs/install.md) covers releases and the
> snapshots to use until then. The status of each phase
> is in [PLAN.md §7](PLAN.md#7-phases); the documentation is in
> [docs/](docs/README.md). The architecture
> ([ADR-0015](docs/adr/0015-modules-and-configuration-layout.md)) defines
> the Supervisor, Firewall, Remote sequence; future modules require
> [product discovery](docs/future-modules.md) first.

## Roadmap

| Phase | Content | Release |
|---|---|---|
| 1 | Repository, CI and security baseline (protected branches, pinned actions, govulncheck, CodeQL, Scorecard) | none |
| 2 | Core: configuration loader and module framework; events, state, notifications; daemon, Unix socket with authorization tiers, audit log, CLI | none |
| 3 | Supervisor module: HTTP, processes, recovery, systemd, cron; packages (DEB/RPM/APK/tarball, signed releases); hardening | **v1.0.0** |
| 4 | Firewall module: nftables plan/apply/rollback, blocklists, CrowdSec, containers and Kubernetes, NetworkPolicy, WAF, iptables | v1.1 … v1.6 |
| 5 | Remote module: agent-side protocol, trust model and integration | later |
| 6+ | Future-module discovery, then selected capabilities (Monitor and observability candidates) | exploratory |

Versioning: semantic versioning; new modules and capabilities arrive in
minor releases; a major release only for breaking changes (D-065).

## Architecture

A modular monolith: one daemon, a core, shared platform adapters and
modules that never import each other, wired by the daemon through small
interfaces. Details: [docs/architecture.md](docs/architecture.md) ·
decisions: [docs/decisions.md](docs/decisions.md) and
[docs/adr/](docs/adr/README.md) · threat model:
[docs/threat-model.md](docs/threat-model.md).

### Observation vs enforcement

Observation (checks, status, plans, diffs, discovery) is separate from
enforcement (starting and restarting programs and units, firewall
changes, NetworkPolicy changes), and every enforcing action is off until
the central configuration file arms it:

- The supervisor starts and restarts programs only with
  `modules.supervisor.processes: true`, and restarts systemd units only
  with `modules.supervisor.systemd_restart: true`. Automatic restarts
  count against a recovery budget, and every restart is audited.
- The firewall (Phase 4) starts in `read_only` with `dry_run: true`.
  Changing the network requires `mode: enforce` **and** `dry_run: false`,
  a fresh plan, confirmation, an audit record and a backup, with automatic
  rollback if the change is not confirmed within the safety timeout.
  Sentinel never flushes the global firewall ruleset and only touches
  objects it owns.

## Configuration

`sentineld` loads this configuration and `sentinelctl validate` checks it.
A central file holds the core settings and the module switches, and each
enabled module has its own directory:

```
/etc/sentinel/
├── sentinel.yaml      daemon, notifications, modules (enabled + safety gates)
├── supervisor/        services and jobs, one or more *.yaml files
└── firewall/          policies, blocklists
```

```yaml
# /etc/sentinel/sentinel.yaml
version: 1
daemon:
  state_dir: /var/lib/sentinel
notifications:
  channels:
    - name: ops
      type: webhook
      url: ${OPS_WEBHOOK_URL}
modules:
  supervisor:
    enabled: true
```

```yaml
# /etc/sentinel/supervisor/web.yaml
version: 1
services:
  - name: web
    url: https://web.example.org/health
    body_contains: '"status":"ok"'
    notifications: [ops]
```

The supervisor also runs process services and cron jobs and observes
systemd units ([docs/supervisor.md](docs/supervisor.md); examples in
`configs/supervisor/`).

Rules: only the central file configures the core and arms enforcing
modules; a disabled module's directory is not read; unknown keys are
errors; secrets come only from environment variables; commands run without
a shell unless `script:` is used explicitly; planned features are rejected
with a clear message instead of being ignored. Full rules:
[ADR-0015](docs/adr/0015-modules-and-configuration-layout.md).

## Requirements

- Linux: RHEL 8+ and compatibles (Rocky, Alma), Debian 12+, Ubuntu 22.04+ (systemd), Alpine Linux (OpenRC);
  amd64 or arm64.
- Building: Go **1.27+**, [Task](https://taskfile.dev) v3,
  [golangci-lint](https://golangci-lint.run) v2 for linting.

Binaries are built with `CGO_ENABLED=0` and embed the time zone database.

## Building and testing

```sh
task check       # offline CI checks: fmt, tidy, vet, lint, test, race, build
task --list      # every task
```

Unit tests need neither systemd nor root; integration tests run as root
on a matrix of virtual machines (`task test-vm`). Every task, CI and
repository security: [docs/development.md](docs/development.md);
contributing: [CONTRIBUTING.md](CONTRIBUTING.md).

## Security (design)

- Sentinel runs as root and can change the firewall: the repository, CI
  and release pipeline are hardened first (Phase 1), releases are signed
  (Phase 3d).
- No TCP listener: the control socket is a Unix socket with authorization
  tiers (`read`, `operate`, `admin`) from peer credentials; every
  `operate`/`admin` request is audited.
- No implicit shell; command paths must be absolute.
- Secrets are read from the environment and redacted from logs, CLI
  output, state and notifications.
- Configuration files must be owned by root or the daemon user and not
  writable by group or others.

Report vulnerabilities privately: [SECURITY.md](SECURITY.md).

## License

[Apache License 2.0](LICENSE).
