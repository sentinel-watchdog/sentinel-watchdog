# Sentinel Watchdog

<img src="assets/branding/sentinel-watchdog.png" alt="Sentinel Watchdog: watchdog head inside a shield" width="240">

Logo and usage notes: [branding](docs/branding.md).
Project responsibilities: [core, design and website](docs/project-layout.md).

**Sentinel Watchdog** ("Sentinel" for short) is a Linux service,
infrastructure and network-security watchdog written in Go. One tool for
Linux-only hosts, container hosts and Kubernetes clusters, simple to
configure and safe by default. Docs: <https://sentinel-watchdog.io>
(published from Phase 9).

- **Supervision:** it watches systemd services, supervised processes,
  scheduled jobs and HTTP(S) endpoints; when something fails it records
  the event, applies a bounded recovery policy (restart with delay,
  backoff, attempt window and restart-loop protection), escalates through
  webhook notifications when recovery is exhausted, and notifies again
  when the target is healthy.
- **Network and security orchestration (planned):** observe and — only
  when explicitly armed — manage a dedicated host firewall table
  (nftables, iptables fallback) with plan / apply / rollback, feed dynamic
  blocklists (local lists, CrowdSec decisions), discover Docker and Podman
  containers, observe Kubernetes and plan/apply NetworkPolicies, and
  integrate with WAF and reverse-proxy tools. Sentinel
  orchestrates; it does not replace CrowdSec, a WAF, a CNI or the
  container runtime.

> **Project status: early development.** Phase 1 (configuration,
> logging, event model, state persistence) is implemented and tested.
> Phase 2 produced the design for the network and security features
> (architecture, ADRs, threat model) — **no network or security feature is
> implemented**. Supervisors, the daemon, the CLI and packages are **not
> available yet**. See [PLAN.md](PLAN.md) for the roadmap and progress.

## Features

| Feature | Status |
|---|---|
| YAML configuration (`version: 1`), main file + `conf.d/`, strict validation, `${VAR}` expansion | ✅ implemented |
| Structured logging (text / JSON / journald priority prefixes) with secret redaction | ✅ implemented |
| Event model and webhook payload schema | ✅ implemented |
| JSON state with atomic writes, schema versioning, corruption recovery, retention | ✅ implemented |
| Cron expression parsing (5 fields + `@daily`-style macros) | ✅ implemented (scheduling: Phase 7) |
| Recovery engine: max attempts, window, delay, fixed/exponential backoff, stable_after, cooldown | 🚧 Phase 4 |
| Generic JSON webhook notifications with retry and de-duplication | 🚧 Phase 4 |
| systemd and process supervisors | 🚧 Phases 6–7 |
| HTTP/HTTPS supervisor · cron supervisor | 🚧 Phase 5 · Phase 7 |
| `sentineld` daemon, Unix-socket protocol, `sentinelctl` | 🚧 Phases 5 (minimal) and 8 (complete) |
| systemd unit, OpenRC script, DEB/RPM/tarball packages | 🚧 Phase 9 |
| Provider/capability model, read-only gated clients, CIDR/port validation | 📐 design only (Phase 11) |
| nftables: status, capabilities, plan, dry-run (dedicated `inet sentinel` table) | 📐 design only (Phase 12) |
| nftables: apply, confirm, rollback, safety timeout, audit log | 📐 design only (Phase 13) |
| Dynamic blocklists (local/file), CrowdSec read-only decisions as events | 📐 design only (Phase 14) |
| CrowdSec decision → firewall synchronisation | 📐 design only (Phase 15) |
| Docker and Podman discovery | 📐 design only (Phase 16) |
| Kubernetes read-only discovery, CNI detection | 📐 design only (Phase 17) |
| NetworkPolicy planner and drift detection | 📐 design only (Phase 18) |
| WAF / reverse-proxy integrations | 📐 design only (Phase 19) |
| Kubernetes NetworkPolicy enforcement | 📐 design only (Phase 20) |
| iptables backend (legacy / nf_tables) | 📐 design only (Phase 21) |

Legend: ✅ implemented · 🚧 planned (supervision MVP, v0.1.0) · 📐 designed
(ADR in [docs/adr/](docs/adr/README.md)), not implemented. Configuration
sections for planned features are rejected, never silently ignored.

## Architecture

A modular monolith: one daemon, strictly separated packages for core
supervision, events, recovery, state, audit, notifications, firewall,
blocklists, containers, Kubernetes, security integrations, CLI and
packaging, wired through small interfaces and explicit providers.
Details: [docs/architecture.md](docs/architecture.md) ·
decisions: [docs/adr/](docs/adr/README.md) ·
threat model: [docs/threat-model.md](docs/threat-model.md).

