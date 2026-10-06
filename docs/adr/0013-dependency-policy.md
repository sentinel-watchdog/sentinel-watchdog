# ADR-0013: Dependency policy and evaluated libraries

- Status: Accepted
- Date: 2026-10-06
- Phases: all

## Context

Sentinel runs as root and may control the host firewall. Every dependency
is code running with those privileges, a supply-chain risk and a
maintenance cost. The only runtime dependency today is
`go.yaml.in/yaml/v3` (D-001).

## Policy

A new dependency is accepted only when **all** hold, and is recorded in
PLAN.md's decision log with: module path, version, license, purpose,
alternatives, maintenance signal (releases in the last 12 months, owners),
transitive module count and binary size delta.

1. The standard library (or `golang.org/x/*` maintained by the Go team)
   cannot do it with reasonable effort.
2. License compatible with Apache-2.0 (Apache-2.0, MIT, BSD-2/3, ISC,
   MPL-2.0 as an unmodified dependency). No GPL/AGPL/LGPL in the binary.
3. Pure Go (`CGO_ENABLED=0`), builds for linux/amd64 and linux/arm64.
4. Actively maintained; prefer official or widely used clients.
5. Structured API (no screen scraping) and testable with fakes.
6. `govulncheck` clean at adoption; Dependabot/Renovate updates in CI.

Tools used only in CI (golangci-lint, govulncheck, GoReleaser, kind,
kubeconform) are not runtime dependencies but are pinned by version in
workflows.

## Evaluated libraries (2026-10-06)

None is adopted in Phase 2 (design baseline). "Version" is pinned at adoption time.

| Module | Purpose | License | Assessment | Decision |
|---|---|---|---|---|
| `github.com/google/nftables` | nftables over netlink | Apache-2.0 | Pure Go, but low-level expressions, plans not human-reviewable | **Rejected** for Phases 12–13 (ADR-0006); revisit only if `nft` JSON is inadequate on a target |
| `github.com/coreos/go-iptables` | iptables wrapper | Apache-2.0 | Per-rule exec calls, no `iptables-restore` atomicity | **Rejected**; use `iptables-restore` directly (ADR-0007) |
| `github.com/docker/docker/client` (moby) | Docker API | Apache-2.0 | Very large transitive graph for a few GET endpoints | **Rejected**; stdlib HTTP over Unix socket (ADR-0009) |
| `github.com/containers/podman/v5/pkg/bindings` | Podman API | Apache-2.0 | Heavy, cgo/build-tag requirements | **Rejected** (ADR-0009) |
| containerd Go client | containerd API | Apache-2.0 | gRPC + protobuf stack | **Deferred** until a containerd adapter is scheduled |
| `k8s.io/client-go` | Kubernetes API | Apache-2.0 | Official, complete auth, large graph and binary growth | **Accepted** for Phase 17 (ADR-0010, D-055); confined to `internal/kubernetes/client`, version pinned at adoption |
| `github.com/crowdsecurity/go-cs-bouncer` | CrowdSec LAPI | MIT | Official bouncer library; pulls CrowdSec module dependencies; LAPI surface we need is small | **Rejected** for Phases 14–15; stdlib client (ADR-0008) |
| `github.com/prometheus/client_golang` | metrics | Apache-2.0 | Standard choice; moderate graph | **Deferred** to the metrics phase (alternative: hand-written text exposition format) |
| `github.com/coreos/go-systemd` | sd_notify, D-Bus | Apache-2.0 | Not needed: sd_notify is a datagram write; `systemctl` is used for unit control | **Rejected** |
| `golang.org/x/sys/unix` | syscalls | BSD-3-Clause | `syscall` already offers `GetsockoptUcred` and `Flock` on Linux | **Not needed** for Phases 5 and 8; adopt only if a missing syscall requires it |

## Consequences

- External integrations are reached through processes (`nft`,
  `iptables-*`, `systemctl`) or HTTP over Unix/TCP sockets with typed
  DTOs and recorded fixtures.
- Build stays static, small and reproducible; `task vuln` (govulncheck)
  runs in CI from Phase 3.
