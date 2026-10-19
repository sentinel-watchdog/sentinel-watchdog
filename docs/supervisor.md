# Supervisor

The supervisor watches services and tells you when one fails and when it
comes back. Today it checks **HTTP endpoints** (Phase 3a). Processes and
recovery arrive in Phase 3b, and systemd units, cron jobs and logs in 3c.
Until then a `process` or `systemd` service, or any job, is refused as
"planned but not implemented".

Related: [configuration](configuration.md) (the central file and module
directories), [notifications](notifications.md) (channels, payload,
routing), [operations](operations.md) (running sentineld). Decisions:
D-080, D-081, D-082.

## Enabling it

In the central file:

```yaml
modules:
  supervisor:
    enabled: true            # the block holds nothing else (ADR-0015 rule 1)
```

The services live in `<config dir>/supervisor/*.yaml`, usually
`/etc/sentinel/supervisor/`. Each file needs `version: 1`, and the files
are read in lexical order. If the directory or the services are missing,
the supervisor starts empty and logs a warning.

```sh
sentinelctl validate          # checks the files and reads every CA bundle
sentinelctl reload            # applies them to a running sentineld
```

A complete example is in
[configs/supervisor/http.yaml](../configs/supervisor/http.yaml)
(`examples/supervisor/http.yaml` in the tarball).

## Configuration

### Settings

`settings` may appear in at most one file. `max_concurrent_checks` is
module-wide; `check_interval`, `timeout` and `failure_policy` are
defaults that any service can override, key by key.

```yaml
version: 1
settings:                      # all optional; defaults shown
  max_concurrent_checks: 16    # 1-256 checks at once
  check_interval: 30s          # 1s-24h, from the end of one check to the next
  timeout: 10s                 # 100ms-5m; past it the check fails with "timeout"
  failure_policy:
    consecutive_failures: 3    # 1-100 failures make the service failed
    consecutive_successes: 1   # 1-100 successes make a failed one healthy
```

### HTTP services

A service needs only `name` and `url`. Everything else is optional:

```yaml
services:
  - name: web                  # lower-case letters, digits, . _ - (D-011); unique
    url: https://web.example.org/health

  - name: billing
    url: https://billing.internal:8443/v1/ping
    method: POST               # GET (default), HEAD, POST, PUT
    headers:                   # at most 32, names case-insensitive
      Authorization: "Bearer ${BILLING_TOKEN}"
    body: '{"probe": true}'    # POST and PUT only, at most 64 KiB
    success_status_codes: [200, 204]   # default: any 2xx
    body_contains: '"ok"'      # searched in the first 1 MiB of the body
    follow_redirects: false    # default (D-019)
    address: 10.0.0.5          # connect here instead of resolving the host
    tls:                       # https only
      ca_file: /etc/sentinel/ca/internal.pem
      server_name: billing.internal
      min_version: "1.3"       # "1.2" (default) or "1.3"
      insecure_skip_verify: false
    check_interval: 10s        # check_interval, timeout and failure_policy
                               # override the settings
    timeout: 3s
    failure_policy: {consecutive_failures: 2}
    notifications: [ops]       # or {channels: [ops], events: [service_failed]}
```

What each key does:

| Key | Notes |
|---|---|
| `url` | `http` or `https`, with a host. At most 2048 bytes, no `#fragment`. Credentials in the URL (`https://user:pass@host/`) are sent as basic auth. |
| `headers` | Values may come from the environment (`${VAR}`). Control characters other than tab are refused. `Host` is refused: use `address`. `Content-Length`, `Transfer-Encoding` and `Connection` are set by the client. Without a `User-Agent`, the check sends `sentinel-watchdog`. |
| `success_status_codes` | 200–599. A 3xx is accepted only without `follow_redirects`, to check that a redirect happens (for example http → https). |
| `body_contains` | Exact bytes, at most 4 KiB, not with `HEAD`. The body is read only when this key is set. |
| `follow_redirects` | Off by default. When on, only redirects to the same scheme, host and port are followed, for at most 5 hops. Anything else fails with `redirect_refused` or `too_many_redirects`, so headers and a replayed body never leave the origin. |
| `address` | An IP address, optionally with a port (`10.0.0.5`, `10.0.0.5:8443`, `[2001:db8::1]:8443`). Without a port, the URL's port is used. The check connects there without DNS, but the `Host` header, SNI, certificate verification and the redirect rule all follow `url`. Use it to check one node behind a load balancer, or a virtual host on a known IP. |
| `tls.ca_file` | An absolute path to a PEM bundle, used instead of the system roots. It is read at start, at reload and by `validate`. The bundle, its directory and its parents must be owned by root or the sentineld user and not writable by group or others. A link may only name a file in the same directory. A rotated bundle with an unchanged configuration needs a restart. |
| `tls.insecure_skip_verify` | Disables certificate verification. It is accepted with a warning: use it only for lab devices. |

