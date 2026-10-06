# Project audit — Phase 2 (2026-10-06)

Audit of the repository before extending Sentinel with firewall,
container, Kubernetes and security integrations. It records what exists,
what holds up, and what must change. Decisions are in
[docs/adr/](adr/README.md) and in the PLAN.md decision log.

## 1. Inventory

| Area | Content | State |
|---|---|---|
| Module | `github.com/sentinel-watchdog/sentinel-watchdog` (organisation and repository `sentinel-watchdog/sentinel-watchdog`, D-057), Go 1.27 | ✅ |
| Dependencies | `go.yaml.in/yaml/v3` only | ✅ |
| `internal/config` | YAML model, `conf.d` loader, strict decoding, `${VAR}` expansion, defaults, validation, redacted view | ✅ 93.3 % coverage |
| `internal/logging` | slog text/json/journal/auto + redaction | ✅ 90.1 % |
| `internal/redact` | header/URL/env/text masking | ✅ 98.0 % |
| `internal/state` | state model v1, atomic store, quarantine, retention | ✅ 86.3 % |
| `internal/scheduler/cronexpr` | cron parser (no `Next()` yet) | ✅ 97.5 % |
| `pkg/model` | events, monitor states, capability statuses | ✅ 97.9 % |
| `internal/version` | ldflags metadata | ✅ (no tests, trivial) |
| Binaries, daemon, monitors, recovery, notifications, socket, packaging, CI | — | 🚧 Phases 3–10 |
| Docs | README, CHANGELOG, PLAN, docs/architecture.md, docs/configuration.md | ✅ |

Verification run during the audit: `task check` (gofmt, vet,
golangci-lint 0 issues, tests, race tests, build) passes on darwin/arm64;
`GOOS=linux GOARCH=arm64 go build ./...` passes. No `.github/workflows`
exist yet (planned for Phase 3).

## 2. Strengths to keep

- Strict configuration: unknown keys, duplicate keys and planned types are
  errors with file and line (D-008). New domains must follow the same
  pattern.
- Fail-closed env expansion (D-006), no implicit shell (D-005),
  secrets only from the environment, redaction everywhere.
- Atomic state writes with fsync, quarantine and downgrade protection
  (D-015).
- Capability statuses already distinguish `unsupported`, `unavailable`,
  `permission_denied` — exactly what the specification asks providers to
  report.
- Small packages, consumer-defined interfaces, injected time.

## 3. Gaps relative to the new direction

| Gap | Impact | Resolution |
|---|---|---|
| No event bus, recovery engine, notifier yet | Event model can still change freely | Phase 3 implements the generalised model (ADR-0002) before anything depends on it |
| No executor/Runner | Firewall backends need a safe exec layer | Phase 6 `internal/executor` is a prerequisite of Phase 12 |
| No socket/CLI | `sentinelctl firewall …` needs the protocol and authorization | Phases 5 and 8, with tiers (ADR-0012) |
| No audit log | Enforcement requires audit | Phase 8 `internal/audit` (ADR-0011) |
| No CI | Cannot gate privileged vs. unprivileged tests | Phase 3 `ci.yml`; privileged jobs added by the phases that need them |
| No shared network-address validation | Firewall, blocklists, CrowdSec, Kubernetes would duplicate CIDR/port parsing | `internal/netspec` in Phase 11 |

## 4. Collisions between supervision and the new domains

