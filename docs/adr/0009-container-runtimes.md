# ADR-0009: Container runtime adapters (Docker, Podman, containerd)

- Status: Accepted
- Date: 2026-10-06
- Phases: 16 (discovery, read-only); container monitors, actions and firewall integration later

## Context

Sentinel should discover containers, their state, port and network
mappings and labels, and later monitor them and integrate them with
firewall policy. Docker and Podman both expose an HTTP API on a Unix
socket; containerd exposes gRPC. Access to these sockets is usually
equivalent to root on the host.

## Options considered

| Option | Pros | Cons |
|---|---|---|
| Official SDKs (`github.com/docker/docker/client`, `containers/podman/.../bindings`) | Typed, complete | Very large dependency trees; Podman bindings need cgo/build tags; API churn; we need a handful of GET endpoints |
| **Stdlib HTTP over Unix socket** with small typed DTOs | No dependencies, GET-only gate is trivial, easy `httptest` fakes | We maintain DTOs and API version negotiation |

## Decision

- **Stdlib HTTP over Unix socket**, one shared helper
  (`internal/container/unixhttp`) and **two distinct adapters**:
  `internal/container/docker` and `internal/container/podman` (Podman's
  Docker-compatible and libpod endpoints differ in details such as pods,
  rootless networking and health fields).
- **Model** (`internal/container/model`): `Container{ID, Name, Image,
  Runtime, State, Health, Labels, Ports[{host_ip, host_port, container_port,
  protocol}], Networks[{name, ip, mode}], NetworkMode, RestartCount,
  StartedAt}`.
- **Consumer interfaces**: `Lister`, `Inspector`, `EventStream`
  (`GET /events`, reconnect with backoff). All GET-only in Phase 16.
- **Detection** (`runtime: auto`): Docker `/run/docker.sock` or
  `/var/run/docker.sock`; Podman rootful `/run/podman/podman.sock`;
  rootless Podman only when `socket:` is set explicitly. Version via
  `GET /version`, API version pinned to the minimum supported and
  negotiated down, never up.
- **Labels**: discovery filter `monitor_labels` (all must match; keys
  chosen by the operator). Labels that Sentinel itself defines use the
  prefix `sentinel-watchdog.io/` (D-054), a domain the project owns,
  instead of the specification's `sentinel.io/`.
- **No changes in Phase 16**: `restart_failed` and `firewall_integration` are
  rejected as "planned but not implemented" when set to `true`.
- **containerd**: deferred; needs gRPC (new dependency) and an ADR at that
  time. Kubernetes nodes are covered by the Kubernetes provider instead.

## Docker and Podman firewall facts (drive docs/containers.md)

- Docker (default `iptables: true`) creates and rewrites its own rules
  (`DOCKER`, `DOCKER-USER`, `DOCKER-ISOLATION-*` chains, NAT in
  `PREROUTING`/`POSTROUTING`). Recent Docker releases also offer an
  nftables firewall backend; the active backend must be detected, not
  assumed.
- Published ports are DNATed in `PREROUTING`: that traffic traverses
  **FORWARD, not INPUT**. Host `input` rules do not protect published
  container ports. Filtering it needs `forward` rules matching the
  post-DNAT address/port (or conntrack original destination).
- `network_mode: host` containers are filtered by `input`/`output` like
  host processes. Overlay / swarm ingress uses additional chains and
  namespaces and is out of scope for promises of isolation.
- Podman with netavark uses its own nftables or iptables rules; rootless
  Podman (pasta/slirp4netns) creates no host firewall rules and its traffic
  looks like host-process traffic.
- Sentinel reports which networking mode each container uses and which
  firewall path would apply; it never claims isolation the backend does
  not guarantee and never modifies `DOCKER*` or netavark objects.

## Consequences

- No runtime SDKs in the binary; fixtures from real API responses per
  runtime version drive the tests.
- Socket access is documented as root-equivalent (threat model T-11);
  container discovery is opt-in and read-only by construction.
