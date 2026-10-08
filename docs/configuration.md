# Configuration reference

Sentinel is configured in YAML only. This page documents schema
`version: 1` as implemented by `internal/core/config` (Phase 2a): the
central file and the rules every module directory follows. Each module
documents its own keys when it is implemented (the supervisor in Phase 3a).
Design: [ADR-0015](adr/0015-modules-and-configuration-layout.md).

> **Status.** The loader and its validation exist; there is no runnable
> daemon yet (Phase 2c) and no module is implemented yet (planned modules
> are rejected when enabled).

- [Files and load order](#files-and-load-order)
- [File ownership and modes](#file-ownership-and-modes)
- [Strictness](#strictness)
- [Environment variables and secrets](#environment-variables-and-secrets)
- [Value formats](#value-formats)
- [daemon](#daemon)
- [notifications](#notifications)
- [modules](#modules)
- [Module directories](#module-directories)
- [For module authors](#for-module-authors)

## Files and load order

```
/etc/sentinel/
├── sentinel.yaml      central file: daemon, notifications, module switches and safety gates
├── supervisor/        read only when modules.supervisor.enabled is true
│   ├── 10-nginx.yaml
│   └── 20-backup.yaml
└── firewall/          read only when modules.firewall.enabled is true
    └── policies.yaml
```

1. The central file is read first (default `/etc/sentinel/sentinel.yaml`).
   Its top-level keys are `version`, `daemon`, `notifications` and
   `modules`.
2. For every **enabled** module, the directory `<dir of the central
   file>/<module name>/` is read: regular `*.yaml` and `*.yml` files in
   lexical byte order. Hidden files and other extensions are ignored;
   subdirectories are ignored with a warning. There is no per-module path
   override.
3. The directory of a **disabled** module — or of a module not named
   under `modules` — is not read at all, so its files may reference
   environment variables that are not defined yet. `sentinelctl validate
   --all` (Phase 2c) reads the directories of every available module
   anyway; the daemon reports unread directories as ignored.
4. Every file — central or module — holds exactly one YAML document whose
   top level is a mapping, declares `version: 1`, and is at most 4 MiB.

Only the central file configures the core and arms modules. A file in a
module directory describes *what* the module does and can never enable a
module, change the socket or the notification channels, or relax a
safety gate (ADR-0015 rule 1).

## File ownership and modes

Before reading anything, the loader checks:

- every **parent** of the configuration directory, up to `/`;
- the configuration directory itself, the central file, each module
  directory that is read, and each file in it.

Each must be owned by **root or by the user running `sentineld`**, and must
not be writable by group or others. A parent directory writable by others
is accepted only with the sticky bit (as `/tmp`), where other users cannot
rename entries they do not own. Otherwise another local user could change
what a root daemon executes, for example by creating a module directory
for an enabled module that has none (D-070).

```
/etc/sentinel/sentinel.yaml: is writable by group or others (mode 0664); remove the write bits with chmod go-w
```

The configuration directory is opened once and every later file is
resolved relative to that open directory (Go's `os.Root`), and each file is
checked after it has been opened: the file that is checked is the file
that is read. Consequences:

- **symbolic links must stay inside their directory**: the central file
  may link to another file of the configuration directory, a module file
  to another file of the same module directory; a link that leads outside
  is an error;
- named pipes, sockets and devices in a module directory are ignored with
  a warning, and opening a file never blocks.

## Strictness

The loader rejects, with file and line number:

- unknown keys at any level (with the list of valid keys);
- duplicate keys in the same mapping (including a module listed twice);
- wrong value types (`shutdown_timeout: 30`, `enabled: "yes"`);
- unknown module names, and enabling a module that is planned but not
  implemented in this release or not built into this binary;
- unknown or not-yet-implemented notification types and unknown enum
  values;
- more than one YAML document per file, and excessive YAML alias
  expansion;
- duplicate keys in any mapping (such as `enabled: true` followed by
  `enabled: false`), and YAML merge keys (`<<:`), which add keys that are
  not written in the file (D-071). Anchors and aliases are allowed.

Size limits: 4 MiB per file, 1000 files per module directory, 16 MiB in
total, and environment variables may add at most 4 MiB to a file.

All problems are collected and reported together:

```
invalid configuration (2 problem(s)):
  - /etc/sentinel/sentinel.yaml: line 7: unknown field "sockets" (valid fields: access, log, shutdown_timeout, socket, socket_group, socket_mode, state_dir, timezone)
  - /etc/sentinel/sentinel.yaml: notifications.channels[ops].url: scheme must be http or https
```

Legal but risky settings produce **warnings** (plain-HTTP webhook,
`insecure_skip_verify: true`, an `admin_group`, ignored subdirectories).

## Environment variables and secrets

Scalar **values** may reference environment variables as `${NAME}`:

```yaml
url: ${SENTINEL_WEBHOOK_URL}
headers:
  Authorization: Bearer ${SENTINEL_WEBHOOK_TOKEN}
```

Rules (D-006):

- `NAME` must match `[A-Za-z_][A-Za-z0-9_]*`.
- An undefined variable is an error (an empty but defined variable is
  allowed). There is no `${NAME:-default}` syntax.
- `$${` produces a literal `${`. A `$` not followed by `{` is kept as-is.
- Keys are never expanded. Expansion happens after YAML parsing, so a
  variable value can never change the document structure.
- Expansion is not recursive.
- An unquoted value is re-typed after expansion (`attempts: ${N}`
  becomes an integer). Quote it to force a string: `"${N}"`.
- Inside a flow list (`[...]`) or flow mapping (`{...}`), quote values
  that start with `${`: an unquoted `{` would open a mapping.

**Secrets** belong in the environment, never in YAML. Under systemd, use a
root-only environment file referenced by a drop-in:

```ini
# /etc/systemd/system/sentineld.service.d/secrets.conf
[Service]
EnvironmentFile=/etc/sentinel/secrets.env   # mode 0600, owner root
```

`sentinelctl config show` (Phase 2c) prints a redacted view: webhook URLs
are reduced to scheme and host and header values are masked except for
well-known safe headers (`Content-Type`, `Accept`, `User-Agent`, …).
Modules redact their own sections.

Configuration problems never quote a value that came from an environment
variable: it appears as `[REDACTED]` (values shorter than 4 bytes are
too short to mask safely). YAML type errors do not quote the offending
value at all; the line number locates it.

## Value formats

| Kind | Format | Examples |
|---|---|---|
| Duration | Go duration string. Integers are rejected. | `500ms`, `15s`, `10m`, `2h`, `1h30m` |
| Size | Integer bytes or number + unit. `KB/MB/GB/TB` = powers of 1000, `KiB/MiB/GiB/TiB` = powers of 1024. | `1073741824`, `512MiB`, `1GB` |
| File mode | **Quoted** octal string. | `"0660"`, `"0640"` |
| Name | `^[a-z0-9][a-z0-9._-]{0,62}$` | `ops`, `api-health` |
| Module name | `^[a-z][a-z0-9_]{0,31}$` | `supervisor`, `firewall` |
| Path | Absolute and clean (no `..`, `.`, `//`, trailing `/`). | `/var/lib/sentinel` |
| Group | Account name or numeric id. | `sentinel`, `1001` |

An omitted field — or one set to its zero value — takes its default.
Explicit invalid values are rejected.

## daemon

```yaml
daemon:
  socket: /run/sentinel/sentinel.sock
  socket_mode: "0660"
  socket_group: sentinel
  state_dir: /var/lib/sentinel
  timezone: Local
  shutdown_timeout: 30s
  log:
    level: info
    format: auto
  access:
    operator_group: sentinel-operators
    admin_group: ""
```

| Key | Default | Validation / notes |
|---|---|---|
| `socket` | `/run/sentinel/sentinel.sock` | absolute, clean, ≤ 107 bytes |
| `socket_mode` | `"0660"` | owner must have read/write; world-writable rejected |
| `socket_group` | (none) | group allowed to connect: grants the `read` tier (ADR-0012) |
| `state_dir` | `/var/lib/sentinel` | absolute, clean; core and module state files live below it |
| `timezone` | `Local` | IANA name; the time zone database is embedded |
| `shutdown_timeout` | `30s` | 1s – 10m |
| `log.level` | `info` | `debug`, `info`, `warn`, `error` |
| `log.format` | `auto` | `auto` (journal under systemd, text otherwise), `text`, `json`, `journal` |
| `access.operator_group` | (none: root only) | may restart services (`operate` tier) |
| `access.admin_group` | (none: root only) | may change the firewall (`admin` tier): root-equivalent, warns |

## notifications

Channels shared by every module. Only `type: webhook` exists; `slack` and
`teams` are reserved and rejected as "planned but not implemented".
Delivery is implemented in Phase 2b.

```yaml
notifications:
  channels:
    - name: ops
      type: webhook
      url: ${SENTINEL_WEBHOOK_URL}
      method: POST
      timeout: 10s
      headers:
        Authorization: Bearer ${SENTINEL_WEBHOOK_TOKEN}
      retry: {attempts: 3, delay: 5s, backoff: exponential, max_delay: 1m}
      success_status_codes: [200, 202, 204]
      tls: {ca_file: /etc/pki/internal-ca.pem}
```

| Key | Default | Validation / notes |
|---|---|---|
| `name` | required | name format, unique |
| `type` | required | `webhook` |
| `enabled` | `true` | |
| `url` | required | `http` or `https` with host; `http` warns |
| `method` | `POST` | `POST`, `PUT` |
| `timeout` | `10s` | 100ms – 5m, per attempt |
| `headers` | `Content-Type: application/json` added if absent | valid header names; values without CR, LF or NUL |
| `retry.attempts` | `3` | 1 – 10, including the first attempt |
| `retry.delay` | `5s` | 0 – 24h |
| `retry.backoff` | `exponential` | `fixed`, `exponential` |
| `retry.max_delay` | `1m` | 0 – 24h, ≥ `delay` |
| `success_status_codes` | any 2xx | 100 – 599 |
| `tls.ca_file` | system roots | absolute path to a PEM bundle |
| `tls.server_name` | from the URL | |
| `tls.insecure_skip_verify` | `false` | warns |

## modules

One entry per module. `enabled` is the module switch (default `false`);
the other keys of the block are the module's **safety gates and
switches**, documented by each module (for example the firewall's `mode`
and `dry_run`, ADR-0003).

```yaml
modules:
  supervisor:
    enabled: true
  firewall:
    enabled: false
    mode: read_only
    dry_run: true
```

- A name that this binary does not know is an error, even when disabled
  (typos are caught).
- Enabling a module that is **planned** (not implemented in this release)
  or **not built** into this binary (excluded with a build tag) is an
  error. Listing it disabled is allowed.
- An empty block (`supervisor:`) means "disabled".

Known modules today: `supervisor` (Phase 3), `firewall` (Phase 4) and
`remote` (Phase 5), all planned. Future modules are selected through
discovery first (D-067).

## Module directories

For an enabled module, `<dir of sentinel.yaml>/<module>/` holds its
content. Common rules (ADR-0015 rule 6):

- every file declares `version: 1`;
- files are read in lexical byte order (use prefixes such as `10-`, `20-`);
- lists in different files are merged by the module; names must be unique
  within the module;
- a `settings` block may appear in **at most one** file of the directory;
- whether the directory is required is the module's decision: the
  supervisor starts empty with a warning, the firewall refuses to start.

## For module authors

`config.Load` never imports a module. The daemon passes the names it knows
(`LoadOptions.Modules`, from `module.Registry.Availability`), and each
module receives a `config.ModuleConfig`:

| Field | Content |
|---|---|
| `Name`, `Enabled`, `Availability` | the module switch and what this binary can do |
| `Central` | the module's block in the central file, without `enabled` |
| `Dir`, `DirExists` | the module directory and whether it exists |
| `Files` | one `config.Section` per file, in order, without `version` |

Decode a section with `Section.Decode(&v)`: unknown keys and type
mismatches become a `*config.ValidationError` whose problems carry the
file and line. Return problems from `Configure` the same way, so that
`sentinelctl validate` reports everything at once.
