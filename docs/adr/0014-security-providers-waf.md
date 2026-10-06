# ADR-0014: Security provider abstraction (WAF, reverse proxy)

- Status: Proposed — adapters chosen at the start of Phase 19
- Date: 2026-10-06
- Phases: 19

## Context

Future providers: CrowdSec AppSec, Coraza, ModSecurity, Nginx, Traefik,
HAProxy, Caddy, Envoy and a generic reverse proxy. Sentinel must not
become a WAF. It may read events, correlate IPs and services, request a
block, notify, check health and configuration, coordinate remediation and
detect drift.

## Decision (direction)

- Interface from the specification, owned by `internal/security`:

  ```go
  type SecurityProvider interface {
      Name() string
      Detect(ctx context.Context) ([]SecurityEvent, error)   // new events since the last call (cursor kept by the provider)
      Status(ctx context.Context) (ProviderStatus, error)
      Actions(ctx context.Context) ([]SecurityAction, error) // remediation *proposals*, never executed by the provider
  }
  ```

- `SecurityEvent` is converted to the common `model.Event`
  (`source_type: waf`, ADR-0002); `SecurityAction` (e.g. "block
  198.51.100.7 for 1h, reason …") is executed only through the blocklist
  path (ADR-0008) and therefore only under its allowlists, caps, audit and
  enforcement mode.
- Inputs are logs or admin/metrics APIs the product already exposes
  (e.g. Coraza/ModSecurity audit logs, proxy access logs, CrowdSec AppSec
  alerts via LAPI). Sentinel never inserts itself in the request path,
  never edits proxy configuration in Phase 19, and treats all parsed fields as
  untrusted (size limits, control-character stripping).
- Health and configuration checks reuse the HTTP monitor and future `log`
  monitor rather than new code paths.
- Correlation (security events ↔ services ↔ containers) uses event
  attributes (`client_ip`, `service`, `container_id`) and the correlation
  ID; it is a consumer of the bus, not part of any provider.

## Consequences

- Each WAF/proxy adapter is small and read-only; remediation stays in one
  audited place.
- Exact provider list and parsing formats are decided per adapter in Phase 19.
