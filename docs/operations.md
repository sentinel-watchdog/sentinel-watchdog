# Operations

How to install, run and look after `sentineld` and `sentinelctl` as they
are today: the core and the Supervisor's HTTP checks (Phase 3a).
Reference material lives elsewhere and is linked, not copied:
[configuration](configuration.md), [supervisor](supervisor.md),
[notifications](notifications.md),
[security](security.md), [architecture](architecture.md).

> First version (Phase 2c-4). There are no packages and no published
> release yet: binaries come from a snapshot build (below). Packages,
> a hardened systemd unit and install scripts arrive in Phase 3d.

## Install from a snapshot

`task snapshot` (or the `snapshot` artifact of a CI run) produces in
`dist/`:

| File | Content |
|---|---|
| `sentinel-watchdog_<version>-snapshot_linux_<arch>.tar.gz` | `sentineld`, `sentinelctl`, `LICENSE`, `README.md`, `CHANGELOG.md`, `examples/sentinel.yaml`, `examples/supervisor/http.yaml`; `amd64` and `arm64` |
| `<binary>_<version>-snapshot_linux_<arch>.cdx.json` | CycloneDX SBOM of each binary: the Go modules and standard library linked into it |
| `checksums.txt` | SHA-256 of every file above |

Binaries are static (`CGO_ENABLED=0`): one build runs on glibc and musl
(Alpine) systems. Builds are reproducible: the same commit gives the same
bytes (version and dates come from the commit, not from the build time).

```sh
sha256sum -c checksums.txt --ignore-missing
tar -xzf sentinel-watchdog_<version>-snapshot_linux_amd64.tar.gz
cd sentinel-watchdog_<version>-snapshot_linux_amd64
install -o root -g root -m 0755 sentineld sentinelctl /usr/local/bin/
sentineld -version
```

A checksum proves the file is the one the build produced, not who built
it: snapshots are not signed (signatures come with releases, Phase 3d).

## Files and modes

| Path | Owner, mode | Notes |
|---|---|---|
| `/usr/local/bin/sentineld`, `sentinelctl` | root, `0755` | |
| `/etc/sentinel/` | root, `0750` (or `0755`) | not writable by group or others, parents included ([configuration](configuration.md#file-ownership-and-modes)) |
| `/etc/sentinel/sentinel.yaml` | root, `0640` or `0600` | never writable by group or others |
| `/var/lib/sentinel/` (`daemon.state_dir`) | root, `0750`, created if missing | state files `0600`, audit log `0600` |
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
variable, stops sentineld at start. Static builds resolve groups from
`/etc/group` only, not LDAP or SSSD.

Check before starting or reloading; neither command changes anything:

```sh
sentineld -validate -config /etc/sentinel/sentinel.yaml
sentinelctl validate -all     # also reads the directories of disabled modules
```

## Run under systemd

An **example** unit, enough to run the core; it is not hardened
(capability bounding, sandboxing and the packaged unit are Phase 3d):

```ini
# /etc/systemd/system/sentineld.service — example
[Unit]
Description=Sentinel Watchdog agent
After=network-online.target
Wants=network-online.target

[Service]
Type=notify
NotifyAccess=main
ExecStart=/usr/local/bin/sentineld -config /etc/sentinel/sentinel.yaml
ExecReload=/usr/local/bin/sentinelctl reload
# Longer than daemon.shutdown_timeout (30s by default).
TimeoutStopSec=45s
Restart=on-failure
RuntimeDirectory=sentinel
RuntimeDirectoryMode=0755
StateDirectory=sentinel
StateDirectoryMode=0750
# Secrets for ${VARIABLE} references: root-only file (configuration.md).
# EnvironmentFile=/etc/sentinel/secrets.env

[Install]
WantedBy=multi-user.target
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
| `read` | anyone who can open the socket (`socket_group`) | `status`, `modules`, `events`, `audit`, `config show` |
| `operate` | root, `access.operator_group` | `reload` |
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
sentinelctl supervisor list            # supervised services and their state
sentinelctl supervisor status [name]   # details: endpoint, last check, failure code, incident
```

## Audit log

Every `operate` and `admin` request — accepted, failed or denied — and
every reload is recorded in `<state_dir>/audit.jsonl` before it runs, and
copied to the journal. Records are hash-chained; `audit_head.json` holds
the last one. Rotation keeps five files of 10 MiB (fixed). Read the tail
and the integrity with `sentinelctl audit [-n N]`.

If a record cannot be written, the request is refused (fail closed). If
the start-up check finds the chain **broken** (a file edited, removed or
reordered), `sentinelctl status` and `audit` report it, the event
`audit_integrity_broken` is sent, and every new record — so every
`operate`/`admin` request — is refused. sentineld never repairs the
chain. To recover: keep the evidence (copy `audit.jsonl*` and
`audit_head.json` off the host), investigate, move the files out of the
state directory, then restart sentineld, which starts a new chain
(ADR-0011, D-077).

## Logs

sentineld logs to stderr: under systemd the journal format is chosen
automatically (`daemon.log.format: auto`), so `journalctl -u sentineld`
shows levels as priorities. Secrets are redacted. `daemon.log.level`
changes on reload.

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| `... is writable by group or others` at start | a configuration file, directory or parent has group/other write bits: `chmod go-w` (or own it by root) |
| `group "...": ... unknown group ...` at start | a `socket_group` or `access` group is missing from `/etc/group` (static builds do not read LDAP/SSSD) |
| `environment variable "NAME" is not set` | a `${NAME}` in the configuration is not set: add it to the unit's `EnvironmentFile` |
| start fails: socket in use or locked | another sentineld runs (the lock `<socket>.lock` is held); stop it first. A stale socket with nobody listening is removed automatically |
| `sentinelctl`: permission denied | the user cannot open the socket: add them to `socket_group` and log in again |
| `sentinelctl`: `permission_denied` from the daemon | the user's tier is too low for the command |
| `sentinelctl`: `unavailable` | the daemon is stopping, or runs outside Linux (no peer credentials) |
| `systemd notifications disabled` warning | `NOTIFY_SOCKET` is not a valid address; under `Type=notify` the start then times out: check the unit |
| reload rejected | `sentinelctl reload` lists every problem; `sentineld -validate` shows the same offline |
| status shows the audit chain broken | see [Audit log](#audit-log); requests that change something are refused until then |
