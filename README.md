# Sentinel

**Sentinel** is a Linux service and infrastructure watchdog written in Go.
It watches systemd services, supervised processes, scheduled jobs and
HTTP(S) endpoints; when something fails it records the event, applies a
bounded recovery policy (restart with delay, backoff, attempt window and
restart-loop protection), escalates through webhook notifications when
recovery is exhausted, and notifies again when the target is healthy.

> **Project status: early development (Phase 1 of 7).** The configuration
> system, logging, event model and state persistence are implemented and
> tested. Monitors, the daemon, the CLI and packages are **not available
> yet**. See [PLAN.md](PLAN.md) for the roadmap and progress.

## Features

| Feature | Status |
|---|---|
| YAML configuration (`version: 1`), main file + `conf.d/`, strict validation, `${VAR}` expansion | ✅ implemented |
| Structured logging (text / JSON / journald priority prefixes) with secret redaction | ✅ implemented |
| Event model and webhook payload schema | ✅ implemented |
| JSON state with atomic writes, schema versioning, corruption recovery, retention | ✅ implemented |
| Cron expression parsing (5 fields + `@daily`-style macros) | ✅ implemented (scheduling: Phase 4) |
| Recovery engine: max attempts, window, delay, fixed/exponential backoff, stable_after, cooldown | 🚧 Phase 2 |
| Generic JSON webhook notifications with retry and de-duplication | 🚧 Phase 2 |
| systemd and process monitors | 🚧 Phase 3 |
| HTTP/HTTPS and cron monitors | 🚧 Phase 4 |
| `sentineld` daemon, Unix-socket protocol, `sentinelctl` | 🚧 Phase 5 |
| systemd unit, OpenRC script, DEB/RPM/tarball packages | 🚧 Phase 6 |

## Architecture

Separate packages for configuration, scheduling, monitors, recovery,
notifications, state, logging, local API, CLI and packaging. Details:
[docs/architecture.md](docs/architecture.md).

## Requirements

- Linux: RHEL 8+, Ubuntu 22.04+ (systemd), Alpine Linux (OpenRC);
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

monitors:
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
explicitly; planned monitor types (`mount`, `port`, …) are rejected with a
clear message instead of being ignored.

## CLI, notifications, systemd, OpenRC, packaging

Not available yet — designed in [PLAN.md](PLAN.md) (Phases 2–6). Planned
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
`docs/security.md` with the daemon (Phase 5–6).

## Current limitations

Only the foundation exists: there is no runnable binary yet. Anything not
marked ✅ above is not implemented.

## Roadmap

- **MVP:** systemd, process, HTTP and cron monitors; webhook; Unix socket;
  CLI; JSON state; DEB; RPM; OpenRC; systemd unit; GitHub Actions.
- **Next:** port, mount, log and advanced resource monitors; cgroups v2;
  Slack and Teams; Prometheus metrics; local HTTP API; APK; advanced
  process groups; dashboard.

## License

[Apache License 2.0](LICENSE).
