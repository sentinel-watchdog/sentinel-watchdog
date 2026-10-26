# Supervisor

The supervisor watches services and tells you when one fails and when it
comes back. It checks **HTTP endpoints** (Phase 3a) and runs **process
services** it owns (Phase 3b-2): it starts them, watches them, restarts
them within a bounded budget (Phase 3b-3) and stops them, and operators
can stop, start, disable or reset them (Phase 3b-5); a process over its
memory limit is stopped and recovered (Phase 3b-6). systemd units, cron
jobs and logs arrive in 3c. Until then a `systemd` service, or any job,
is refused as "planned but not implemented".

Related: [configuration](configuration.md) (the central file and module
directories), [notifications](notifications.md) (channels, payload,
routing), [operations](operations.md) (running sentineld). Decisions:
D-080, D-081, D-082, D-084, D-086, D-087, D-088, D-089, D-090.

## Enabling it

In the central file:

```yaml
modules:
  supervisor:
    enabled: true
    processes: true          # let it run process services; default false
```

`processes` is the only other key of the block (ADR-0015 rule 1): running
programs as root is armed in the central file, never by a file added to
the module directory (D-084 (1)). Without it, process services are
validated and shown as `not_armed`, but never started, and validation
warns about them.

The services live in `<config dir>/supervisor/*.yaml`, usually
`/etc/sentinel/supervisor/`. Each file needs `version: 1`, and the files
are read in lexical order. If the directory or the services are missing,
the supervisor starts empty and logs a warning.

```sh
sentinelctl validate          # checks the files and reads every CA bundle
sentinelctl reload            # applies them to a running sentineld
```

Complete examples are in
[configs/supervisor/http.yaml](../configs/supervisor/http.yaml) and
[configs/supervisor/process.yaml](../configs/supervisor/process.yaml)
(`examples/supervisor/` in the tarball).

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
| `enabled` | `true` by default. `false` keeps the service validated but never checked, whatever an operator asks (D-020); its state is kept. |

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

### Process services

