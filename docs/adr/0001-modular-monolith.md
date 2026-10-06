# ADR-0001: Modular monolith with in-process providers

- Status: Accepted
- Date: 2026-10-06
- Phases: all

## Context

Sentinel grows from a service watchdog (systemd, process, HTTP, cron) into
a host supervision and network-security orchestrator: host firewall
(nftables, iptables), dynamic blocklists, container runtimes, Kubernetes
NetworkPolicy, CrowdSec and, later, WAF/reverse-proxy integrations.

Requirements that shape the process architecture:

- one static binary per role (`CGO_ENABLED=0`, D-023) for RHEL 8+,
  Ubuntu 22.04+ and Alpine, amd64 and arm64;
- every external integration replaceable and testable without the real
  software in CI;
- no monolith *in code*: core, monitors, scheduler, recovery, events,
  state, notifications, firewall, containers, Kubernetes, security
  integrations, CLI and packaging stay separate;
- privileges are already high (root for `systemctl restart` and user
  switching, R-001); firewall enforcement adds `CAP_NET_ADMIN`.

## Options considered

| Option | Description | Pros | Cons |
|---|---|---|---|
| **A. Modular monolith** | One `sentineld` process; each domain is an internal package behind small interfaces; providers wired explicitly by the daemon. | One binary, one config, one state store, simplest packaging, cheap event bus, easy to test with fakes. | All providers share the daemon's privileges; a provider crash/panic can affect the daemon. |
| B. Out-of-process plugins | Core + plugins over gRPC/exec (go-plugin style). | Fault and privilege isolation per plugin; third-party plugins. | New dependencies (gRPC/protobuf), versioned plugin protocol, packaging of N binaries, harder end-to-end tests. |
| C. Multiple daemons | `sentineld` (supervision) + `sentinel-fw` (firewall) + `sentinel-k8s`, talking over local sockets. | Real privilege separation (only the firewall agent holds `CAP_NET_ADMIN`). | N units for systemd and OpenRC, distributed state, cross-daemon transactions, more failure modes. |
| D. Go `plugin` package | Shared objects loaded at runtime. | — | Requires cgo, exact toolchain match, no musl static builds. Rejected. |

## Decision

**Option A**, with seams that keep Option C possible later:

1. Each domain is a package tree under `internal/` (`firewall/`,
   `blocklist/`, `container/`, `kubernetes/`, `security/`) that depends
   only on `pkg/model`, shared leaf packages (`netspec`, `redact`,
   `executor`, `audit`) and its own sub-packages — never on `daemon`,
   never on another domain.
2. Cross-domain interaction goes through **events** (ADR-0002) or through
   **small consumer-defined interfaces** injected by `internal/daemon`
   (e.g. the blocklist module receives a `SetUpdater`, not the firewall
   package).
3. Providers are constructed by explicit factory functions in the daemon
   from configuration. No `init()` self-registration, no global registry.
4. Every provider call takes a `context.Context` with a deadline; panics
   inside provider goroutines are recovered, reported as `error` status
   and an event, and never take down monitors of other domains.
5. Read-only access is enforced by the client the provider receives
   (ADR-0004), so a later move of a provider out of process (Option C)
   only changes the wiring, not the provider contract.

Dependency direction (enforced in review, later by a test that inspects
`go list -deps`):

```
cmd/*  ─▶ internal/daemon ─▶ domains (monitor/*, recovery, firewall, blocklist,
                                       container, kubernetes, security/*)
                         └─▶ core services (events, state, audit, notification,
                                            scheduler, transport, lifecycle)
domains, core services ─▶ leaf packages (netspec, executor, redact, privilege,
                                          logging, config) ─▶ pkg/model
```

## Consequences

- One unit file / OpenRC script and one state directory remain enough.
- The daemon process holds every privilege any enabled provider needs;
  the threat model (docs/threat-model.md) treats it as root-equivalent.
  Disabled domains are not constructed at all, so they open no sockets and
  run no binaries.
- Heavy client libraries would land in the single binary; the dependency
  policy (ADR-0013) therefore prefers small stdlib clients.
- Revisit Option C if a deployment needs to run supervision without
  `CAP_NET_ADMIN` or the firewall agent without root (tracked as R-015).

## Follow-ups

- Phase 3: architecture test that fails when a domain package imports
  `internal/daemon` or another domain.
