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

> **Project status: foundations designed, implementation restarting.** The
> architecture (core + modules, [ADR-0015](docs/adr/0015-modules-and-configuration-layout.md))
> defines the core and Supervisor → Firewall → Remote sequence. Future
> modules require [product discovery](docs/future-modules.md) first.
> The existing Go code is a prototype that
> Phase 2 replaces (D-064). **There is no runnable daemon or CLI yet.**
> See [PLAN.md](PLAN.md) for agent phases and progress.

## Roadmap

| Phase | Content | Release |
|---|---|---|
| 1 | Repository, CI and security baseline (protected branches, pinned actions, govulncheck, CodeQL, Scorecard) | — |
| 2 | Core: configuration loader and module framework; events, state, notifications; daemon, Unix socket with authorization tiers, audit log, CLI | — |
| 3 | Supervisor module: HTTP, processes, recovery, systemd, cron; packages (DEB/RPM/tarball, signed); hardening | **v1.0.0** |
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
enforcement (restarts, firewall changes, NetworkPolicy changes). Every
enforcing module is disabled by default and starts in `read_only` with
`dry_run: true`; changing the network requires `mode: enforce` **and**
`dry_run: false` in the central configuration file, a fresh plan,
confirmation, an audit record and a backup, with automatic rollback if the
change is not confirmed within the safety timeout. Sentinel never flushes
the global firewall ruleset and only touches objects it owns.

## Configuration

The configuration loader exists (Phase 2a); the daemon that uses it arrives
in Phase 2c. A central file holds the core settings and the module switches,
and each enabled module has its own directory:

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
# /etc/sentinel/supervisor/nginx.yaml
version: 1
services:
  - name: nginx
    type: systemd
    unit: nginx.service
    recovery: { action: restart, max_attempts: 3, window: 10m }
    notifications: [ops]
```

Key rules: only the central file configures the core and arms enforcing
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
task --list      # available tasks
task build       # compile all packages
task test        # unit tests
task test-race   # unit tests with the race detector
task lint        # golangci-lint
task check       # offline CI checks: fmt, tidy, vet, lint, test, race, build
task vuln        # govulncheck
task test-linux  # tests in glibc and musl containers (local Docker)
task cover       # coverage report (coverage.html)
```

Tests do not need systemd or root. Details, CI and repository security:
[docs/development.md](docs/development.md); contributing:
[CONTRIBUTING.md](CONTRIBUTING.md).

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
- Configuration files must be root-owned and not group- or world-writable.

Report vulnerabilities privately: [SECURITY.md](SECURITY.md).

## License

[Apache License 2.0](LICENSE).
