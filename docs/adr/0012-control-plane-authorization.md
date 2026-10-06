# ADR-0012: Control-plane authorization tiers

- Status: Accepted (resolves Q-002)
- Date: 2026-10-06
- Phases: 5 (`tier` field in the protocol), 8 (enforcement)

## Context

`sentinelctl` talks to `sentineld` over one Unix socket (mode `0660`,
group configurable). With firewall and Kubernetes enforcement, a socket
peer able to call `firewall apply` controls host reachability, which is
root-equivalent. Socket file permissions alone grant all-or-nothing access.

## Decision

Each request is authorised from the peer credentials (`SO_PEERCRED`, read
with the `syscall` package — no new dependency) against three tiers:

| Tier | Who (defaults) | Commands |
|---|---|---|
| `read` | any peer allowed to connect (socket mode/group) | `status`, `list`, `events`, `logs`, `config show` (redacted), `<domain> status|capabilities|list|policies|decisions`, `firewall plan|diff|validate`, `kubernetes plan` |
| `operate` | uid 0, or members of `settings.operator_group` (optional) | supervisor `start|stop|restart|enable|disable|reset`, `reload` |
| `admin` | uid 0 only, or members of `settings.admin_group` (optional, documented as root-equivalent) | `firewall apply|confirm|rollback`, blocklist manual `block|unblock`, `kubernetes apply` (Phase 20) |

- The tier is a property of the protocol command in `pkg/api`, not of the
  CLI. The server re-validates everything; the CLI is untrusted.
- Every `operate`/`admin` request — accepted or denied — is written to the
  audit log with uid, gid and pid.
- Destructive requests carry `plan_id`, `fingerprint`, `dry_run` and
  `confirmed`; the server refuses an apply without a fresh plan
  fingerprint even if the CLI skipped its prompt.
- `--dry-run` on any destructive command forces effective mode `dry_run`
  for that request (it can only lower, never raise, the configured mode).
- A future second socket (read-only, world-connectable) remains possible
  but is not planned.

## Consequences

- Q-002 is closed: group membership grants `read`; enforcement needs root
  unless the operator explicitly delegates with `admin_group`.
- `pkg/api` gains a `tier` per command and the structured error
  `permission_denied` planned for the socket protocol (Phase 5).
