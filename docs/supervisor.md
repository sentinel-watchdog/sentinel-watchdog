# Supervisor

The supervisor watches services, reports when one fails and when it comes
back, and starts some of them again. It handles four kinds of entry:

- HTTP services: an endpoint checked at an interval.
- Process services: a program sentineld starts, watches, restarts within
  a budget and stops; a memory limit can stop it.
- systemd services: an existing unit observed, reported when it fails,
  stays in transition or keeps being restarted by systemd, and restarted
  by sentineld when you arm it.
- Jobs: a program run on a cron schedule in a time zone, to its end or
  its timeout.

Operators can stop, start, restart, disable, enable or reset a service and
read the recent logs of a service or job. Its decisions are D-080 to
D-082, D-084, D-086 to D-090 and D-092 to D-100 in the
[decision log](decisions.md). Related: [configuration](configuration.md)
(the central file and module directories), [notifications](notifications.md)
(channels, payload, routing), [operations](operations.md) (running
sentineld).

## Enabling it

In the central file:

```yaml
modules:
  supervisor:
    enabled: true
    processes: true          # run process services and jobs; default false
    systemd_restart: true    # restart systemd services with recovery; default false
```

The two switches are the block's only other keys (ADR-0015 rule 1).
Running programs as root and restarting units are armed in the central
file, never by a file added to the module directory (D-084 (1), D-095).

- Without `processes`, process services and jobs are validated and shown
  as `not_armed` but never started, and validation warns about them.
