# Operations

How to install, run and look after `sentineld` and `sentinelctl` as they
are today: the core and the supervisor. References:
[configuration](configuration.md), [supervisor](supervisor.md),
[notifications](notifications.md), [security](security.md),
[architecture](architecture.md).

Installing, upgrading and removing, from a release or from a snapshot, is
in [install](install.md).

## Files and modes

| Path | Owner, mode | Notes |
|---|---|---|
| `/usr/bin/sentineld`, `sentinelctl` (packages) or `/usr/local/bin/` (tarball) | root, `0755` | |
| `/etc/sentinel/` | root, `0750` (or `0755`) | not writable by group or others, parents included ([configuration](configuration.md#file-ownership-and-modes)) |
| `/etc/sentinel/sentinel.yaml` | root, `0640` or `0600` | never writable by group or others |
| `/var/lib/sentinel/` (`daemon.state_dir`) | root, `0750`, created by the package or, if missing, by sentineld | state files `0600`, audit log `0600` |
| `/run/sentinel/sentinel.sock` (`daemon.socket`) | root, group `daemon.socket_group`, `daemon.socket_mode` (`0660`) | its directory is created `0755` if missing |

sentineld sets umask `027` whatever it inherits.

## Configure

A minimal central file with no module and no notification is valid:

```yaml
# /etc/sentinel/sentinel.yaml
version: 1
```

The defaults then apply: socket `/run/sentinel/sentinel.sock` usable by
root only, state in `/var/lib/sentinel`, logs at `info`. To let a group
read the status, add `daemon.socket_group`; to let one reload, add
`daemon.access.operator_group`
([configuration: daemon](configuration.md#daemon)).

`examples/sentinel.yaml` in the archive is a commented reference, not a
ready file: it names the groups `sentinel` and `sentinel-operators` and
the variables `SENTINEL_WEBHOOK_URL` and `SENTINEL_WEBHOOK_TOKEN`, which a
host may not have. A group that does not exist, or an undefined
variable, stops sentineld at start. Static builds resolve group names from
`/etc/group` only, not LDAP or SSSD: give such a group by its numeric id.

Check before starting or reloading; neither command changes anything:

```sh
sentineld -validate -config /etc/sentinel/sentinel.yaml
sentinelctl validate -all     # also reads the directories of disabled modules
```

## Run under systemd

The packages install `/usr/lib/systemd/system/sentineld.service`
([deploy/systemd/sentineld.service](../deploy/systemd/sentineld.service),
D-102): `Type=notify`, `ExecReload=sentinelctl reload`, `Restart=on-failure`,
`TimeoutStopSec=45s` (longer than `daemon.shutdown_timeout`). sentineld
creates `/run/sentinel` and `/var/lib/sentinel` itself and refuses unsafe
ones; the unit has no `RuntimeDirectory=` or `StateDirectory=`, which would
let systemd fix their modes first and hide a tampered directory.

Two settings must match the unit when you change them:

- `daemon.shutdown_timeout` above 40 s: raise `TimeoutStopSec=` in a
  drop-in too, or systemd kills sentineld before it has stopped.
- `daemon.socket` other than the default: `ExecReload=` calls `sentinelctl
  reload`, which uses the default socket; override it with
  `ExecReload=/usr/bin/sentinelctl reload -socket <path>` (an empty
  `ExecReload=` line first).

sentineld runs as **root without sandboxing**: no `CapabilityBoundingSet=`,
`ProtectSystem=` or `NoNewPrivileges=`. The processes and jobs it supervises
inherit every restriction of the unit, so one that suits sentineld can
break a backup or package-upgrade job (R-002; evaluation in backlog B-048).
Add one with a drop-in only when it suits what the host supervises. The
default `KillMode=control-group` means stopping or restarting sentineld
stops what it supervises; it starts those again when it starts.

Changes go in drop-ins (`systemctl edit sentineld`), never in the packaged
file. Secrets for `${VARIABLE}` references (root-only file,
[configuration](configuration.md)):

```ini
# /etc/systemd/system/sentineld.service.d/secrets.conf
[Service]
EnvironmentFile=/etc/sentinel/secrets.env
```

A tarball install in `/usr/local/bin` copies `deploy/systemd/sentineld.service`
to `/etc/systemd/system/` and points it at the binaries:

```ini
# /etc/systemd/system/sentineld.service.d/local-bin.conf
[Service]
ExecStart=
ExecStart=/usr/local/bin/sentineld -config /etc/sentinel/sentinel.yaml
ExecReload=
ExecReload=/usr/local/bin/sentinelctl reload
```

With `Type=notify` sentineld tells systemd (sd_notify, D-079):

| When | Message |
|---|---|
| start complete: socket serving, every module started or reported failed | `READY=1` and a status line (`ready: N modules running`, or `ready, degraded: F of N modules not running`) |
| a signal arrived during the start (remaining modules skipped) | no `READY=1` |
| shutdown begins | `STOPPING=1` |
| reload | nothing; `ExecReload` waits for the answer instead |

`systemctl start sentineld` therefore returns once the control socket
answers, and fails if the core does not start. A module that failed to
start does not hold readiness back (D-075): `systemctl status` shows
`degraded` and `sentinelctl modules` names it. sentineld removes
`NOTIFY_SOCKET` from its environment, so processes it starts cannot speak
for it; an invalid value is logged (without the value) and ignored.
Outside systemd, without `NOTIFY_SOCKET`, nothing is sent. Each message
is one non-blocking datagram; while systemd's queue is full sentineld
retries for at most one second, then logs `systemd notification failed`
and goes on. sd_notify is implemented for Linux only.

`Type=notify-reload` is not supported yet (backlog B-007): use
`ExecReload=sentinelctl reload` as above. It is synchronous, audited with
the calling user, and exits non-zero when the new configuration is
rejected, so `systemctl reload` reports the failure.

## Run under OpenRC

The APK installs `/etc/init.d/sentineld`
([deploy/openrc/sentineld](../deploy/openrc/sentineld), D-102):
sentineld runs as root under `supervise-daemon`, restarted after a crash
at most 5 times a minute, its output in syslog (`logger -t sentineld`).

- `rc-service sentineld start` returns once sentineld answers on its
  control socket, within 20 s, and fails otherwise, for example with a
  configuration it rejects (see the system log).
- `rc-service sentineld reload` runs `sentinelctl reload`: synchronous,
  audited, non-zero when the new configuration is rejected.
- `stop` waits up to 45 s after SIGTERM, longer than
  `daemon.shutdown_timeout`; raise `retry` in a copy of the script if you
  raise that.
- `/etc/conf.d/sentineld` (root, `0600`, yours): export secrets for
  `${VARIABLE}` references there, and set `sentinel_socket=<path>` when
  `daemon.socket` is not the default (readiness and `reload` use it).
- A package upgrade or removal while `rc-service sentineld start` is still
  waiting for sentineld skips the restart or stop (`--ifstarted`): let the
  start finish first (backlog B-053).

The script does not use OpenRC's `notify="socket:ready"`: with OpenRC
0.62.6 (Alpine 3.22) `supervise-daemon` then leaves its socket behind at
stop, and every later start fails with `bind: Address in use`.

## Signals and exit codes

| Signal | Effect |
|---|---|
| `SIGTERM`, `SIGINT` | clean stop within `daemon.shutdown_timeout`; during start, cancels it; a second signal ends the process at once |
| `SIGHUP` | reload, as `sentinelctl reload` (a burst counts as one); the result is only logged |

| Exit | `sentineld` | `sentinelctl` |
|---|---|---|
| 0 | clean stop, or `-validate` found the configuration valid | success |
| 1 | invalid configuration, failed start of the core, or unclean stop (a module failed or was abandoned at start or stop, or deliveries were abandoned) | invalid configuration, daemon unreachable, request refused or failed |
| 2 | usage | usage |

## Reload

`sentinelctl reload [-timeout 2m]` (tier `operate`) or `SIGHUP`. The new
configuration is validated as a whole; any problem keeps the running one
and lists every problem. Otherwise only modules whose configuration
changed restart. `daemon.socket`, `socket_mode`, `socket_group`,
`state_dir` and `log.format` need a restart (a reload warns and keeps the
running value). Details: [configuration: reload](configuration.md#reload),
D-078.

## Control socket and tiers

`sentinelctl` talks to `daemon.socket`. The daemon reads the caller's
identity from the kernel and maps it to a tier (ADR-0012):

| Tier | Who | Commands |
|---|---|---|
| `read` | anyone who can open the socket (`socket_group`) | `status`, `modules`, `events`, `audit`, `config show`, `supervisor list\|status` |
| `operate` | root, `access.operator_group`, `access.admin_group` | `reload`, `supervisor start\|stop\|restart\|enable\|disable\|reset\|logs` |
| `admin` | root, `access.admin_group` | none yet (firewall, Phase 4) |

Group membership counts from the user's next login. `sentinelctl validate`
and `version` work without the daemon.

## Status

```sh
sentinelctl status        # version, uptime, configuration digest, modules, audit integrity, delivery counters
sentinelctl modules       # state of each module
sentinelctl config show   # effective core configuration, secrets redacted
sentinelctl status -json  # the same for scripts
sentinelctl events [-n 50]             # the last events, newest first (kept in memory: a restart empties them)
sentinelctl supervisor list            # services and jobs, and their state
sentinelctl supervisor status [name]   # details: endpoint, process phase and pid, unit state or job runs; last check, exit or run; failure code, incident
```

Acting on one service (tier `operate`, audited with its name as target;
[supervisor: operator commands](supervisor.md#operator-commands)). Jobs
take only `enable`, `disable` and `logs`:

```sh
sentinelctl supervisor stop worker      # stop and hold stopped (persisted)
sentinelctl supervisor start worker
sentinelctl supervisor restart worker
sentinelctl supervisor disable web      # not run or checked until enable (persisted; jobs too)
sentinelctl supervisor enable web
sentinelctl supervisor reset worker     # renew an exhausted recovery budget
sentinelctl supervisor logs worker -n 50 # read its recent output, oldest first
```

## Audit log

Every `operate` and `admin` request (accepted, failed or denied) and
every reload is recorded in `<state_dir>/audit.jsonl` before it runs.
Each record is also logged at `info`, so under systemd it reaches the
journal unless `log.level` is above `info`. A request that acts on a named object (a
supervisor service or job) carries it as `target`, validated, never the raw
argument. Records are hash-chained; `audit_head.json` holds
the last one. Rotation keeps five files of 10 MiB (fixed). Read the tail
and the integrity with `sentinelctl audit [-n N]`.

If a record cannot be written, the request is refused (fail closed). If
the start-up check finds the chain **broken** (a file edited, removed or
reordered), `sentinelctl status` and `audit` report it, the event
`audit_integrity_broken` is sent, and every new record, so every
`operate`/`admin` request, is refused. sentineld never repairs the
chain. To recover: keep the evidence (copy `audit.jsonl*` and
`audit_head.json` off the host), investigate, move the files out of the
state directory, then restart sentineld, which starts a new chain
(ADR-0011, D-077).

## Logs

sentineld logs to stderr: under systemd the journal format is chosen
automatically (`daemon.log.format: auto`), so `journalctl -u sentineld`
shows levels as priorities. Secrets are redacted. `daemon.log.level`
changes on reload. For the output of a supervised service or job, use
`sentinelctl supervisor logs <name>` ([logs](supervisor.md#logs)). Under
systemd that output reaches the journal as entries of its own, under
`sentineld/<name>`, outside sentineld's log format; journald's rate limit
(`RateLimitBurst=`) is shared by sentineld and every program it runs
([output in the journal](supervisor.md#output-in-the-journal)).

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| `... is writable by group or others` at start | a configuration file, directory or parent has group/other write bits: `chmod go-w` (or own it by root) |
| `group "...": ... unknown group ...` at start | a `socket_group` or `access` group is missing from `/etc/group` (static builds do not read LDAP/SSSD: use the group's numeric id) |
| `environment variable "NAME" is not set` | a `${NAME}` in the configuration is not set: add it to the unit's `EnvironmentFile` |
| start fails: socket in use or locked | another sentineld runs (the lock `<socket>.lock` is held); stop it first. A stale socket with nobody listening is removed automatically |
| start fails: `state directory ... is in use by another sentineld` | another sentineld uses the same `daemon.state_dir` (the lock `sentineld.lock` in it is held), whatever its socket; give each daemon its own state directory, or stop the other one. `sentineld.lock ... readable by group or others`: `chmod 600` it (anyone who can open it can hold the lock) |
| `sentinelctl`: permission denied | the user cannot open the socket: add them to `socket_group` and log in again |
| `sentinelctl`: `permission_denied` from the daemon | the user's tier is too low for the command |
| `sentinelctl`: `unavailable` | the daemon is stopping, or runs outside Linux (no peer credentials) |
| `systemd notifications disabled` warning | `NOTIFY_SOCKET` is not a valid address; under `Type=notify` the start then times out: check the unit |
| reload rejected | `sentinelctl reload` lists every problem; `sentineld -validate` shows the same offline |
| status shows the audit chain broken | see [Audit log](#audit-log); requests that change something are refused until then |
