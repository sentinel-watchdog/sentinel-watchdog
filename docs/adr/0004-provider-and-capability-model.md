# ADR-0004: Provider and capability model; read-only by construction

- Status: Accepted
- Date: 2026-10-06
- Phases: 11 (framework), each integration phase (providers)

## Context

Each external integration (nft, iptables, Docker, Podman, Kubernetes API,
CrowdSec LAPI, WAFs) must be detectable, report what it can do, be
mockable, and distinguish *not implemented*, *not present* and *not
allowed*. Phase 1 already defines `model.CapabilityStatus`:
`supported | unsupported | unavailable | permission_denied | error`.

## Decision

### Terminology

- **Provider**: a component that talks to one external system
  (`nftables`, `iptables`, `docker`, `podman`, `kubernetes`, `crowdsec`).
- **Capability**: one named feature of a provider on this host
  (`nftables.json`, `nftables.set_timeout`, `ipv6`,
  `kubernetes.networkpolicy_enforcement`).
- **ProviderState** (new, `pkg/model`): overall health —
  `disabled | ok | degraded | unavailable | permission_denied | unsupported | error`.

Status meanings (shared by capabilities and provider state):

| Status | Meaning | Example |
|---|---|---|
| `unsupported` | Sentinel does not implement it, or the target cannot do it by design | `managed_only: false`; set timeouts on a kernel without them |
| `unavailable` | Not present / not reachable on this host | no `nft` binary; Docker socket missing; LAPI connection refused |
| `permission_denied` | Present but Sentinel lacks the privilege | no `CAP_NET_ADMIN`; HTTP 401/403 from LAPI or Kubernetes |
| `degraded` (provider only) | Works partially | IPv6 disabled; one of two container runtimes down |
| `error` | Unexpected failure; details in `last_error` | malformed response |

Status is never inferred from human-readable output when a structured
source exists (e.g. `CAP_NET_ADMIN` is read from `CapEff` in
`/proc/self/status`, not from an `nft` error string).

### Public, serialisable types (`pkg/model`)

```go
type Capability struct {
    Name   string           `json:"name"`
    Status CapabilityStatus `json:"status"`
    Detail string           `json:"detail,omitempty"`
}

type ProviderStatus struct {
    Name          string        `json:"name"`
    Kind          string        `json:"kind"`           // firewall|container|kubernetes|security
    EffectiveMode string        `json:"effective_mode"` // read_only|dry_run|enforce|observe
    State         ProviderState `json:"state"`
    Capabilities  []Capability  `json:"capabilities"`
    Conflicts     []Conflict    `json:"conflicts,omitempty"` // other managers detected (firewalld, Docker, …)
    CheckedAt     time.Time     `json:"checked_at"`
    LastError     string        `json:"last_error,omitempty"` // redacted
}
```

### Interfaces: small and owned by the consumer

The spec's 7-method `FirewallProvider` is kept as the *facade* the daemon
and CLI use (ADR-0005), but components depend on narrow interfaces:

```go
// internal/provider (status aggregation) consumes:
type Describer interface {
    Name() string
    Status(ctx context.Context) (model.ProviderStatus, error)
}
```

Domain-specific interfaces (`Observer`, `Committer`, `Lister`,
`DecisionSource`, …) are defined in the consuming package and documented
in the domain ADRs. There is **no `pkg/provider`** package: publishing Go
interfaces in `pkg/` would freeze them as public API and contradict the
"interfaces defined by the consumer" rule. Wire types live in `pkg/model`
and `pkg/api`.

### Read-only by construction

Each provider receives a *gated client*, not raw access:

| External system | Gate | Allowed in effective mode `read_only` |
|---|---|---|
| `nft` | `executor` command builder owned by the backend; argv is assembled by the gate, never by callers | `nft -j list …`, `nft --check -j -f -` |
| `iptables*` | same | `iptables-save`, `iptables-restore --test` |
| HTTP over Unix socket (Docker, Podman) | `http.RoundTripper` wrapper | `GET` only |
| Kubernetes API | `http.RoundTripper` wrapper | `GET` (incl. watch); `POST` only to `selfsubjectaccessreviews`/`selfsubjectrulesreviews` (non-mutating reviews) |
| CrowdSec LAPI | `http.RoundTripper` wrapper | `GET`; `POST /v1/watchers/login` only when alerts are enabled (credential exchange) |

A read-only provider is constructed **without** the mutating interface
implementation (e.g. the firewall service gets an `Observer`, no
`Committer`), so an enforcement call cannot even be compiled into that
path. Tests assert that every argv/request produced in read-only mode is on
the allowlist.

### Wiring and testing

- Factories in `internal/daemon` build providers from config; tests build
  them with fakes (`executor.FakeRunner`, `httptest.Server` over a Unix
  socket, fake Kubernetes API server).
- `internal/provider` aggregates `Describer`s for `sentinelctl … status`,
  refreshes capabilities on start, on reload and every
  `capability_refresh_interval` (default 5m), and emits
  `provider_state_changed` events.

## Consequences

- Phase 1 `CapabilityStatus` is reused unchanged; `ProviderState` and
  `Conflict` are added in Phase 11.
- Every provider has the same status surface; the CLI renders one table.
- The read-only allowlists are part of the security contract and are
  reviewed like code that mutates state.