- Without `systemd_restart`, systemd services are observed only; see
  [Restarting units](#restarting-units).

## Files and common rules

Services and jobs live in `<config dir>/supervisor/*.yaml`, usually
`/etc/sentinel/supervisor/`. Each file needs `version: 1`, and the files
are read in lexical order. If the directory or the entries are missing,
the supervisor starts empty and logs a warning.

```sh
sentinelctl validate          # checks the files and reads every CA bundle
sentinelctl reload            # applies them to a running sentineld
```

Complete examples are in
[configs/supervisor/http.yaml](../configs/supervisor/http.yaml),
[configs/supervisor/process.yaml](../configs/supervisor/process.yaml),
[configs/supervisor/systemd.yaml](../configs/supervisor/systemd.yaml) and
[configs/supervisor/jobs.yaml](../configs/supervisor/jobs.yaml)
(`examples/supervisor/` in the tarball).

Rules for every entry:

- `services:` and `jobs:` hold the entries. They share one name space and
  a limit of 256 entries.
- `name`: 1 to 63 characters, lower-case letters, digits, `.`, `_` and
  `-`, starting with a letter or digit (D-011); unique.
- `type`: inferred from the keys (`url` for `http`, `command` or `script`
  for `process`, `unit` for `systemd`; a job is `cron`). An entry with keys
  of two types is an error.
- `enabled`: `true` by default. `false` keeps the entry validated but
  never checked, run or scheduled, whatever an operator asks (D-020); its
  state is kept.
- `notifications`: the channels its events go to, as a list (`[ops]`) or
  with the events chosen (`{channels: [ops], events: [service_failed]}`).
  A job chooses among `job_succeeded`, `job_failed` and `job_timeout`. A
  systemd service's route cannot choose `service_flapping`: a route
  without `events` gets it.
  Without a route, events are only logged. Either way the last ones are
  shown by `sentinelctl events`.
- Keys not listed for a type are errors; some are refused with a reason.
- Every problem is reported at once with its file, key path and line.
  Problems never quote a value, because a URL, header, body or schedule
  may carry a token.

### Settings

`settings` may appear in at most one file. `max_concurrent_checks` and
`max_concurrent_runs` are module-wide; `check_interval`, `timeout` and
`failure_policy` are defaults that an HTTP or systemd service can
override, key by key.

```yaml
version: 1
settings:                      # all optional; defaults shown
  max_concurrent_checks: 16    # 1-256 checks at once
  max_concurrent_runs: 32      # 1-256 runs of jobs under concurrency_policy: allow
  check_interval: 30s          # 1s-24h, from the end of one check to the next
  timeout: 10s                 # 100ms-5m; past it the check fails with "timeout"
  failure_policy:
    consecutive_failures: 3    # 1-100 failures make the service failed
    consecutive_successes: 1   # 1-100 successes make a failed one healthy
```

## How supervision works

### Checks and failure policy

HTTP and systemd services are checked every `check_interval`, each check
bounded by `timeout`. A failed check is recorded as a fixed code
([Failure codes](#failure-codes)). At most
`max_concurrent_checks` checks run at once; a check waiting for a slot
does not count that wait in its timeout.

A service is `unknown` until its first successful check, or until its
failures reach `consecutive_failures`. After that it is `healthy` or
`failed`:

- after `consecutive_failures` failed checks in a row it becomes `failed`
  and emits `service_failed`;
- after `consecutive_successes` successful checks in a row it becomes
  `healthy` again and emits `service_recovered`.

The first success of an `unknown` service emits nothing. Stopping or
reloading sentineld is never counted as a failure.

A process service has no checks. It is `unknown` until it has run for
`startup_grace`, then `healthy`. Every exit sentineld did not ask for is a
failure, exit status 0 included.

### Incidents

A `service_failed` opens an incident; its event id identifies it. Every
later event of the incident (`service_restarted`,
`service_recovery_exhausted`, `service_recovered`) carries that id as
`correlation_id`. While an incident is open, a later failure adds no
`service_failed`. A checked service closes it after
`consecutive_successes`; a process service after running for
`stable_after`; a systemd service with `recovery` needs both.

### Recovery

Recovery starts a failed service again within a budget (D-013, D-014,
D-087). Process services always have it: the defaults below when
`recovery` is absent, behind `modules.supervisor.processes`. A systemd
service has it only with a `recovery` mapping and
`modules.supervisor.systemd_restart` ([Restarting
units](#restarting-units)). HTTP services and jobs refuse `recovery`:
there is nothing to restart, and a failed run is reported while the next
slot runs as usual (D-012).

| Key | Default | Rules |
|---|---|---|
| `action` | `restart` | the only action |
| `max_attempts` | `5` | 1 to 100 attempts in `window` |
| `window` | `10m` | 1m to 24h |
| `delay` | `1s` | 100ms to 10m, before the first attempt |
| `backoff` | `exponential` | `fixed` (always `delay`) or `exponential` (doubled each attempt) |
| `max_delay` | `1m`, or `delay` if longer | `delay` to 1h; with `exponential` only |
| `stable_after` | `1m`, or `startup_grace` if longer | 1s to 24h, not shorter than `startup_grace` |
| `cooldown` | `10m` | 1m to 7d, or `0`: exhausted until `sentinelctl supervisor reset` (D-013) |
| `restart_timeout` | `2m` | systemd services only; see [Restarting units](#restarting-units) |

- An attempt is a launch made while the service has an open incident:
  after a crash, a failed start, or a restart or reload of sentineld that
  finds the incident still open. The first launch of a healthy service is
  not one. A process service emits `service_restarted` at each attempt's
  launch, a systemd service when its attempt succeeds.
- Budget: at most `max_attempts` attempts in any `window`. Attempt *n*
  waits `delay` (fixed) or `delay × 2^(n−1)` up to `max_delay`. When the
  window is full the service is exhausted: `service_recovery_exhausted`,
  once per exhaustion, and no launch until `cooldown` has passed; then the
  budget starts again with an immediate attempt. With `cooldown: 0` only
  `sentinelctl supervisor reset` renews it.
- Stability: a service that runs for `stable_after` closes its incident
  (`service_recovered`). That does not free its attempts; the window does.
  A program that crashes every few minutes is therefore still stopped
  after `max_attempts` crashes in the window.
- Persistence: each attempt is saved in the state file before the launch,
  so neither a restart of sentineld nor a reload resets the budget.
  Changing the policy keeps it.
- Audit: every launch sentineld makes by itself is recorded in the audit
  log before it happens and its outcome after (ADR-0011): action
  `supervisor.start` (the first launch) or `supervisor.restart` (an
  attempt), actor `daemon`, the service as target, `trigger`, `attempt`
  and `max_attempts`, the incident as correlation id. The cleanup of a
  group a previous run left is recorded as `supervisor.cleanup` when the
  log takes records, and never waits for it. Never the program, its
  arguments or its environment.
- Blocked: if the intent cannot be recorded (the audit log is broken) or
  the attempt cannot be saved (the state directory is not writable),
  nothing is launched. Status shows `blocked` with `audit_unavailable` or
  `state_unavailable`, and the launch is tried again every `max_delay`. A
  refused intent uses no attempt. A broken audit log refuses every record
  until sentineld restarts with the evidence restored, so until then no
  process service starts, no unit is restarted and no job runs.
- Corrupt state file: it is moved aside. Processes still start and units
  are still observed, but the budgets of services with recovery count as
  spent for one `window` (the first crash
  exhausts), and sentineld publishes `daemon_error` (`state_corrupt`),
  shown in status (D-084 (3)).
- Clock jumps: a clock set back never frees budget early; a clock set
  forward frees it early, by at most `max_attempts` attempts per window of
  wall time.

### What each type supports

| | HTTP | Process | systemd | Job |
|---|---|---|---|---|
| Central switch | none | `processes` | `systemd_restart`, for restarts | `processes` |
| `check_interval`, `failure_policy` | yes | refused | yes | refused |
| `timeout` | bounds a check | refused | bounds one read of the unit | bounds a run |
| `recovery` | refused | yes (default policy) | optional | refused |
| `limits.memory`, `startup_grace` | refused | yes | refused | refused |
| Operator commands | `disable`, `enable` | all six | `restart`, `disable`, `enable`, `reset` | `disable`, `enable` |
| `supervisor logs` | `unsupported` | its output | the unit's journal | its output |

## HTTP services

A service needs only `name` and `url`. Everything else is optional:

```yaml
version: 1
services:
  - name: web
    url: https://web.example.org/health

  - name: billing
    url: https://billing.internal:8443/v1/ping
    method: POST
    headers:
      Authorization: "Bearer ${BILLING_TOKEN}"
    body: '{"probe": true}'
    success_status_codes: [200, 204]
    body_contains: '"ok"'
    follow_redirects: false
    address: 10.0.0.5
    tls:
      ca_file: /etc/sentinel/ca/internal.pem
      server_name: billing.internal
      min_version: "1.3"
      insecure_skip_verify: false
    check_interval: 10s
    timeout: 3s
    failure_policy: {consecutive_failures: 2}
    notifications: [ops]
```

| Key | Notes |
|---|---|
| `url` | `http` or `https`, with a host. At most 2048 bytes, no `#fragment`. Credentials in the URL (`https://user:pass@host/`) are sent as basic auth. |
| `method` | `GET` (default), `HEAD`, `POST` or `PUT`. |
| `headers` | At most 32; names are case-insensitive and given once; values at most 8 KiB. Values may come from the environment (`${VAR}`). Control characters other than tab are refused. `Host` is refused: use `address`. `Content-Length`, `Transfer-Encoding` and `Connection` are set by the client. Without a `User-Agent`, the check sends `sentinel-watchdog`. |
| `body` | The request body, with `POST` and `PUT` only, at most 64 KiB. |
| `success_status_codes` | A non-empty list of 200 to 599; default any 2xx. A 3xx is accepted only without `follow_redirects`, to check that a redirect happens (for example http to https). |
| `body_contains` | Exact bytes, not empty, at most 4 KiB, not with `HEAD`, searched in the first 1 MiB. The body is read only when this key is set. |
| `follow_redirects` | Off by default (D-019). When on, only redirects to the same scheme, host and port are followed, for at most 5 hops. Anything else fails with `redirect_refused` or `too_many_redirects`, so headers and a replayed body never leave the origin. |
| `address` | A unicast IP address, optionally with a port (`10.0.0.5`, `10.0.0.5:8443`, `[2001:db8::1]:8443`). Without a port, the URL's port is used. The check connects there without DNS, but the `Host` header, SNI, certificate verification (unless `tls.server_name` is set) and the redirect rule all follow `url`. Use it to check one node behind a load balancer, or a virtual host on a known IP. |
| `tls.ca_file` | An absolute path to a PEM bundle (a regular file of at most 1 MiB), used instead of the system roots. It is read at start, at reload and by `validate`. The bundle, its directory and its parents must be owned by root or the sentineld user and not writable by group or others. A link may only name a file in the same directory: name a system bundle by its real file, or omit `ca_file` for the system roots. A rotated bundle with an unchanged configuration needs a restart. |
| `tls.server_name` | The name sent as SNI and checked in the certificate, instead of the URL's host. |
| `tls.min_version` | `"1.2"` (default) or `"1.3"`. |
| `tls.insecure_skip_verify` | Disables certificate verification. It is accepted with a warning: use it only for lab devices. |
| `tls` | `https` URLs only. |
| `check_interval`, `timeout`, `failure_policy` | Override the [settings](#settings). |

### What a check does

- It sends one HTTP/1.1 request on a new connection. Connections are
  never reused.
- The proxy environment (`HTTP_PROXY`, `HTTPS_PROXY`) is ignored.
- It waits at most `timeout`. Past it, the check fails with `timeout` and
  the connection is closed.
- Response headers are bounded (64 KiB), and the body is read only for
  `body_contains`, up to 1 MiB.

## Process services

A process service is a program sentineld starts and owns. `name` and
`command` (or `script`) are enough.

```yaml
version: 1
services:
  - name: worker
    command: /usr/local/bin/worker     # absolute; never looked up in $PATH
    args: [--queue, jobs]
    user: worker                       # name or number; a number needs group
    group: worker
    working_dir: /var/lib/worker
    environment: {QUEUE_URL: "${QUEUE_URL}"}
    output: log                        # log | discard | {file: /var/log/worker.log, mode: "0640"}
    stop_signal: SIGTERM
    stop_timeout: 10s
    startup_grace: 5s
    recovery: {max_attempts: 5, window: 10m, delay: 1s, max_delay: 1m, cooldown: 10m}
    limits: {memory: 512MiB}
    notifications: [ops]
  - name: cleanup
    script: "exec /usr/local/bin/cleanup --loop"   # run by shell -c
    shell: /bin/sh
```

| Key | Default | Rules |
|---|---|---|
| `command` | none | either `command` or `script` (D-005) |
| `args` | none | with `command` only; at most 256, 4 KiB each, 64 KiB in all |
| `script` | none | run as `<shell> -c <script>`; at most 64 KiB |
| `shell` | `/bin/sh` | with `script` only; absolute |
| `user`, `group` | sentineld's | names (from `/etc/passwd` and `/etc/group`, never NSS) or decimal ids; a numeric user needs a group; supplementary groups are always dropped |
| `working_dir` | `/` | absolute; checked when the process starts |
| `environment` | none | the child's whole environment, plus `PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin` unless set; nothing of sentineld's own environment; at most 128 variables, names `[A-Za-z_][A-Za-z0-9_]*` up to 128 bytes, values without NUL up to 32 KiB, the `name=value` entries 128 KiB in all |
| `output` | `log` | `log` (see [Output and its secrets](#output-and-its-secrets)), `discard`, or a `file` opened for appending, created with `mode` (default `"0640"`, never group- or other-writable); `log` and `file` are read back with [`supervisor logs`](#logs) |
| `stop_signal` | `SIGTERM` | `SIGTERM`, `SIGINT`, `SIGQUIT`, `SIGHUP`, `SIGUSR1`, `SIGUSR2` or `SIGKILL` |
| `stop_timeout` | `10s` | 1s to 5m; then `SIGKILL` |
| `startup_grace` | `5s` | 0 to 10m; an exit before it is reported as during startup |
| `recovery` | the defaults | see [Recovery](#recovery) |
| `limits` | none | `memory` only: see [Memory limit](#memory-limit) |
| `notifications`, `enabled` | | see [Files and common rules](#files-and-common-rules) |

Paths (`command`, `shell`, `working_dir`, an output `file`) must be
absolute and clean, without NUL, at most 4096 bytes.
`failure_policy`, `check_interval` and `timeout` belong to checked
services and are refused (D-012).

The program must be trusted (D-086). The file, every directory on its
path and every symbolic link on the way must be owned by root or by
sentineld's user and not writable by group or others (a sticky directory
such as `/tmp` may be passed through, but not hold the program or a link
to it). Otherwise a local user could replace what sentineld runs as root.
It is checked by `sentinelctl validate` and again at every start;
sentineld executes the resolved file, keeping the configured path as
`argv[0]`. What the program itself reads or runs is not covered.

### How it runs

Each process gets its own process group, `/dev/null` as stdin, and on
Linux a parent-death signal: if sentineld dies, the process is killed.
When it is stopped (shutdown, a reload that changes its definition, or an
operator's command), the stop signal goes to the whole group, then
`SIGKILL` after `stop_timeout`. When the program exits by itself, what is
left of its group is killed.

sentineld records each group in its state file. If sentineld itself is
killed (`kill -9`), at its next start, before launching the service again,
it kills what is left in that group. Only processes of the same boot, in
the recorded group and session and not older than the recorded leader are
killed.

Residuals (D-084 (2), D-086): a process that leaves its group or session
(`setsid`) escapes; after a `kill -9` of sentineld outside systemd, the
leftovers run until sentineld starts again; a group and a session both
reused by unrelated programs while sentineld was down would match. Under
systemd, `KillMode=control-group` kills the whole tree.

### Output and its secrets

With `output: log`, each line goes to the journal under systemd, else to
sentineld's log ([Output in the journal](#output-in-the-journal)), at
most 100 lines per second with bursts of 1000 for each stream. A line
longer than 4 KiB is cut there and marked `[line cut]`, before redaction.

Lines sent to the log are masked with the configuration's secrets
(`${VARIABLE}` values, notification URLs and headers) and with every value
of the `environment` of process services and jobs except `PATH` (each run
also masks its own, whatever a reload changed). Values shorter than 4
bytes, a secret cut at the 4 KiB line limit, and literal secrets in `args`
or `script` are not masked: pass secrets through `environment`. `file`
output is never masked when written.

### Memory limit

`limits.memory` stops a process that uses too much memory and lets
[recovery](#recovery) start it again (D-084 (7), D-090):

```yaml
limits:
  memory: 512MiB          # 1MiB to 1024TiB; bytes, or KiB, MiB, GiB, TiB (KB, MB, GB, TB: powers of 1000)
```

- What is measured: the resident memory (RSS) of the process sentineld
  started, the leader of its group, read from `/proc/<pid>/statm` every 5
  seconds from its start. Its children are not counted: a `script`
  measures the shell, unless it ends with `exec` (as in the example
  above), and a server that forks workers is measured by its master only.
  Shared and file-backed pages count as resident.
- Over the limit: the first sample above `memory` (equal is not above)
  stops the whole group with `stop_signal`, then `SIGKILL` after
  `stop_timeout`, as any stop. The end is a failure with code
  `memory_limit`: `service_failed` carries `rss_bytes` and `limit_bytes`,
  and recovery starts the process again within its budget. The stop keeps
  its `stop_timeout` even if a reload, an operator's command or shutdown
  comes meanwhile: they wait for it. Only the end of
  `daemon.shutdown_timeout` cuts it short, as it does every stop: the
  group is then killed. While a process is being stopped for its memory
  (phase `stopping`), neither `startup_grace` nor `stable_after` makes it
  healthy or recovered: its state changes when it ends, as a failure.
- Not a hard ceiling: between two samples a process can allocate freely.
  A fast one can go well past its limit, or meet the kernel's OOM killer
  first (then the failure is `killed`, `SIGKILL`). For a hard ceiling use
  cgroups (systemd `MemoryMax=`); Sentinel does not manage cgroups yet
  (D-021).
- Samples that fail: a sample that cannot be read is never taken as zero
  and never stops the process. Status shows memory `UNKNOWN`, one error is
  logged, and the limit is checked again at the next sample that
  succeeds.
- Audit: each stop is recorded before it happens and its outcome after:
  action `supervisor.stop`, actor `daemon`, the service as target,
  `trigger: memory_limit`, `rss_bytes` and `limit_bytes`, the incident as
  correlation id. The intent is waited for at most 5 seconds; a broken
  audit log is logged but does not prevent the stop, which protects the
  host.
- Only memory: any other limit (`cpu`, `open_files`, …) is refused rather
  than accepted and ignored (D-021). Where sentineld cannot sample
  processes (outside Linux), a service with `limits.memory` is not
  started: its failure is `unsupported`.
- Changing it restarts the process and keeps its state and budget, as
  `recovery` does.

## systemd services

A service with a `unit` observes an existing systemd unit of the system
manager. Sentinel never creates, edits, enables, disables, starts or
stops a unit (PLAN.md §2); it restarts one only as described in
[Restarting units](#restarting-units).

```yaml
version: 1
services:
  - name: web
    unit: nginx.service          # implies type: systemd
  - name: console
    type: systemd
    unit: getty@tty1.service     # an instance of a template
    check_interval: 1m
    transition_grace: 2m
    failure_policy: {consecutive_failures: 2}
    notifications: [ops]
```

| Key | Default | Meaning |
|---|---|---|
| `unit` | required | a `.service` unit, in systemd's own grammar: at most 255 bytes of letters, digits, `:_.-\`, and `@` before an instance (`foo@bar.service`; a template `foo@.service` is refused) |
| `check_interval`, `timeout`, `failure_policy` | the settings' | as for HTTP services; `timeout` bounds one read of the unit |
| `transition_grace` | `90s` (0 to 10m) | how long the unit may stay unsettled before that counts as a failure |
| `recovery` | absent | a mapping: sentineld restarts the unit when `modules.supervisor.systemd_restart` is true; `recovery: {}` takes the defaults. Absent: the unit is observed only. A null `recovery:` is an error |

`limits` and `startup_grace` are refused. Two services cannot name the
same unit. Two names of one unit (an alias, or an alias and its real
name) are found when they are read: both services then decide nothing
(`unit_alias`, phase `unavailable`), neither is restarted, and the log
says so until one of them is removed.

### What a check reads

Every `check_interval`, sentineld asks systemd over its private socket
(`/run/systemd/private`, the one `systemctl` uses as root; systemd accepts
only root on it, and no bus daemon is needed) for a few properties of the
unit: `LoadState`, `ActiveState`, `SubState`, whether a job is queued,
`InvocationID`, `NRestarts` and `Result`. It never reads the unit's command
line or environment. It uses `LoadUnit` to find the unit, which loads an
inactive unit's definition into systemd's memory: that changes no file
and starts nothing.

| The unit is | The check |
|---|---|
| `active` | succeeds |
| `failed` or `inactive` | fails: `unit_failed` or `unit_inactive`. A unit an administrator stops with `systemctl stop` is a failure; `sentinelctl supervisor disable` pauses its supervision instead |
| not found, masked, or with errors in its file | fails: `unit_not_found`, `unit_masked`, `unit_load_failed` |
| `activating`, `deactivating`, `reloading`, waiting for an automatic restart (`auto-restart`) or with a job queued | decides nothing while the unit has been unsettled for less than `transition_grace`, however its substate changes; then fails with `unit_stuck` |
| `active` again with a new `InvocationID` or a higher `NRestarts` since the last active check | fails once with `unit_restarted`: it was started again since, by systemd or by hand, also after a check saw it failed. A unit that crashes and is restarted at every interval therefore never counts a success |

A check that cannot read the unit decides nothing: no failure, no
success, no event. That happens when systemd is not the init system (as
on Alpine), does not answer in time, the unit changed during the read, or
systemd answered something sentineld does not know, such as a state a
newer systemd added. Status shows the phase `unavailable` and why
(`systemd_unavailable`, `unit_changing`, `unit_invalid`), and the log says
when a unit stops and starts being readable. A configuration with systemd
services is therefore valid on any host.

sentineld must run as root to read units (the private socket admits root
only); otherwise its systemd services stay `unavailable`. The baseline of
restart detection is saved in the state file, with the boot it was read
in: a restart by systemd while sentineld was down is seen at its next
start, and counted once; a reboot is not a restart of the unit. If the
boot id cannot be read, a reboot counts as one restart.

Automatic restarts by systemd (its own `Restart=` policy: the increases of
the unit's `NRestarts`, read in any state, also while the unit waits in
`auto-restart`) are counted apart from sentineld's (`unit_restarts` in
status). The count is saved with the unit's name and the boot: after a
reboot, after sentineld's own restart and after an administrator's start
(which set it to 0 again), what it holds is new; another unit behind the
name starts it over. When they reach `max_attempts` within `window` (those
of the service's `recovery`, or the defaults, 5 in 10 minutes, for a unit
only observed), `service_flapping` says so, once per window: a unit
systemd restarts at every interval may look healthy or in transition at
each check. The event
itself decides nothing: it does not use the budget, and each restart seen
when the unit is next active is still one failed check
(`unit_restarted`).

### Restarting units

A systemd service with a `recovery` mapping is restarted when
`modules.supervisor.systemd_restart: true` is set in the central file
(D-095). Both are required for automatic restarts: with only one of them,
`validate` warns, and a service with `recovery` shows
`blocked_reason: not_armed` while an incident is open. An operator's
`restart` needs only the switch.

```yaml
# /etc/sentinel/sentinel.yaml
modules:
  supervisor:
    enabled: true
    systemd_restart: true

# /etc/sentinel/supervisor/web.yaml
version: 1
services:
  - name: web
    unit: nginx.service
    recovery: {max_attempts: 3, window: 30m, restart_timeout: 3m}
```

`recovery` takes the keys of a process service's, with the same defaults,
and `restart_timeout` (default `2m`, 1m to 15m): how long an attempt waits
for systemd's job. It does not change the unit's own timeouts.

- When: only with an open incident (after `consecutive_failures`), and
  only while the unit is settled and not running (`failed` or
  `inactive`), loaded and with no job queued. A unit that systemd is
  restarting itself (`auto-restart`), one in transition, one that is not
  found, masked or with errors in its file is left alone. Attempts follow
  the [recovery](#recovery) budget. During the delay the unit is still
  read: one that comes back meanwhile is not restarted.
- How: a fresh read first; then the intent is recorded in the audit log
  (action `supervisor.restart`, the service as target, `unit`, `trigger`,
  `attempt`, the incident as correlation id) and the attempt saved; then
  systemd is asked to restart the unit in mode `fail`, which never
  replaces a job already queued (an administrator's
  `systemctl stop --no-block`, say): systemd refuses instead. sentineld
  reads the unit every second until its job ends, at most
  `restart_timeout`. The attempt succeeded if the unit runs again with a
  new invocation; it emits `service_restarted`.
- Outcomes: an attempt that fails spends the attempt and records one of
  the [restart outcomes](#restart-outcomes) in the audit record's
  `failure`. systemd refuses a unit at its start limit, and sentineld
  never resets the limit (`systemctl reset-failed` is the
  administrator's).
- Unknown outcomes: when the connection drops or systemd does not answer
  after the request was sent, sentineld does not ask again: it reads the
  unit. A job there is waited for; a new invocation is a restart;
  otherwise the attempt is spent and the budget goes on.
- Its own restarts are not systemd's: the invocation sentineld replaces is
  saved with the attempt, so the new one is never counted as a restart by
  systemd, also across a restart of sentineld; restarts by systemd after
  it still are. Residual: an administrator's `systemctl restart` while
  sentineld's attempt runs is taken for that attempt.
- Pausing: a unit stopped on purpose (`systemctl stop`) is restarted too;
  use `sentinelctl supervisor disable` to pause its supervision.
- Reloads: turning `systemd_restart` off ends any attempt waiting for its
  delay. A job already submitted is left to systemd.
- Aliases: a restart waits until every other systemd service has read its
  unit once, so that an alias is found before it acts. Until then status
  shows `blocked_reason: alias_unresolved` and the log says so once. A
  service whose unit is never read (systemd unreachable, or an answer
  sentineld cannot read) keeps holding the others back, since it may still
  be an alias: fix or disable it. An operator's `restart` in that short
  window is refused (`conflict`) rather than kept waiting: try again.

## Jobs

A job is a program sentineld runs on a cron schedule (D-097, D-098). It
needs the same central switch as process services,
`modules.supervisor.processes: true`; without it jobs are validated and
never run. Jobs go under `jobs:` in the module's files, beside
`services:`.

```yaml
version: 1
jobs:
  - name: backup
    schedule: "30 2 * * *"            # 5 fields or a macro (@daily, @hourly…)
    timezone: Europe/Rome             # default: daemon.timezone
    timeout: 2h                       # default 1h, 1s to 24h
    command: /usr/local/bin/backup    # or script/shell, as a process service
    args: [--target, /srv/backup]
    user: backup
    group: backup
    environment: {BACKUP_KEY: "${BACKUP_KEY}"}
    output: log
    stop_signal: SIGTERM
    stop_timeout: 30s
    notifications: {channels: [ops], events: [job_failed, job_timeout]}
```

| Key | Default | Rules |
|---|---|---|
| `schedule` | none | required: minute hour day-of-month month day-of-week, or `@yearly`, `@annually`, `@monthly`, `@weekly`, `@daily`, `@midnight`, `@hourly` (D-018). A schedule that can never run (`0 0 30 2 *`) is refused |
| `type` | `cron` | the only type of a job |
| `timezone` | `daemon.timezone` | an IANA name or `Local` |
| `timeout` | `1h` | 1s to 24h: then the run is stopped with `stop_signal`, `SIGKILL` after `stop_timeout` |
| `concurrency_policy` | `forbid` | what a slot does while a run of the job is not over: `forbid`, `allow` or `replace` |
| `missed_runs` | `skip` | what happens to slots more than a minute late: `skip` or `run_once` |
| `command` … `stop_timeout` | as for [process services](#process-services) | the same keys, rules, defaults and trust checks |
| `notifications` | none | events `job_succeeded`, `job_failed`, `job_timeout` |
| `enabled` | `true` | `false`: validated, never scheduled |

`recovery`, `failure_policy`, `startup_grace`, `limits`,
`check_interval`, `restart`, `url` and `unit` are refused: a failed run is
reported, and the next slot runs as usual (D-012).

### When a job runs

- The first run of a new job is its next slot after sentineld starts or
  the job appears: no slot of the past is run.
- A slot runs if sentineld launches it within one minute of its time; a
  slot later than that is missed. After sentineld was down, or the clock
  jumped forward, every slot older than a minute is missed at once:
  status counts one catch-up and shows the interval. With
  `missed_runs: skip` they are not run; with `run_once` the latest of them
  runs once ([Catch-up](#catch-up)).
- A slot already run is never run again, whatever the clock does (set
  back by NTP or by hand): the job waits until the clock is past the slots
  it consumed, and status shows `clock_behind_watermark` meanwhile. If the
  clock was wrong forward by more than a day (a bad hardware clock at
  boot, then NTP), waiting would stop the job for that long: at the next
  reload or start such a watermark goes back to now, with a warning, and a
  slot of that day may run a second time.
- Daylight saving time follows D-096: an hour field starting with `*`
  (`*`, `*/2`) follows elapsed time; any other hour is a fixed local time
  that runs once after a spring gap and once, at its first occurrence, in
  an autumn overlap.
- A job reads its zone when it is configured: a change of the host's own
  zone (`TZ`, `/etc/localtime`) under `Local` applies at the next restart
  of sentineld.

### Concurrency

`forbid` and `replace` count runs by name, whatever definition launched
them; `allow` counts only the runs launched under `allow`, in the whole
module. With every policy a slot is skipped (logged once per streak and
counted, no event) while a service's process or a job loop that did not
end still holds the name.

- `forbid`: a slot is skipped while a run of the job is going, or while a
  run's process group is not confirmed gone. The run goes on.
- `allow`: runs overlap. `settings.max_concurrent_runs` (default 32)
  bounds the runs launched under `allow` in the whole module, with those
  whose group is not confirmed gone, and they count until their record
  goes; `forbid` and `replace` jobs do not count, and one `allow` job may
  fill it for the others. At the limit the slot is skipped, and status
  shows `runs_full` as what the last slot found, without changing the
  phase. A new value applies at once, without a restart: runs above it
  finish.
- `replace`: a slot stops every run of the job, each with its own
  `stop_signal` and `stop_timeout`, waits for their ends and launches. The
  intent (`replaces`: up to 15 run ids; `replaces_count`) and the
  reservation come first: if the audit log or the state file refuses
  them, nothing is stopped. Two runs never overlap: if a process does not
  end or its group is not confirmed gone after `stop_timeout` and the
  cleanup, nothing is launched and the slot is skipped (audit
  `replace_stop_failed`). A run whose record has no process here (left by
  a crash, or not cleaned up) is never signalled: the slot is skipped. A
  stop may take minutes: the launch stays valid, and slots passed
  meanwhile count once in `skipped`. A job still running when its next
  slot comes gets `job_failed` with `failure: replaced`, once per slot.

### Catch-up

With `missed_runs: run_once`, after a downtime or a forward clock jump,
one run is made for the latest missed slot, whatever its age and at once,
not spread out with other jobs: `scheduled` is that slot, the audit says
`trigger: catch_up`, and `missed` counts one. A slot still within its
minute stands for the older ones and runs as usual.

- A catch-up that cannot start now waits, with status `catch_up_pending`,
  retrying every 10 s. Behind a run under `forbid`, a record of its name
  or a full `max_concurrent_runs` it writes nothing, so it survives a
  restart. If the audit log or the state file refuses it, status says
  `blocked`, and each try that reaches the audit log leaves an intent and
  a failed outcome. The next regular slot absorbs it.
- Under `replace` it stops the active run; under `allow` it starts if
  there is room. A slot expiring during its admission becomes a catch-up.
- There is no catch-up for a job that is new, changed, enabled again or
  armed again: it starts from its next slot, even if that happened during
  a downtime. Nor is there one for slots skipped by the concurrency
  policy.
- A crash during a `replace` ends both runs `abandoned` and leaves that
  slot consumed: a program is not run again without knowing how far the
  last one got.

### A run

Before the launch, the intent is recorded in the audit log
(`supervisor.job_run`, the run's id as correlation id) and the run saved
in the state file. If either fails, nothing is launched and the slot is
tried again while it is within its minute, then missed. Status shows
`blocked` with the reason: `audit_unavailable`, `state_unavailable`,
`records_full` (the state file holds as many process and run records as it
may) or `reload_stop_failed`; the daemon reports a broken audit log, and
status says `state file: NOT SAVED` while the state cannot be written.
The outcome of the launch is recorded too.

Each run that reaches its launch ends with one event
([States and events](#states-and-events)), of source type `cron`. A
channel's `repeat_interval` throttles repeated events of one job whatever
their run id.

### Ownership

A run belongs to sentineld, not to the job's configuration. Removing a
job, disabling it (in the file or with `supervisor disable`) or changing
its definition lets its runs finish (or time out) under the definition
they started with; a changed job schedules from its next slot.
sentineld's shutdown and `processes: false` stop them (`canceled`).

Each run's process group is recorded with the run. After a crash,
sentineld kills what is left of a recorded run at the next start and
reports it `abandoned`; its slot is not run again. A run whose group
cannot be confirmed empty stays recorded (and keeps its job's slots
skipped) until a later cleanup succeeds: sentineld tries again at each of
the job's slots and at the next start, and logs the skips once per
streak. The residuals of process services apply ([How it
runs](#how-it-runs)): a descendant that left its group, or one launched
just before a crash that kept the group from being saved, escapes.

## Commands

The reading commands need the `read` tier (members of
`daemon.socket_group`); the [operator commands](#operator-commands) and
[`logs`](#logs) need the `operate` tier. If the module is not enabled or
not running, the commands answer `unavailable`.

### List and status

```sh
sentinelctl supervisor list             # one line per service and job
sentinelctl supervisor status           # details of every entry
sentinelctl supervisor status web       # details of one
sentinelctl supervisor status web -json # the same for scripts
sentinelctl supervisor logs web -n 100  # recent output (operate tier)
```

```text
SERVICE  TYPE  STATE    SINCE
api      http  failed   2026-10-19T13:17:47Z
web      http  healthy  2026-10-19T09:02:11Z
```

`list` marks disabled entries, and `status` says who disabled one
(`configuration` or `operator`) and who holds a process stopped. For a
checked service `status` also shows:

- the endpoint (scheme, host, port);
- the interval and timeout;
- the last check, how long it took and how long it waited for a slot;
- the last failure code and the counters;
- the open incident;
- `waiting`: all `max_concurrent_checks` slots are busy. This is
  capacity, not a fault of the endpoint;
- `stalled`: a timed-out check has not returned yet, and no new check
  starts until it does.

For a process service it shows the phase (`not_armed`, `starting`,
`running`, `exited`, `start_failed`, `restarting`, `exhausted`, `blocked`,
`stopping`, `stopped`, `disabled`), the pid and uptime, the starts,
failures and restarts, the attempts in the window, the next attempt or the
end of the cooldown, why a launch is blocked, the last exit, the open
incident, `output LOST` when the output file failed, and with
`limits.memory` the last
sample against the limit (or `UNKNOWN` while samples fail). It never shows
the program, its arguments, its environment or its user. The header says
when process services are not armed, how many groups left by a previous
run are still to be cleaned up, when the state file was corrupt at start,
and the program output lines dropped for the journal (`output_dropped` in
JSON).

For a systemd service it also shows the phase `transition` or
`unavailable` with its reason, `unit_restarts` (restarts by systemd) and,
while an incident is open, a `blocked_reason` such as `not_armed` or
`alias_unresolved`.

For a job, `list` shows its schedule, zone, phase (`scheduled`,
`running`, `blocked`, `not_armed`, `disabled`), next run and last result.
`status` adds the timeout, the two policies, whether a catch-up is
pending, why a launch is blocked, the runs not over (id, slot, pid,
phase), the last run and the counters: runs, successes, failures,
timeouts, skipped slots and catch-ups with the last missed interval.
`clock_behind_watermark` (with the watermark) and `runs_full` show while
the phase stays as it is.

### Operator commands

Six commands act on one service by name (D-089). They need the `operate`
tier and are recorded in the audit log, with the service as target,
before they run; a refused audit record means nothing runs. None takes a
program, an argument, an environment or a user: they stop, resume or
reset what root configured, and never raise `enabled: false` or
`modules.supervisor.processes`.

```sh
sentinelctl supervisor stop worker      # stop it and hold it stopped
sentinelctl supervisor start worker     # start it again
sentinelctl supervisor restart worker   # stop, then start
sentinelctl supervisor disable web      # take it out of supervision (any type)
sentinelctl supervisor enable web       # put it back
sentinelctl supervisor reset worker     # renew its recovery budget
```

| Command | Applies to | Effect |
|---|---|---|
| `stop` | process | Stops the process with its `stop_signal` and `stop_timeout`, then holds it stopped until `start`. Not a failure: no event, no attempt. |
| `start` | process | Clears the hold and launches the process. Already running: nothing to do. |
| `restart` | process, systemd | Stops the process, then launches it; restarts a systemd unit, running or not, in mode `fail`. |
| `disable` | every service and job | The service is not run (process) or not checked (HTTP, systemd) until `enable`. Its state and open incident are kept as they are. |
| `enable` | every service and job | Clears `disable`. A process held stopped stays stopped. |
| `reset` | process, systemd with `recovery` | Clears the recovery attempts and the exhaustion. An exhausted or waiting service is launched (restarted) at once; a running one is left alone. The open incident stays open until `stable_after`. |

- systemd services: `start` and `stop` are refused, since a unit is
  started and stopped with `systemctl`. `restart` needs
  `modules.supervisor.systemd_restart: true`, but not a `recovery` key. It
  is refused when systemd has a job queued for the unit, for an alias,
  and, for a service with `recovery`, with an open incident when the
  budget is spent (a unit only observed has no budget). It restarts the
  `stable_after` of an open incident. It holds the command slot until the
  restart and the reads around it end (the attempt itself at most
  `restart_timeout`), so a second command answers `unavailable`
  meanwhile. `reset` needs `recovery`.
- Jobs: `disable` and `enable` apply as to services, with the same tier
  and audit, persisted across reloads, restarts and definition changes,
  and `enabled: false` in the file wins. `disable` lets active runs finish
  and starts no new one (phase `disabled` while they do; they show in the
  runs not over). `enable` starts from the next slot, also under
  `run_once`. A loop that does not end for a `disable` is reported
  `unavailable` and launches nothing; `enable` is refused until it has
  ended. `start`, `stop`, `restart` and `reset` are refused.
- Precedence: `enabled: false` in the configuration wins: `enable`,
  `start` and `restart` are refused (`conflict`). A disabled service must
  be enabled before `start`. Without `processes: true`, `start` and
  `restart` of a process are refused.
- Recovery budget: a launch with an open incident is a recovery attempt,
  whoever asks for it. `start` and `restart` go through the budget, emit
  `service_restarted` with `trigger: operator`, and are refused
  (`conflict`) when it is spent: `restart` then stops nothing. Only
  `reset` renews it. A `restart` of a healthy service is not an attempt.
- Persistence: `stop` and `disable` are saved in the state file and
  survive reloads, restarts of sentineld and reboots. They follow the
  service's name, also when its definition changes, and are dropped when
  the service is removed. `enable`, `start` and `reset` take effect only
  once saved: if the state file cannot be written they change nothing
  (`unavailable`); in the rare case where the file was replaced but cannot
  be written back either, the message says a restart could apply the
  change. `stop` and `disable` apply even then, and the reply says they
  are not saved: repeat the command once the disk is fixed to save them.
- Replies: a command answers within two seconds. A stop that takes longer
  goes on under its own `stop_timeout` and the reply says it is still in
  progress; `supervisor status` shows how it ends (`stopping`, then
  `stopped`). The socket's audit record `ok` means accepted. sentineld
  also records the outcome of each stop, disable, enable and reset itself
  (actor `daemon`, `trigger: operator`), and each launch as usual, with
  the request's correlation id.
- One at a time: a second command while one is running, or during a
  reload, answers `unavailable`: try again. A reload waits for a running
  command.
- A process that does not stop when asked keeps running, owned by
  sentineld and stopped at shutdown; its name is `blocked`
  (`operator_stop_failed`) until sentineld restarts, as after a reload.

### Logs

`sentinelctl supervisor logs <name> [-n N]` shows the last lines of a
service or job, oldest first: 50 by default, at most 1000. It needs the
`operate` tier and every request is audited, because output may hold what
a program was given (D-099, D-100).

| Target | Where the lines come from |
|---|---|
| systemd service | the journal, under the unit's canonical name, so an alias works and a disabled service reads too |
| process service or job, `output: {file: …}` | the tail of the current file, with no times; rotated files are not read; a file not written yet has no lines |
| process service or job, `output: log` | the journal, the program's own entries ([Output in the journal](#output-in-the-journal)); each line of a job shows its run id. `unavailable` when sentineld's stderr is not journald's stream: without systemd, or started by hand |
| `output: discard`, HTTP service | `unsupported`, there is nothing to read |

Lines are redacted when read: the configuration's secrets and every value
of an `environment` except `PATH`, each line of a multi-line one too.
Values under 4 bytes are not masked; `sentinelctl validate` warns about
short values from `${NAME}`, and about those under a secret-looking key in
`environment` or a notification header. Only what the configuration knows
now is masked: a secret you removed still shows in older lines, and a
secret caught half written, on a last line not yet ended, shows what was
written so far. A line holding characters a terminal could act on is
printed quoted.

An answer holds at most 256 KiB, and a line 4 KiB; the newest lines are
kept. `some lines may be missing` means a bound was reached or
`journalctl` reported a problem such as a corrupt journal file: the
details are in sentineld's log. At most 4 reads run at once, and
`journalctl` has 10 s. A read stuck on a filesystem that does not answer
keeps its place until the filesystem answers or the module restarts, so
four of them turn every `logs` away with `too many log reads`.

`no lines` means the source could be read and holds nothing: for a
journal, a second query confirmed it is readable. A journal that cannot be
read answers `unavailable`, with the reason in sentineld's log: no
`journalctl` (a host without systemd; `file` output still reads there), a
`journalctl` not owned by root, no group `systemd-journal`, or a journal
closed to that group. `journalctl` runs as uid 65534 (`nobody`) with that
group, or as sentineld's own user when that is not root, which then needs
read access. An output file that is a link, has more than one hard link,
or could be changed by another user is refused as it is when written: the
answer is `unavailable`, with the reason in sentineld's log.

#### Output in the journal

When sentineld runs under systemd (its stderr is journald's stream,
whatever `daemon.log.format`), each line of `log` output is a journal
entry of its own (D-100), sent to journald's socket with these fields:

| Field | Value |
|---|---|
| `MESSAGE` | the line, redacted |
| `PRIORITY` | `6` (info), for both streams |
| `SYSLOG_IDENTIFIER` | `sentineld/<name>` |
| `SENTINEL_SOURCE` | the service or job name |
| `SENTINEL_STREAM` | `stdout` or `stderr` |
| `SENTINEL_RUN_ID` | a job's run id, as in its events and audit records |

`journalctl -u sentineld` still shows the lines, under
`sentineld/<name>`. To query them yourself, match the source together
with sentineld's uid, which journald sets and no other user can:

```sh
journalctl SENTINEL_SOURCE=web _UID=0
```

`SENTINEL_SOURCE` alone is not enough: any local user can write an entry
with that field, under their own uid. A program supervised as root can
forge one under uid 0 too; `supervisor logs` cannot tell those apart.

A line is at most 4087 bytes once redacted, so that `journalctl -o json`
shows it; a longer one ends with `[line cut]`.

A program waits for journald at most 20 ms per line. When journald's
socket is full, sentineld retries for that long, then drops the line and
counts it (`program output lines dropped` in `supervisor status`,
`output_dropped` in its JSON), and does not wait again for that stream
until journald takes one of its lines. When the socket is missing, for
example while journald restarts, lines go to sentineld's log instead,
with one warning per outage: `supervisor logs` does not find those, and
while sentineld's own log cannot be written, as when journald stops
reading, that write can hold the program too. Delivery is best effort:
journald's own rate limit, shared by sentineld and every program it runs,
and its storage failures are not counted.

Without systemd, or with sentineld started by hand, lines go to
sentineld's log, and `supervisor logs` answers `unavailable` for `log`
output. Output written before an upgrade to v0.3.0 cannot be read per
program, and a name reused for another program shows the earlier one's
lines too.

## Reload

`sentinelctl reload` (or SIGHUP) applies a new supervisor configuration
in place (D-088): only the entries whose definition changed are touched.
`sentinelctl reload` then lists `reconfigured: supervisor`.

- Unchanged services keep running: same process and pid, same loop, same
  incident and budget. A change of `notifications` alone only changes
  where the next events go.
- Changed services are stopped, then started from their new definition. A
  service whose program, arguments, identity, directory, environment,
  output, stop or startup settings are unchanged keeps its state, so
  editing `recovery` or `limits` restarts the process but keeps its
  attempts and its open incident.
- Removed services are stopped and forgotten; new ones start.
- `modules.supervisor.processes: false` stops every process service;
  `true` starts them again within their budget.
- A new `settings.max_concurrent_checks` restarts the whole module.
- A kept HTTP service keeps its checker: a rotated `tls.ca_file` with an
  unchanged configuration still needs a restart.
- Jobs: an unchanged job keeps its schedule. A changed one (program,
  schedule, time zone, timeout) starts from its next slot, never running a
  slot it already consumed. A removed or disabled one schedules nothing
  more. A job enabled or armed again starts from its next slot, without a
  catch-up. Their runs go on, except with `processes: false`, which stops
  them, and except when the reload restarts the whole module (a new
  `max_concurrent_checks`), which stops them as a shutdown does. A launch
  not begun when its loop ends does not start (`canceled`).
- A new `concurrency_policy` counts the old runs by name: to `forbid`,
  slots are skipped until they are all gone; to `replace`, the next slot
  stops them; to `allow`, they count against the limit only if launched
  under `allow`.
- A job an operator disabled stays disabled, also across a change of its
  definition; removed and added again, it starts enabled, with counters
  at zero.
- A change of `daemon.timezone` moves only the jobs that inherit it.

If a service does not stop (its process ignores the stop signal and
`SIGKILL` within the reload's time, or an HTTP check ignores its
cancellation), it keeps running as it was and is stopped at shutdown. Its
new definition waits, with status `blocked` / `reload_stop_failed`, until
sentineld restarts. A process that `SIGKILL` ends at the reload's deadline
is stopped and replaced; one whose output stays open (a descendant that
left its group holds it) still counts as not stopped. A job whose loop
does not end within the reload's time keeps its name until the loop ends;
a later reload then starts its new definition. The reload is reported
`partial`, and so is every later one until sentineld restarts (for a
service) or the loop has ended (for a job). Two instances of a service
never run side by side.

## Reference

### States and events

Service events carry the incident's id as `correlation_id`
([Incidents](#incidents)); job events carry the run's id.

| Event | Severity | When | Attributes |
|---|---|---|---|
| `service_failed` (HTTP, systemd) | error | `consecutive_failures` failed checks | `failure`, `consecutive_failures`; HTTP `endpoint`; systemd `unit`, `active_state`, `sub_state` |
| `service_failed` (process) | error | an exit or a failed start | `failure`; for an exit `exit_code` or `signal`, and `startup`; for `memory_limit` also `rss_bytes` and `limit_bytes` |
| `service_recovered` (HTTP, systemd) | info | the incident closes | `consecutive_successes`, `downtime_seconds`; HTTP `endpoint`; systemd `unit`, `active_state`, `sub_state` |
| `service_recovered` (process) | info | the incident closes | `downtime_seconds`, `restarts` |
| `service_restarted` | warning | a recovery attempt | `attempt`, `max_attempts`, `trigger` (`failure`, `cooldown` for the first attempt after one, or `operator`); for systemd also `unit`, `active_state`, `sub_state` |
| `service_recovery_exhausted` | critical | the budget is spent | `max_attempts`, `window_seconds`, `cooldown_seconds`, `resume_at` (absent with `cooldown: 0`); for systemd also `unit`, `active_state`, `sub_state` |
| `service_flapping` | warning | systemd restarted the unit `max_attempts` times in `window` | `restarts` (by systemd within the window), `window_seconds`, `unit`, `active_state`, `sub_state`; the open incident's id as `correlation_id`, if any |
| `job_succeeded` | info | exit status 0 | the job attributes below |
| `job_failed` | error | any other end | the job attributes and `failure` ([job codes](#job-run-codes)) |
| `job_timeout` | error | stopped at `timeout` | the job attributes |

`endpoint` is the URL's scheme, host and port only: the path and the
query may hold a token.

Job attributes: `job`, `run_id`, `scheduled`; when it ran, `started`,
`duration_ms` and `exit_code` or `signal`. An `abandoned` run has
`started` only if it had started, and neither `duration_ms` nor an exit
(how it ended is not known); a `launch_failed` run, or one `canceled`
before its launch, has none of them.
Times are RFC 3339 in UTC. Events never hold the program, its arguments,
environment or output.

### Failure codes

A failure is a fixed code, never error text.

HTTP checks:

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

The check loop adds `check_failed`, `panic` and `invalid_result` for
checker faults: internal errors that should never appear.

Process services:

| Code | Meaning |
|---|---|
| `exited` | the process exited by itself (`exit_code`, 0 included) |
| `killed` | a signal ended it (`signal`) |
| `not_found` | the program or the working directory is missing |
| `permission_denied` | not allowed: an identity change sentineld may not make, or no execute permission |
| `unsupported` | an identity change, or a memory limit, outside Linux |
| `untrusted_executable` | the program could be changed by another user ([Process services](#process-services)) |
| `output_failed` | the output file could not be opened safely |
| `exec_failed` | any other start failure |
| `cleanup_failed` | what a previous run left in its group could not be killed: the service is not started, and the cleanup is tried again every `max_delay` |
| `memory_limit` | sentineld stopped it: its resident memory was over `limits.memory` (`rss_bytes`, `limit_bytes`) |

A launch interrupted by a shutdown, a reload, a `stop` or a `disable` is
recorded in the audit log only, as `canceled`.

systemd checks:

| Code | Meaning |
|---|---|
| `unit_failed` | the unit is `failed` |
| `unit_inactive` | the unit is `inactive`, with no job queued |
| `unit_not_found` | systemd has no such unit |
| `unit_masked` | the unit is masked |
| `unit_load_failed` | the unit's file has errors (`bad-setting`, `error`) |
| `unit_stuck` | unsettled for longer than `transition_grace` |
| `unit_restarted` | started again since the last active check, by systemd or by hand |

A check that decides nothing shows the phase `transition` with
`unit_transition` (within `transition_grace`), or `unavailable` with
`systemd_unavailable`, `unit_changing`, `unit_invalid` or `unit_alias`.

#### Restart outcomes

The `failure` of a failed restart attempt in its audit record:

| Code | Meaning |
|---|---|
| `job_failed` | systemd's job (not a cron job) ended and the unit does not run |
| `job_timeout` | systemd's job was not confirmed done by `restart_timeout` |
| `outcome_unknown` | the reply was lost and no restart was confirmed; reported when a read finds no job and the old invocation, or at `restart_timeout` |
| `systemd_unavailable` | systemd could not be reached |
| `canceled` | sentineld stopped or reloaded |
| `conflict`, `not_found`, `masked`, `load_failed`, `manual_refused`, `not_applicable`, `denied`, `refused` | systemd refused the restart |

#### Job run codes

The `failure` of a `job_failed` event, also shown in status:

| Code | Meaning |
|---|---|
| `exit_code` | the program exited with a status other than 0 |
| `signal` | a signal ended it |
| `launch_failed` | the program could not be started |
| `canceled` | stopped by sentineld's shutdown or by `processes: false`, or a launch stopped at its last moment (for a shutdown, a reload or a `disable`), except the `run_once` case below |
| `replaced` | stopped for the next slot (`concurrency_policy: replace`) |
| `abandoned` | found running or reserved after a crash of sentineld |

Some runs are given up after their intent was recorded but before any
launch. They emit no event; only their audit outcome says why:

| Code | Meaning |
|---|---|
| `canceled` | given up before its last moment, for a shutdown, a reload or a `disable`; under `run_once` also at its last moment, unless a `replace` stop began, and the slot is given back |
| `missed` | the slot passed its minute while the intent was recorded and the run saved; for a catch-up, the next slot came due |
| `not_due` | the clock went back while the intent was recorded, so the slot is not due yet; the slot is given back |
| `replace_stop_failed` | under `replace`, what was stopped did not end or its group was not confirmed gone: nothing is launched |

### State across restarts

Each service's health, counters and open incident are kept in
`<state_dir>/supervisor/state.json` (root-only, `0600`). A restart
therefore neither repeats a `service_failed` nor forgets an incident.

The file is written on every transition and at most every 60 s
otherwise. It holds no URL, header, body, command, argument or
environment: only a digest of each service's definition; for process
services their start and restart counts, last exit, recovery attempts and
exhaustion, and the process group to clean up; and what operators asked
(`stopped`, `disabled`). Schema 3 (D-093); a file of schema 1 (v0.1.0) or
2 (v0.2.0) is migrated at the first start, and those releases then refuse
the new one instead of misreading it.

When the definition changes, the service starts again from `unknown`. If
it was failed, its incident is then closed without a `service_recovered`.

| Type | Its definition |
|---|---|
| HTTP | the URL, the request keys, TLS, `address`, `timeout` and the failure policy; `check_interval` and `notifications` are not part of it |
| systemd | its unit, `timeout`, `transition_grace` and the failure policy |
| process | every key except `notifications`, `recovery`, `limits` and `enabled` |
| job | the program keys, `schedule`, `timezone`, `timeout`, `concurrency_policy` and `missed_runs` |

For each job the file keeps its watermark (every slot at or before it is
consumed), its counters and last result, and every run that is not over,
by run id, with its process group. A new or changed job starts from now,
never earlier than the watermark it had.

If the state file cannot be written, supervision goes on, and
`sentinelctl supervisor status` shows `state file: NOT SAVED`. A process
started meanwhile has its group recorded in memory only: if sentineld is
then killed outside systemd, a descendant that survived is not cleaned
at the next start (residual, D-092; under systemd
`KillMode=control-group` ends the whole tree).

## Troubleshooting

| Symptom | Cause |
|---|---|
| `tls.ca_file is on an unsafe path` | The bundle, its directory or a parent is writable by group or others, or not owned by root or the sentineld user, or a link leaves its directory. Fix the modes and owners; name a system bundle by its real file, or omit `tls.ca_file` for the system roots. |
| `tls_failed` with an internal CA | Set `tls.ca_file`. If the certificate names another host than the URL, set `tls.server_name`. |
| `tls_failed` with `address` | The certificate is checked against the URL's host: the URL must use the name in the certificate, not the IP, or set `tls.server_name`. |
| `redirect_refused` | The endpoint redirects to another host (often a login page). Check the redirect itself instead: no `follow_redirects`, and its 3xx in `success_status_codes`. |
| `status_mismatch` on a 3xx | Redirects are not followed by default: add the code, or set `follow_redirects`. |
| Many services show `waiting` | Raise `max_concurrent_checks`, lower the timeouts, or spread the intervals. |
| A process service or a job stays `not_armed` | Set `modules.supervisor.processes: true` in the central file. |
| `untrusted executable` | The program, a directory on its path or a link to it is writable by group or others, or owned by another user. Fix the modes and owners. |
| `cleanup_failed` | A process left by an earlier run did not die (often stuck in the kernel, state `D`). Find it by its process group and deal with it; sentineld tries again every `max_delay`. |
| A process service stays `exhausted` | It failed `max_attempts` times in `window`. Fix the cause; it starts again at the end of `cooldown` (never with `cooldown: 0`), at once after `sentinelctl supervisor reset`, or if you change its definition. |
| A process service stays `stopped` or `disabled` | An operator stopped or disabled it (`status` says who): `sentinelctl supervisor start` or `enable`. `disabled` by `configuration`: set `enabled: true`. |
| `operator_stop_failed` | The process did not end within `stop_timeout` and `SIGKILL` (often stuck in the kernel). It runs until sentineld restarts; the name takes no command until then. |
| `conflict` from a command | The service's state does not allow it: the message says why and what to run. |
| A process service or a job stays `blocked` | `audit_unavailable`: the audit chain is broken (`sentinelctl status`); restore the evidence and restart sentineld. `state_unavailable`: the state directory cannot be written. `records_full` (jobs): too many process groups and runs not cleaned up; see the pending cleanups. `reload_stop_failed`: a service or a job loop did not stop for a reload; a service waits for sentineld's restart, a job for a reload after its loop ends. |
| A job shows `runs_full` | Its last slot found `settings.max_concurrent_runs` runs active: raise it, lower the `timeout` of the `allow` jobs, or spread them. |
| A job shows `catch_up_pending` | Under `run_once`, its catch-up waits: for a run or a record of its name, a full `max_concurrent_runs`, or the audit log or state file. `status` shows the runs not over, `runs_full` or the blocked reason; the next regular slot absorbs it. |
| Audit `replace_stop_failed` | A run did not end, or its group is not confirmed gone, so `replace` launched nothing. See the job's runs in `status` and the log; a group left is cleaned again at each slot. |
| A job shows `clock_behind_watermark` | The clock was set back behind slots already run: the job waits for it. More than a day behind (a clock that was wrong forward), the next reload resets the watermark. |
| `permission_denied` with `user` | sentineld is not root, or lacks `CAP_SETUID`/`CAP_SETGID`. |
| `memory_limit` again and again | The program needs more than its limit, or leaks. `supervisor status` shows the last sample: raise `limits.memory` or fix the program; the budget bounds the restarts meanwhile. |
| Memory shows a few MiB for a large program | The limit measures the leader only: a `script` without `exec` measures its shell, a forking server its master. |
| Memory `UNKNOWN` | `/proc/<pid>/statm` could not be read (see sentineld's log). The process runs unchecked until a sample succeeds. |
| A systemd service stays `unavailable` | sentineld is not root, systemd is not the init system, or the unit is an alias of another service's unit (`unit_alias`): the reason is in `status` and the log. |