### Observation vs enforcement

Observation (checks, status, plans, diffs, discovery) is separate from
enforcement (restarts, firewall changes, NetworkPolicy changes). Every
enforcing domain is disabled by default and starts in `read_only` with
`dry_run: true`; changing the network requires `mode: enforce` **and**
`dry_run: false`, a fresh plan, confirmation, an audit record and a
backup, with automatic rollback if the change is not confirmed within the
safety timeout. Sentinel never flushes the global firewall ruleset and
only touches objects it owns.

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
task check       # everything CI runs: fmt-check, vet, lint, test, race, build
task cover       # coverage report (coverage.html)
```

Tests do not need systemd or root.

## Configuration

Default locations:

| Path | Purpose |
|---|---|
| `/etc/sentinel/sentinel.yaml` | main configuration |
| `/etc/sentinel/conf.d/*.yaml` | optional fragments, loaded in lexical order |
| `/var/lib/sentinel/state.json` | runtime state |
| `/run/sentinel/sentinel.sock` | control socket |

Minimal example:

```yaml
version: 1

notifications:
  - name: main-webhook
    type: webhook
    url: ${SENTINEL_WEBHOOK_URL}
    headers:
      Authorization: Bearer ${SENTINEL_WEBHOOK_TOKEN}

supervisors:
  - name: nginx
    type: systemd
    service: nginx.service
    recovery:
      action: restart
      max_attempts: 5
      window: 10m
    notifications: [main-webhook]

  - name: api-health
    type: http
    url: https://example.org/health
    check_interval: 30s
    expect:
      status_codes: [200]
      body_contains: healthy
    notifications: [main-webhook]
```

Full example: [configs/sentinel.example.yaml](configs/sentinel.example.yaml).
Reference: [docs/configuration.md](docs/configuration.md).

Key rules: unknown keys are errors; secrets come only from environment
variables; commands are executed without a shell unless `script:` is used
explicitly; planned supervisor types (`mount`, `port`, …) are rejected with a
clear message instead of being ignored.

## CLI, notifications, systemd, OpenRC, packaging

Not available yet — designed in [PLAN.md](PLAN.md) (Phases 5 and 8). Planned
CLI commands: `status [name]`, `list`, `start|stop|restart|enable|disable|reset <name>`,
`reload`, `validate`, `config show`, `events`, `logs <name>`.

## Security

- No TCP listener: the control socket is a Unix socket with configurable
  mode (world-writable modes are rejected).
- No implicit shell; command paths must be absolute.
- Secrets are read from the environment and redacted from logs, the
  redacted config view and (later) notifications. The state file never
  stores configuration.
- State and runtime files are created with restrictive permissions.

Privilege requirements (root vs. polkit for `systemctl restart`, user
switching for supervised processes) will be documented in
`docs/security.md` with the daemon (Phases 8–9).

## Current limitations

Only the foundation exists: there is no runnable binary yet. Anything not
marked ✅ above is not implemented.

## Roadmap

Full order and status: [PLAN.md §8](PLAN.md#8-phases).

| Release | Content |
|---|---|
| first runnable build (Phase 5) | `sentineld` + `sentinelctl` with HTTP supervisors, state, webhooks |
| **v0.1.0** (Phases 3–10) | systemd, process, HTTP and cron supervisors; recovery engine; webhook; Unix socket with authorization tiers; audit log; CLI; DEB/RPM/tarball; systemd unit and OpenRC script |
| **v0.2.0** (Phases 11–12) | provider framework; nftables status, capabilities, plan, diff, dry-run |
| **v0.3.0** (Phases 13–15) | nftables apply/confirm/rollback with safety timeout; dynamic blocklists; CrowdSec decisions and synchronisation |
| **v0.4.0** (Phases 16–17) | Docker and Podman discovery; Kubernetes read-only discovery and CNI detection |
| **v0.5.0** (Phases 18–19) | NetworkPolicy planner and drift detection; WAF / reverse-proxy integrations |
| **v0.6.0** (Phases 20–21) | Kubernetes NetworkPolicy enforcement; iptables backend for legacy hosts |
| **v1.0.0** (Phase 22) | stable configuration schema, socket API and event payload; complete docs site |
| later | host monitoring (file integrity, package and vulnerability scanning); port, mount, log and advanced resource supervisors; cgroups v2; Slack and Teams; Prometheus metrics; threat-intelligence feeds; containerd; APK |

## License

[Apache License 2.0](LICENSE).