Limits: at most 256 services. Keys that are not listed are errors.
`recovery` is refused on http services, since there is nothing to restart
(D-012).

Every problem is reported at once with its file, key path and line.
Problems never quote a value, because a URL, header or body may carry a
token.

### What a check does

- It sends one HTTP/1.1 request on a new connection. Connections are
  never reused.
- The proxy environment (`HTTP_PROXY`, `HTTPS_PROXY`) is ignored.
- It waits at most `timeout`. Past it, the check fails with `timeout` and
  the connection is closed.
- At most `max_concurrent_checks` checks run at once. A check that waits
  for a slot does not count that wait in its timeout.

## States and events

A service is `unknown` until its first successful check, or until its
failures reach `consecutive_failures`. After that it is `healthy` or
`failed`:

- after `consecutive_failures` failed checks in a row it becomes `failed`
  and emits **`service_failed`** (severity error);
- after `consecutive_successes` successful checks in a row it becomes
  `healthy` again and emits **`service_recovered`** (severity info). Its
  `correlation_id` is the failed event's id, which identifies the
  incident.

The first success of an `unknown` service emits nothing. Stopping or
reloading sentineld is never counted as a failure.

| Event | Attributes |
|---|---|
| `service_failed` | `failure` (the code below), `consecutive_failures`, `endpoint` |
| `service_recovered` | `consecutive_successes`, `downtime_seconds`, `endpoint` |

`endpoint` is the URL's scheme, host and port only: the path and the
query may hold a token. Events go to the channels in the service's
`notifications`. Without a route, they are only logged.

### Failure codes

A failed check is recorded as a fixed code, never as error text:

| Code | Meaning |
|---|---|
| `timeout` | no answer within `timeout` |
| `dns_failed` | the URL's host did not resolve |
| `connect_failed` | no connection: refused, unreachable, or the dial timed out |
| `tls_failed` | handshake or certificate verification failed, or the server spoke plain HTTP |
| `request_failed` | any other transport failure, such as oversized response headers (over 64 KiB) |
| `status_mismatch` | a status code not in `success_status_codes` |
| `body_mismatch` | `body_contains` was not found in the first 1 MiB |
| `body_read_failed` | the body could not be read |
| `redirect_refused` | a redirect to another scheme, host or port |
| `too_many_redirects` | more than 5 redirects |

The loop adds `check_failed`, `panic` and `invalid_result` for checker
faults. They are internal errors and should never appear.

## State across restarts

Each service's health, counters and open incident are kept in
`<state_dir>/supervisor/state.json` (root-only, `0600`). A restart
therefore neither repeats a `service_failed` nor forgets an incident.

The file is written on every transition and at most every 60 s
otherwise. It holds no URL, header or body, only a digest of each
service's definition. When the definition changes (URL, request keys,
TLS, `address`, timeout or failure policy), the service starts again
from `unknown`. If it was failed, its incident is then closed without a
`service_recovered`. Changing only `check_interval` or `notifications`
keeps the state.

If the state file cannot be written, supervision goes on, and
`sentinelctl supervisor status` shows `state file: NOT SAVED`.

## Commands

Both commands need the `read` tier (members of `daemon.socket_group`):

```sh
sentinelctl supervisor list             # one line per service
sentinelctl supervisor status           # details of every service
sentinelctl supervisor status web       # details of one
sentinelctl supervisor status web -json # the same for scripts
```

```text
SERVICE  TYPE  STATE    SINCE
api      http  failed   2026-10-19T13:17:47Z
web      http  healthy  2026-10-19T09:02:11Z
```

`status` also shows:
- the endpoint (scheme, host, port);
- the interval and timeout;
- the last check, how long it took and how long it waited for a slot;
- the last failure code and the counters;
- the open incident;
- `waiting`: all `max_concurrent_checks` slots are busy. This is
  capacity, not a fault of the endpoint;
- `stalled`: a timed-out check has not returned yet, and no new check
  starts until it does.

If the module is not enabled or not running, the commands answer
`unavailable`.

## Troubleshooting

| Symptom | Cause |
|---|---|
| `tls.ca_file is on an unsafe path` | The bundle, its directory or a parent is writable by group or others, or not owned by root or the sentineld user, or a link leaves its directory. Fix the modes and owners. |
| `tls_failed` with an internal CA | Set `tls.ca_file`. If the certificate names another host than the URL, set `tls.server_name`. |
| `tls_failed` with `address` | The certificate is checked against the URL's host: the URL must use the name in the certificate, not the IP. |
| `redirect_refused` | The endpoint redirects to another host (often a login page). Check the redirect itself instead: no `follow_redirects`, and its 3xx in `success_status_codes`. |
| `status_mismatch` on a 3xx | Redirects are not followed by default: add the code, or set `follow_redirects`. |
| Many services show `waiting` | Raise `max_concurrent_checks`, lower the timeouts, or spread the intervals. |