A process service is a program sentineld starts and owns. `name` and
`command` are enough; the type is inferred from `command` or `script` (and
`http` from `url`; a service with both is an error).

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
| `command` | — | absolute, clean path; xor `script` (D-005) |
| `args` | none | with `command` only; at most 256, 4 KiB each, 64 KiB in all |
| `script` | — | run as `<shell> -c <script>`; at most 64 KiB |
| `shell` | `/bin/sh` | with `script` only; absolute |
| `user`, `group` | sentineld's | names (from `/etc/passwd` and `/etc/group`, never NSS) or decimal ids; a numeric user needs a group; supplementary groups are always dropped |
| `working_dir` | `/` | absolute; checked when the process starts |
| `environment` | none | the child's **whole** environment, plus `PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin` unless set; nothing of sentineld's own environment; at most 128 variables |
| `output` | `log` | `log` (lines to sentineld's log, at most 4 KiB each and 100 per second), `discard`, or a `file` opened for appending, created with `mode` (default `"0640"`, never group- or other-writable) |
| `stop_signal` | `SIGTERM` | `SIGTERM`, `SIGINT`, `SIGQUIT`, `SIGHUP`, `SIGUSR1`, `SIGUSR2` or `SIGKILL` |
| `stop_timeout` | `10s` | 1s–5m; then `SIGKILL` |
| `startup_grace` | `5s` | 0–10m; an exit before it is reported as during startup |
| `recovery` | see [Recovery](#recovery) | how the process is started again after a failure |
| `limits` | none | `memory` only: see [Memory limit](#memory-limit) |
| `enabled` | `true` | `false`: validated but never started, whatever an operator asks (D-020); not part of the definition's digest, so turning it off and on keeps the state |

`failure_policy`, `check_interval` and `timeout` belong to checked
services and are refused (D-012).

**The program must be trusted** (D-086). The file, every directory on
its path and every symbolic link on the way must be owned by root or by
sentineld's user and not writable by group or others (a sticky directory
such as `/tmp` may be passed through, but not hold the program or a link
to it). Otherwise a local user could replace what sentineld runs as root.
It is checked by `sentinelctl validate` and again at every start;
sentineld executes the resolved file, keeping the configured path as
`argv[0]`. What the program itself reads or runs is not covered.

**How it runs.** Each process gets its own process group, `/dev/null` as
stdin, and on Linux a parent-death signal: if sentineld dies, the
process is killed. When it is stopped (shutdown, a reload that changes
its definition, or an operator's command), the stop signal goes to the
whole group,
then `SIGKILL` after `stop_timeout`; when the program exits by itself,
what is left of its group is killed. sentineld records each group in its
state file; at the next start, before launching the service again, it
kills what a run that was itself killed (`kill -9`) left in that group.
Only processes of the same boot, in the recorded group and session and not
older than the recorded leader are killed.

Residuals (D-084 (2), D-086): a process that leaves its group or
session (`setsid`) escapes; after a `kill -9` of sentineld outside
systemd, the leftovers run until sentineld starts again; a group and a
session both reused by unrelated programs while sentineld was down would
match. In production the systemd unit (Phase 3d) kills the whole tree.

**Output secrets.** Lines sent to the log are masked with the
configuration's secrets (`${VARIABLE}` values, notification URLs and
headers) and with every value of the process services' `environment`.
Values shorter than 4 bytes, a secret cut at the 4 KiB line limit, and
literal secrets in `args` or `script` are not masked: pass secrets
through `environment`. `file` output is never masked.

### Recovery

A process service that ends without being asked to, or cannot be
started, is started again (D-013, D-014, D-087). Every process service
has a recovery policy, the defaults below when `recovery` is absent; it
runs only behind `modules.supervisor.processes: true`.

| Key | Default | Rules |
|---|---|---|
| `action` | `restart` | the only action |
| `max_attempts` | `5` | 1–100 attempts in `window` |
| `window` | `10m` | 1m–24h |
| `delay` | `1s` | 100ms–10m, before the first attempt |
| `backoff` | `exponential` | `fixed` (always `delay`) or `exponential` (doubled each attempt) |
| `max_delay` | `1m` | `delay`–1h; with `exponential` only |
| `stable_after` | `1m`, or `startup_grace` if longer | 1s–24h, not shorter than `startup_grace` |
| `cooldown` | `10m` | 1m–7d, or `0`: exhausted until `sentinelctl supervisor reset` (D-013) |

- **An attempt** is a launch made while the service has an open
  incident: after a crash, a failed start, or a restart or reload of
  sentineld that finds the incident still open. The first launch of a
  healthy service is not one.
- **Budget.** At most `max_attempts` attempts in any `window`. Attempt *n*
  waits `delay` (fixed) or `delay × 2^(n−1)` up to `max_delay`. When the
  window is full the service is **exhausted**: `service_recovery_exhausted`,
  no launch until `cooldown` has passed, then the budget starts again with
  an immediate attempt. With `cooldown: 0` only `sentinelctl supervisor
  reset` renews it.
- **Stability.** A process that runs for `stable_after` closes its
  incident (`service_recovered`); it does not free its attempts, the window
  does. A program that crashes every few minutes is therefore still
  stopped after `max_attempts` crashes in the window.
- **Persistence.** Each attempt is saved in the state file before the
  launch, so neither a restart of sentineld nor a reload resets the
  budget. Changing the policy keeps it; changing what runs (any other key
  but `notifications`) starts the service fresh.
- **Audit.** Every launch sentineld makes by itself is recorded in the
  audit log before it happens and its outcome after (ADR-0011): action
  `supervisor.start` (the first launch) or `supervisor.restart` (an
  attempt), actor `daemon`, the service as target, `trigger`, `attempt`
  and `max_attempts`, the incident as correlation id; the cleanup of a
  group a previous run left is recorded as `supervisor.cleanup` when the
  log takes records, and never waits for it. Never the program, its
  arguments or its environment.
- **Blocked.** If the intent cannot be recorded (the audit log is broken)
  or the attempt cannot be saved (the state directory is not writable),
  the process is not launched: status shows `blocked` with
  `audit_unavailable` or `state_unavailable`, and the launch is tried
  again every `max_delay`. A refused intent uses no attempt. A broken
  audit log refuses every record until sentineld restarts with the
  evidence restored, so until then no process service starts.
- **Corrupt state file.** It is moved aside; the processes still start,
  but their budgets count as spent for one `window` (the first crash
  exhausts), sentineld publishes `daemon_error` (`state_corrupt`) and
  status shows it (D-084 (3)).
- **Clock jumps.** A clock set back never frees budget early; a clock set
  forward frees it early, by at most `max_attempts` attempts per window of
  wall time.

### Memory limit

`limits.memory` stops a process that uses too much memory and lets
[recovery](#recovery) start it again (D-084 (7), D-090):

```yaml
limits:
  memory: 512MiB          # 1MiB–1024TiB; bytes, or KiB, MiB, GiB, TiB (KB, MB, GB, TB: powers of 1000)
```

- **What is measured.** The resident memory (RSS) of the process
  sentineld started, the leader of its group, read from
  `/proc/<pid>/statm` every **5 seconds** from its start. Its children are
  not counted: a `script` measures the shell, unless it ends with
  `exec` (as in the example above), and a server that forks workers is
  measured by its master only. Shared and file-backed pages count as
  resident.
- **Over the limit.** The first sample above `memory` (equal is not
  above) stops the whole group with `stop_signal`, then `SIGKILL` after
  `stop_timeout`, as any stop. The end is a failure with code
  `memory_limit`: `service_failed` carries `rss_bytes` and `limit_bytes`,
  and recovery starts the process again within its budget. The stop
  keeps its `stop_timeout` even if a reload, an operator's command or
  shutdown comes meanwhile: they wait for it. Only the end of
  `daemon.shutdown_timeout` cuts it short, as it does every stop: the
  group is then killed. While a process is being stopped for its memory
  (phase `stopping`), neither `startup_grace` nor `stable_after` makes it
  healthy or recovered: its state changes when it ends, as a failure.
- **Not a hard ceiling.** Between two samples a process can allocate
  freely: a fast one can go well past its limit, or meet the kernel's
  OOM killer first (then the failure is `killed`, `SIGKILL`). For a hard
  ceiling use cgroups (systemd `MemoryMax=`); Sentinel does not manage
  cgroups yet (D-021).
- **Samples that fail.** A sample that cannot be read is never taken as
  zero and never stops the process: status shows memory `UNKNOWN`, one
  error is logged, and the limit is checked again at the next sample that
  succeeds.
- **Audit.** Each stop is recorded before it happens and its outcome
  after: action `supervisor.stop`, actor `daemon`, the service as target,
  `trigger: memory_limit`, `rss_bytes` and `limit_bytes`, the incident as
  correlation id. The intent is waited for at most 5 seconds; a broken
  audit log is logged but does not prevent the stop, which protects the
  host.
- **Only memory.** Any other limit (`cpu`, `open_files`, …) is refused
  rather than accepted and ignored (D-021). Where sentineld cannot sample
  processes (outside Linux), a service with `limits.memory` is not
  started: its failure is `unsupported`.
- **Changing it** restarts the process (it is part of how it runs) and
  keeps its state and budget, as `recovery` does.

### Reload

`sentinelctl reload` (or SIGHUP) applies a new supervisor configuration
in place (D-088): only the services whose definition changed are touched.

- **Unchanged** services keep running: same process and pid, same loop,
  same incident and budget. A change of `notifications` alone only
  changes where the next events go.
- **Changed** services are stopped, then started from their new
  definition. A service whose program, arguments, identity, directory,
  environment, output, stop or startup settings are unchanged keeps its
  state, so editing `recovery` or `limits` restarts the process but keeps
  its attempts and its open incident.
- **Removed** services are stopped and forgotten; **new** ones start.
- `modules.supervisor.processes: false` stops every process service;
  `true` starts them again within their budget.
- A new `settings.max_concurrent_checks` restarts the whole module.
- `sentinelctl reload` lists `reconfigured: supervisor`.

If a service does not stop (its process ignores the stop signal and
`SIGKILL` within the reload's time, or an http check ignores its
cancellation), it keeps running as it was and is stopped at shutdown;
its new definition waits, with status `blocked` /
`reload_stop_failed`, until sentineld restarts. The reload is reported
`partial`, and so is every later one until then. Two instances of a
service never run side by side.

A kept http service keeps its checker: a rotated `tls.ca_file` with an
unchanged configuration still needs a restart.

### Operator commands

Six commands act on one service by name (D-089). They need the
`operate` tier and are recorded in the audit log, with the service as
target, before they run; a refused audit record means nothing runs.
None takes a program, an argument, an environment or a user: they stop,
resume or reset what root configured, and never raise
`enabled: false` or `modules.supervisor.processes`.

```sh
sentinelctl supervisor stop worker      # stop it and hold it stopped
sentinelctl supervisor start worker     # start it again
sentinelctl supervisor restart worker   # stop, then start
sentinelctl supervisor disable web      # take it out of supervision (any type)
sentinelctl supervisor enable web       # put it back
sentinelctl supervisor reset worker     # renew its recovery budget
```

| Command | Services | Effect |
|---|---|---|
| `stop` | process | Stops the process with its `stop_signal` and `stop_timeout`, then holds it **stopped** until `start`. Not a failure: no event, no attempt. |
| `start` | process | Clears the hold and launches the process. Already running: nothing to do. |
| `restart` | process | Stops the process, then launches it. |
| `disable` | all | The service is not run (process) or not checked (http) until `enable`. Its state and open incident are kept as they are. |
| `enable` | all | Clears `disable`. A process held stopped stays stopped. |
| `reset` | process | Clears the recovery attempts and the exhaustion. An exhausted or waiting service is launched at once; a running one is left alone. The open incident stays open until `stable_after`. |

- **Precedence.** `enabled: false` in the configuration wins: `enable`,
  `start` and `restart` are refused (`conflict`). A disabled service must
  be enabled before `start`. Without `processes: true`, `start` and
  `restart` are refused.
- **Recovery budget.** A launch with an open incident is a recovery
  attempt, whoever asks for it: `start` and `restart` go through the
  budget, emit `service_restarted` with `trigger: operator`, and are
  refused (`conflict`) when it is spent: `restart` then stops nothing.
  Only `reset` renews it. A `restart` of a healthy service is not an
  attempt.
- **Persistence.** `stop` and `disable` are saved in the state file and
  survive reloads, restarts of sentineld and reboots; they follow the
  service's name, also when its definition changes, and are dropped when
  the service is removed. `enable`, `start` and `reset` take effect only
  once saved: if the state file cannot be written they change nothing
  (`unavailable`); in the rare case where the file was replaced but
  cannot be written back either, the message says a restart could apply
  the change. `stop` and `disable` apply even then, and the reply says
  they are not saved: repeat the command once the disk is fixed to save
  them.
- **Replies.** A command answers within two seconds. A stop that takes
  longer goes on under its own `stop_timeout` and the reply says it is
  still in progress; `supervisor status` shows how it ends (`stopping`,
  then `stopped`). The socket's audit record "ok" means accepted;
  sentineld also records the outcome of each stop, disable, enable and
  reset itself (actor `daemon`, `trigger: operator`), and each launch as
  usual, with the request's correlation id.
- **One at a time.** A second command while one is running, or during a
  reload, answers `unavailable`: try again. A reload waits for a running
  command.
- **A process that does not stop** when asked keeps running, owned by
  sentineld and stopped at shutdown; its name is `blocked`
  (`operator_stop_failed`) until sentineld restarts, as after a reload.

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

A **process service** is `unknown` until it has run for `startup_grace`,
then `healthy`. Every exit sentineld did not ask for is a failure, exit
status 0 included: it emits `service_failed` and recovery starts it again
([Recovery](#recovery)), each launch emitting **`service_restarted`**
(severity warning). While an incident is open, a later failure adds no
`service_failed`. Once a process has run for `stable_after`, its incident
closes with `service_recovered`. When the budget is spent,
**`service_recovery_exhausted`** (severity critical) says so, once per
exhaustion. All of them carry the incident's id as `correlation_id`.

| Event | Attributes |
|---|---|
| `service_failed` (http) | `failure` (the code below), `consecutive_failures`, `endpoint` |
| `service_recovered` (http) | `consecutive_successes`, `downtime_seconds`, `endpoint` |
| `service_failed` (process) | `failure`; for an exit `exit_code` or `signal`, and `startup`; for `memory_limit` also `rss_bytes` and `limit_bytes` |
| `service_recovered` (process) | `downtime_seconds`, `restarts` |
| `service_restarted` | `attempt`, `max_attempts`, `trigger` (`failure`, `cooldown` for the first attempt after one, or `operator`) |
| `service_recovery_exhausted` | `max_attempts`, `window_seconds`, `cooldown_seconds`, `resume_at` (absent with `cooldown: 0`) |

`endpoint` is the URL's scheme, host and port only: the path and the
query may hold a token. Events go to the channels in the service's
`notifications`. Without a route, they are only logged. Either way, the
last ones are shown by `sentinelctl events`.

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

Process services:

| Code | Meaning |
|---|---|
| `exited` | the process exited by itself (`exit_code`, 0 included) |
| `killed` | a signal ended it (`signal`) |
| `not_found` | the program or the working directory is missing |
| `permission_denied` | not allowed: an identity change sentineld may not make, or no execute permission |
| `unsupported` | an identity change, or a memory limit, outside Linux |
| `untrusted_executable` | the program could be changed by another user (see above) |
| `output_failed` | the output file could not be opened safely |
| `exec_failed` | any other start failure |
| `cleanup_failed` | what a previous run left in its group could not be killed: the service is not started, and the cleanup is tried again every `max_delay` |
| `memory_limit` | sentineld stopped it: its resident memory was over `limits.memory` (`rss_bytes`, `limit_bytes`) |

## State across restarts

Each service's health, counters and open incident are kept in
`<state_dir>/supervisor/state.json` (root-only, `0600`). A restart
therefore neither repeats a `service_failed` nor forgets an incident.

The file is written on every transition and at most every 60 s
otherwise. It holds no URL, header, body, command, argument or
environment, only a digest of each service's definition, and for process
services their start and restart counts, last exit, recovery attempts
and exhaustion, and the process group to clean up; and what operators
asked (`stopped`, `disabled`).
Schema 2 (D-086); a file of schema 1 (v0.1.0) is migrated at the first
start and v0.1.0 then refuses the new one instead of misreading it. When the definition changes (URL, request keys,
TLS, `address`, timeout or failure policy), the service starts again
from `unknown`. If it was failed, its incident is then closed without a
`service_recovered`. Changing only `check_interval` or `notifications`
keeps the state. For a process service every key except `notifications`,
`recovery`, `limits` and `enabled` is part of its definition.

If the state file cannot be written, supervision goes on, and
`sentinelctl supervisor status` shows `state file: NOT SAVED`.

## Commands

The reading commands need the `read` tier (members of
`daemon.socket_group`); the operator's commands, the `operate` tier
([Operator commands](#operator-commands)):

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

`list` marks disabled services, and `status` says who disabled a service
(`configuration` or `operator`) and who holds a process stopped.

For a process service it shows the phase (`not_armed`, `starting`,
`running`, `exited`, `start_failed`, `restarting`, `exhausted`,
`blocked`, `stopping`, `stopped`, `disabled`), the pid and uptime, the starts,
failures and restarts, the attempts in the window, the next attempt or
the end of the cooldown, why a launch is blocked, the last exit, the
open incident, `output LOST` when the output file failed, and with
`limits.memory` the last sample against the limit (or `UNKNOWN` while
samples fail). It never shows the program,
its arguments, its environment or its user. The header says when process
services are not armed, how many groups left by a previous run are
still to be cleaned up, and when the state file was corrupt at start.

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
| A process service stays `not_armed` | Set `modules.supervisor.processes: true` in the central file. |
| `untrusted executable` | The program, a directory on its path or a link to it is writable by group or others, or owned by another user. Fix the modes and owners. |
| `cleanup_failed` | A process left by an earlier run did not die (often stuck in the kernel, state `D`). Find it by its process group and deal with it; sentineld tries again every `max_delay`. |
| A process service stays `exhausted` | It failed `max_attempts` times in `window`. Fix the cause; it starts again at the end of `cooldown` (never with `cooldown: 0`), at once after `sentinelctl supervisor reset`, or if you change its definition. |
| A process service stays `stopped` or `disabled` | An operator stopped or disabled it (`status` says who): `sentinelctl supervisor start` or `enable`. `disabled` by `configuration`: set `enabled: true`. |
| `operator_stop_failed` | The process did not end within `stop_timeout` and `SIGKILL` (often stuck in the kernel). It runs until sentineld restarts; the name takes no command until then. |
| `conflict` from a command | The service's state does not allow it: the message says why and what to run. |
| A process service stays `blocked` | `audit_unavailable`: the audit chain is broken (`sentinelctl status`); restore the evidence and restart sentineld. `state_unavailable`: the state directory cannot be written. |
| `permission_denied` with `user` | sentineld is not root, or lacks `CAP_SETUID`/`CAP_SETGID`. |
| `memory_limit` again and again | The program needs more than its limit, or leaks. `supervisor status` shows the last sample: raise `limits.memory` or fix the program; the budget bounds the restarts meanwhile. |
| Memory shows a few MiB for a large program | The limit measures the leader only: a `script` without `exec` measures its shell, a forking server its master. |
| Memory `UNKNOWN` | `/proc/<pid>/statm` could not be read (see sentineld's log). The process runs unchecked until a sample succeeds. |
