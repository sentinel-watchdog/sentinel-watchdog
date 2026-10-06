# Configuration reference

Sentinel is configured in YAML only. This page documents schema
`version: 1`. Everything described here is parsed and validated by the
current code; behaviour that depends on runtime components not yet built
is marked **(runtime: Phase N)** — see [PLAN.md](../PLAN.md).

- [Files and load order](#files-and-load-order)
- [Strictness](#strictness)
- [Environment variables and secrets](#environment-variables-and-secrets)
- [Value formats](#value-formats)
- [settings](#settings)
- [notifications](#notifications)
- [monitors — common fields](#monitors--common-fields)
- [systemd](#type-systemd) · [process](#type-process) · [http](#type-http) · [cron](#type-cron)
- [Shared blocks](#shared-blocks): exec, output, recovery, failure_policy, limits, tls
- [Planned types](#planned-types)

## Files and load order

| Path | Purpose |
|---|---|
| `/etc/sentinel/sentinel.yaml` | Main file (required). |
| `/etc/sentinel/conf.d/*.yaml`, `*.yml` | Optional fragments. |

1. The main file is read first.
2. Fragments are read in lexical byte order of their file names
   (`10-web.yaml` before `20-db.yaml`). Use numeric prefixes.
3. Hidden files (`.name.yaml`), other extensions (`.bak`, `.yaml~`,
   `.rpmnew`, `.dpkg-dist`) and non-regular files are ignored. Symlinks to
   regular files are followed.
4. A missing `conf.d/` directory is not an error.
5. By default `conf.d/` is resolved next to the main file.

Every file must contain `version: 1`. Fragments may contain only
`notifications` and `monitors`; `settings` is allowed only in the main file.
Fragments **add** entries — there is no override or merge of entries with
the same name: a duplicate monitor or channel name anywhere is an error
that names both files.

Limits: 4 MiB per file, exactly one YAML document per file (`---`
separating a second document is an error).

## Strictness

The loader rejects, with file and line number:

- unknown keys at any level (typos such as `max_attemps`, keys belonging
  to another monitor type, the draft keys `interval` and `on`);
- duplicate keys in the same mapping;
- wrong value types (`max_attempts: many`, `check_interval: 15`);
- unknown or not-yet-implemented monitor and notification types;
- unknown enum values (backoff, actions, output types, policies...).

All problems are collected and reported together, for example:

```
invalid configuration (2 problem(s)):
  - /etc/sentinel/conf.d/20-db.yaml: line 7: unknown field "max_attemps" (valid fields: action, backoff, ...)
  - monitors[api].url: scheme must be http or https
```

Some legal but risky settings produce **warnings** instead (plain-HTTP
webhook, `insecure_skip_verify: true`, a disabled channel referenced by a
monitor, an HTTP timeout not shorter than the check interval).

## Environment variables and secrets

Scalar **values** may reference environment variables as `${NAME}`:

```yaml
url: ${SENTINEL_WEBHOOK_URL}
headers:
  Authorization: Bearer ${SENTINEL_WEBHOOK_TOKEN}
```

Rules:

- `NAME` must match `[A-Za-z_][A-Za-z0-9_]*`.
- An undefined variable is an error (an empty but defined variable is
  allowed). There is no `${NAME:-default}` syntax.
- `$${` produces a literal `${`. A `$` not followed by `{` is kept as-is.
- Keys are never expanded. Expansion happens after YAML parsing, so a
  variable value can never change the document structure.
- Expansion is not recursive.
- An unquoted value is re-typed after expansion (`max_attempts: ${N}`
  becomes an integer). Quote it to force a string: `"${N}"`.

**Secrets** belong in the environment, never in YAML. Under systemd, use a
root-only environment file referenced by a drop-in:

```ini
# /etc/systemd/system/sentineld.service.d/secrets.conf
[Service]
EnvironmentFile=/etc/sentinel/secrets.env   # mode 0600, owner root
```

`sentinelctl config show` (Phase 5) prints a redacted view: webhook URLs
are reduced to scheme and host, header values other than `Content-Type`,
`Accept`, `User-Agent`, `Cache-Control` and `Accept-*` are masked, URL
passwords and query values are masked, HTTP monitor bodies are masked and
environment variables with secret-looking names (`*PASSWORD*`, `*TOKEN*`,
`*SECRET*`, `*KEY*`...) are masked. `script`, `args` and other fields are
printed as written — do not put secrets in them.

## Value formats

| Kind | Format | Examples |
|---|---|---|
| Duration | Go duration string. Integers are rejected. | `500ms`, `15s`, `10m`, `2h`, `1h30m` |
| Size | Integer bytes or number + unit. `KB/MB/GB/TB` = powers of 1000, `KiB/MiB/GiB/TiB` = powers of 1024. | `1073741824`, `512MiB`, `1GB` |
| File mode | **Quoted** octal string. | `"0660"`, `"0640"` |
| Name | `^[a-z0-9][a-z0-9._-]{0,62}$` | `api-health`, `db.primary` |
| Path | Absolute and clean (no `..`, `.`, `//`, trailing `/`). | `/var/log/app/out.log` |
| User / group | Account name or numeric id. | `myapp`, `1001` |

An omitted numeric or duration field — or one set to `0` — takes its
default. Explicit invalid values (negative, out of range) are rejected.

## settings

Main file only. All keys are optional.

| Key | Default | Validation / notes |
|---|---|---|
| `state_file` | `/var/lib/sentinel/state.json` | absolute path |
| `socket` | `/run/sentinel/sentinel.sock` | absolute path, ≤ 107 bytes |
| `socket_mode` | `"0660"` | owner must have rw; world-writable rejected |
| `socket_group` | — (daemon's group) | name or gid (runtime: Phase 5) |
| `log_level` | `info` | `debug`, `info`, `warn`, `error` |
| `log_format` | `auto` | `auto`, `text`, `json`, `journal` — see [Logging](architecture.md#logging) |
| `timezone` | `Local` | IANA name (`Europe/Rome`, `UTC`); tz database embedded |
| `default_check_interval` | `15s` | 1s – 24h; default for monitors' `check_interval` |
| `default_command_timeout` | `5m` | 1s – 7 days; default cron `timeout` |
| `shutdown_timeout` | `30s` | 1s – 10m |
| `history_limit` | `50` | 1 – 10000 entries per monitor in the state file |
| `event_limit` | `500` | 1 – 100000 entries in the global event log |
| `daemon_notifications` | `[]` | channels receiving `configuration_error` and `daemon_error` |

## notifications

List of channels. Only `type: webhook` exists in this release; `slack` and
`teams` are reserved and rejected as "planned but not implemented" (a
generic webhook works with both services' incoming-webhook URLs as long as
their payload format is acceptable).

```yaml
notifications:
  - name: main-webhook
    type: webhook
    enabled: true
    url: ${SENTINEL_WEBHOOK_URL}
    method: POST
    timeout: 10s
    headers:
      Authorization: Bearer ${SENTINEL_WEBHOOK_TOKEN}
    retry:
      attempts: 3
      delay: 5s
      backoff: exponential
      max_delay: 1m
    success_status_codes: [200, 202, 204]
    tls:
      ca_file: /etc/pki/internal-ca.pem
```

| Key | Default | Validation / notes |
|---|---|---|
| `name` | required | name format, unique across all files |
| `type` | required | `webhook` |
| `enabled` | `true` | |
| `url` | required | `http` or `https` with host; `http` warns |
| `method` | `POST` | `POST`, `PUT` |
| `timeout` | `10s` | 100ms – 5m, per attempt |
| `headers` | `Content-Type: application/json` added if absent | RFC 7230 names; values without CR/LF/NUL |
| `retry.attempts` | `3` | 1 – 10, total attempts including the first |
| `retry.delay` | `5s` | 0 – 24h |
| `retry.backoff` | `exponential` | `fixed`, `exponential` |
| `retry.max_delay` | `1m` | ≥ `delay` |
| `success_status_codes` | any 2xx | 100 – 599 |
| `tls` | | see [tls](#tls) |

Payload format and delivery semantics: `docs/notifications.md` (Phase 4).

## monitors — common fields

```yaml
monitors:
  - name: api-health        # required, unique
    type: http              # required: systemd | process | http | cron
    enabled: true           # default true
    description: Public API # optional, free text
    notifications: [main-webhook]
```

### notifications (per monitor)

Short form — list of channel names, default events:

```yaml
notifications: [main-webhook, oncall-webhook]
```

Long form — explicit event filter:

```yaml
notifications:
  channels: [main-webhook]
  events: [recovery_exhausted, monitor_recovered]
```

| Monitor types | Valid events | Default events |
|---|---|---|
| systemd, process, http | `monitor_failed`, `recovery_started`, `recovery_exhausted`, `monitor_recovered` | `monitor_failed`, `recovery_exhausted`, `monitor_recovered` |
| cron | `job_succeeded`, `job_failed`, `job_timeout` | `job_failed`, `job_timeout` |

Channels must exist; listing one twice is an error; `events` without
`channels` is an error. Daemon events are routed with
`settings.daemon_notifications`. Delivery and de-duplication: Phase 4.

## type: systemd

Watches an **existing** systemd service. Sentinel never creates, edits,
enables or disables units; with `recovery.action: restart` it runs
`systemctl restart <service>`.

```yaml
- name: mycustom
  type: systemd
  service: mycustom.service
  check_interval: 15s
  command_timeout: 30s
  journal_lines: 20
  failure_policy:
    consecutive_failures: 1
    recovery_after_successes: 1
  recovery:
    action: restart
    max_attempts: 5
    window: 10m
```

| Key | Default | Validation / notes |
|---|---|---|
| `service` | required | unit name ending in `.service`, `[A-Za-z0-9:_.\@-]`, not starting with `-` |
| `check_interval` | `settings.default_check_interval` | 1s – 24h |
| `command_timeout` | `30s` | timeout for each `systemctl`/`journalctl` call, 1s – 5m |
| `journal_lines` | `20` | 0 – 1000 lines attached to failure events; `0` disables |
| `failure_policy` | `1` / `1` | see [failure_policy](#failure_policy) |
| `recovery` | none (disabled) | see [recovery](#recovery) |

On hosts without systemd (Alpine/OpenRC) the configuration is still
valid; the monitor reports `unavailable` at runtime (Phase 7).

## type: process

Runs and supervises a long-lived foreground process.

```yaml
- name: worker
  type: process
  command: /opt/myapp/bin/worker
  args: [--config, /etc/myapp/worker.yaml]
  user: myapp
  group: myapp
  working_directory: /opt/myapp
  environment:
    APP_ENV: production
  stdout: {type: file, path: /var/log/myapp/worker.log}
  stderr: {type: log}
  health:
    startup_grace_period: 30s
    check_interval: 5s
    stop_signal: SIGTERM
    stop_timeout: 30s
  limits:
    max_cpu_percent: 80
    max_memory_bytes: 1GiB
    sustained_for: 1m
    action: restart
  recovery:
    action: restart
```

Accepts every [exec](#exec) key plus:

| Key | Default | Validation / notes |
|---|---|---|
| `health.startup_grace_period` | `10s` | 0 – 1h; exits inside it are failed starts |
| `health.check_interval` | `settings.default_check_interval` | resource sampling interval, 1s – 24h |
| `health.stop_signal` | `SIGTERM` | `SIGTERM`, `SIGINT`, `SIGQUIT`, `SIGHUP`, `SIGUSR1`, `SIGUSR2` (`TERM` and `sigterm` accepted) |
| `health.stop_timeout` | `30s` | 1s – 1h, then SIGKILL to the process group |
| `limits` | none | see [limits](#limits) |
| `recovery` | none (disabled) | see [recovery](#recovery) |

## type: http

Probes an HTTP(S) endpoint.

```yaml
- name: api-health
  type: http
  url: https://example.org/health
  method: GET
  timeout: 10s
  check_interval: 30s
  follow_redirects: false
  headers:
    Accept: application/json
  expect:
    status_codes: [200]
    body_contains: healthy
    body_max_bytes: 1MiB
  failure_policy:
    consecutive_failures: 3
    recovery_after_successes: 2
```

| Key | Default | Validation / notes |
|---|---|---|
| `url` | required | `http`/`https` with host |
| `method` | `GET` | `GET HEAD POST PUT PATCH DELETE OPTIONS` |
| `headers` | — | as for notifications |
| `body` | — | request body |
| `timeout` | `10s` | 100ms – 5m; warning if ≥ `check_interval` |
| `check_interval` | `settings.default_check_interval` | 1s – 24h (`interval` is **not** accepted) |
| `follow_redirects` | `false` | a 3xx is then evaluated against `expect.status_codes` |
| `max_redirects` | `10` | 1 – 50 when following |
| `tls` | verification on | see [tls](#tls) |
| `expect.status_codes` | any 2xx | 100 – 599 |
| `expect.body_contains` | — | plain substring |
| `expect.body_max_bytes` | `1MiB` | ≤ 64MiB; at most this much body is read |
| `failure_policy` | `3` / `1` | see [failure_policy](#failure_policy) |

HTTP monitors have no `recovery` block in this release (nothing to restart
until the `execute` action exists).

## type: cron

Runs a command on a traditional cron schedule.

```yaml
- name: nightly-backup
  type: cron
  schedule: "0 2 * * *"
  timezone: Europe/Rome
  command: /usr/local/bin/backup.sh
  user: backup
  timeout: 2h
  concurrency_policy: forbid
  missed_runs: skip
  notifications:
    channels: [main-webhook]
    events: [job_succeeded, job_failed, job_timeout]
```

Accepts every [exec](#exec) key plus:

| Key | Default | Validation / notes |
|---|---|---|
| `schedule` | required | 5 fields `minute hour day-of-month month day-of-week`, or `@yearly @annually @monthly @weekly @daily @midnight @hourly` |
| `timezone` | `settings.timezone` | IANA name |
| `timeout` | `settings.default_command_timeout` | 1s – 7 days |
| `concurrency_policy` | `forbid` | `forbid` (skip the new run), `allow`, `replace` (stop the old run) |
| `missed_runs` | `skip` | `skip`, `run_once` (one catch-up run after downtime) |
| `stop_timeout` | `30s` | SIGTERM → SIGKILL grace on timeout |

Cron syntax: `*`, numbers, ranges `1-5`, lists `1,3,5`, steps `*/15`,
`0-30/10`, `5/10` (= `5-59/10`), month names `jan`–`dec`, weekday names
`sun`–`sat`, weekday `7` = Sunday. When both day-of-month and day-of-week
are restricted, a day matches if **either** matches (Vixie cron rule).
Not supported: `@reboot`, seconds, `L`, `W`, `#`, `?`, `every 5m`.

## Shared blocks

### exec

Used by `process` and `cron`. Exactly one of `command` or `script`:

| Key | Notes |
|---|---|
| `command` | Absolute path, executed directly (`execve`). No shell, no `$PATH` lookup. |
| `args` | Arguments for `command`; not allowed with `script`. |
| `script` | Shell text, run as `<shell> -c <script>`. Use only when shell features are needed. |
| `shell` | Absolute path, default `/bin/sh`; only valid with `script`. |
| `user`, `group` | Run as this account (requires sentineld to run as root). |
| `working_directory` | Absolute path. |
| `environment` | Extra variables; names `[A-Za-z_][A-Za-z0-9_]*`. |
| `stdout`, `stderr` | See [output](#output). |

### output

| `type` | Behaviour |
|---|---|
| `log` (default) | Each line is logged by sentineld with the monitor name (→ journald under systemd). |
| `file` | Appended to `path` (absolute, required), created with `mode` (default `"0640"`, world-writable rejected). Rotation is left to logrotate (`copytruncate` or a reopen signal: Phase 6). |
| `discard` | Dropped. Must be chosen explicitly. |

`stdout` and `stderr` may not point to the same file.

### recovery

Allowed on `systemd` and `process`. A present block is enabled unless
`enabled: false` or `action: none`.

| Key | Default | Validation / notes |
|---|---|---|
| `enabled` | `true` | |
| `action` | `restart` | `none`, `restart` (`execute` is planned) |
| `max_attempts` | `5` | 1 – 1000 attempts per `window` |
| `window` | `10m` | 1s – 30 days |
| `delay` | `5s` | 0 – 24h before the first attempt (an explicit `0s` also means the default) |
| `backoff` | `exponential` | `fixed`, `exponential` (delay doubles per attempt) |
| `max_delay` | `5m` | ≥ `delay` |
| `stable_after` | `15m` | healthy for this long resets the attempt counter |
| `cooldown` | `0` | 0 = stay `exhausted` until `sentinelctl reset`; otherwise retry automatically after this period |

Engine semantics: Phase 4.

### failure_policy

Allowed on `systemd` and `http`.

| Key | Notes |
|---|---|
| `consecutive_failures` | failed checks needed to enter `failed` (1 – 1000) |
| `recovery_after_successes` | successful checks needed to leave `failed` (1 – 1000) |

### limits

Allowed on `process`. **Modelled and validated only; enforcement by
sampling `/proc` arrives in Phase 6.** Until then a configured limit is
reported as `unsupported` and never silently assumed to work.

| Key | Default | Notes |
|---|---|---|
| `max_cpu_percent` | — | relative to one CPU (200 = two cores) |
| `max_memory_bytes` | — | resident memory; size format |
| `sustained_for` | `30s` | must be exceeded continuously this long |
| `action` | `log` | `log`, `notify`, `restart`, `stop`, `kill` |

At least one of `max_cpu_percent` / `max_memory_bytes` is required.

### tls

| Key | Notes |
|---|---|
| `ca_file` | PEM bundle (absolute path) used instead of system roots |
| `server_name` | SNI / verification name override |
| `insecure_skip_verify` | `false` by default; `true` produces a warning |

## Planned types

These monitor types are reserved. Using them is a configuration error that
says so explicitly:

`port`, `mount`, `resource`, `log`, `openrc`, `process_group`.

Notification types `slack` and `teams` are reserved likewise.
