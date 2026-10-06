# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project
uses [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- Repository and CI security baseline (Phase 1): CI workflow (lint, tests
  on amd64 and arm64, Linux builds, govulncheck, workflow linting with
  actionlint and zizmor), CodeQL and OpenSSF Scorecard workflows,
  Dependabot with cooldown, `SECURITY.md`, `CONTRIBUTING.md`, `CODEOWNERS`,
  pull request and issue templates, `docs/development.md`, Taskfile tasks
  `test-linux`, `lint-actions`, `labels`, `tidy-check`.
- Module architecture and configuration layout
  ([ADR-0015](docs/adr/0015-modules-and-configuration-layout.md)): core,
  platform adapters and modules (supervisor, firewall, later remote and
  monitor); central `sentinel.yaml` with module switches and safety gates
  plus one configuration directory per module.
- Decision log moved to `docs/decisions.md` (D-063 module architecture,
  D-064 clean restart, D-065 roadmap and versioning).
- Three-project layout documented: core, design and website, with technical
  documentation owned by core and visual asset masters owned by design.
- Sentinel Watchdog logo: transparent PNG emblem for repository and tool
  avatars, with usage notes and generation prompt in `docs/branding.md`.
- Foundation verification refresh in `docs/project-audit.md` (2026-10-06).
- Phase 1 foundation (now the Phase 0 prototype, replaced in Phase 2 per D-064):
  - YAML configuration model (`version: 1`) with main file and `conf.d/`
    fragments, deterministic load order, strict unknown-key detection with
    line numbers, `${VARIABLE}` expansion, defaults and rigorous validation.
  - Cron expression parser used for configuration validation.
  - Structured logging with text, JSON and journald formats and secret
    redaction.
  - Public event, supervisor state and capability status model.
  - JSON state file with atomic writes, schema versioning, corruption
    quarantine and retention.
  - Taskfile, golangci-lint configuration, documentation and development
    plan.
- Phase 2 design baseline for the network and security features (documentation
  only, no runtime behaviour):
  - Project audit (`docs/project-audit.md`) with the collisions between
    supervision and the new domains.
  - Architecture Decision Records 0001–0014 (`docs/adr/`): modular
    monolith, generalised event model, observation vs enforcement modes,
    provider/capability model, backend-agnostic firewall service,
    nftables and iptables backends, blocklists and CrowdSec, container
    runtimes, Kubernetes, state/audit/transactions, control-plane
    authorization tiers, dependency policy, WAF providers.
  - Threat model (`docs/threat-model.md`).
  - Target architecture in `docs/architecture.md`; decisions, risks and
    open questions in `PLAN.md`; README feature table distinguishes
    implemented, scheduled and design-only features.
  - Roadmap as one numbered sequence (Phases 1–22) with a walking
    skeleton at Phase 5 and releases v0.1.0 (supervision) through v1.0.0
    (Kubernetes, WAF and iptables included).

### Changed

- Roadmap restructured into phases 1–6: repository and CI security
  baseline, core, supervisor (v1.0.0), firewall (v1.x), remote, monitor.
  New modules and capabilities ship as minor releases.
- Project renamed to **Sentinel Watchdog**; Go module path is now
  `github.com/sentinel-watchdog/sentinel-watchdog`. Binaries stay
  `sentineld` and `sentinelctl`.
- "Monitor" renamed to "supervisor" across configuration (`supervisors:`),
  types, event types (`supervisor_failed`, `supervisor_recovered`), state
  keys and docs; "monitoring" is reserved for a future host-integrity
  domain (D-062). The observe/change split is now called "observation vs
  enforcement".