| # | Collision | Where | Resolution |
|---|---|---|---|
| C-01 | **Phase numbering**: existing Phases 1–7 (core MVP) vs the new specification's Phases 0–10 | PLAN.md, docs | One numbered sequence (Phases 1–21) in PLAN.md §8 (D-047, superseding the interim core + S-track split D-025) |
| C-02 | **Monitor-centric event model**: `monitor_name` required for non-daemon scopes; counters as top-level fields; `EventScope` = monitor/job/daemon | `pkg/model/event.go`, `state.AppendEvent` | Generalised `source`/`source_type`/`severity`/`correlation_id`/`attributes` in Phase 3 (ADR-0002) |
| C-03 | **Recovery actions**: config knows `none`/`restart` (`execute` planned) and limit actions `log/notify/restart/stop/kill`; the spec adds `block`, `unblock`, `apply_firewall_policy`, `rollback_firewall_policy` | `internal/config` | Generic action registry in Phase 4; firewall actions valid only when the firewall domain runs in `enforce` (ADR-0003) |
| C-04 | **State schema v1** has no provider/firewall/security sections | `internal/state` | Schema v2 + migration (ADR-0011) |
| C-05 | **`cooldown`** already means recovery cooldown (D-013); spec also asks for event cooldown | config | Event/notification throttle is named `repeat_interval` (ADR-0002) |
| C-06 | **`mode` vs `default_mode`**: firewall `mode: read_only` and policy `default_mode: allow` use the same word for different concepts; `allow` ≠ rule vocabulary `accept` | spec §7 | `default_action: accept\|drop` (D-048) |
| C-07 | **`family`**: `inet` at firewall level, IPv4/IPv6 at rule level | spec §3, §7, §8 | One meaning everywhere: address-family scope with nft vocabulary `inet\|ip\|ip6` (ADR-0005) |
| C-08 | **`${KUBECONFIG}` / `${CROWDSEC_LAPI_KEY}`** in disabled blocks fail the whole load when undefined (D-006) | spec §11, §12 | Example config ships these blocks commented out until implemented; `kubeconfig` default empty (ADR-0010) |
| C-09 | **`network_policy.enforcement`** duplicates `mode`; **`crowdsec.firewall_provider`** duplicates `firewall.backend` | spec §11, §12 | Dropped in favour of `mode` / `sync_decisions` (D-048) |
| C-10 | **Socket authorization**: one group grants everything (Q-002) | socket protocol plan | Tiers `read`/`operate`/`admin` (ADR-0012) |
| C-11 | **Privileges**: unit planned as root with reduced `CapabilityBoundingSet` (R-001); firewall needs `CAP_NET_ADMIN`; container sockets are root-equivalent | packaging plan | Capabilities documented per feature in docs/threat-model.md; unit drop-ins per feature (Phases 9 and 13) |
| C-12 | **Fragments (`conf.d`)** may contain `notifications` and `monitors` only | D-007 | New top-level sections (`firewall`, `blocklists`, `containers`, `kubernetes`, `security`) are main-file only, at least initially: one owner for security policy |
| C-13 | **`pkg/provider`** in the spec's layout would publish Go interfaces | spec §17 | Not created; wire types in `pkg/model`/`pkg/api`, interfaces stay in consumers (ADR-0004) |
| C-14 | **Integration test location**: repo convention is build-tagged tests next to code; spec wants `tests/integration/<domain>` | spec §17 | Privileged/real-software tests in `tests/integration/<domain>/` with tags `integration` and `privileged`; unprivileged Linux-only tests may stay next to code |
| C-15 | **systemd timer as dead-man switch** for firewall safety timeout would create a unit | binding decision | Not allowed; in-daemon timer + persisted pending transaction + rollback at start-up (ADR-0003) |
| C-16 | **Label prefix `sentinel.io/`** uses a domain the project does not own | spec §9 | `sentinel-watchdog.io/` (D-054) |

## 5. Required changes to existing code (scheduled, not done in Phase 2)

| Change | Phase |
|---|---|
| `pkg/model.Event` → generalised model; `EventScope`s; `Severity`; `SourceType`; attribute limits | Phase 3 |
| `state.File` v2 + v1 migration; `AppendEvent` keyed on `source` | Phase 3 |
| Recovery action registry (config enums become registry-validated) | Phase 4 |
| `ProviderState`, `Conflict`, `ProviderStatus` in `pkg/model`; `internal/netspec`; `internal/provider` | Phase 11 |
| Config: reserve `firewall`, `blocklists`, `containers`, `kubernetes`, `security` with "planned but not implemented" errors | Phase 11 |
| Config file ownership/mode check (refuse group/world-writable config for enforcement) | Phase 8 |
| `settings.operator_group`, `settings.admin_group` | Phase 8 |

## 6. Risks found

New risks R-009 … R-017 are listed in PLAN.md §4; threats and mitigations
in [threat-model.md](threat-model.md).
