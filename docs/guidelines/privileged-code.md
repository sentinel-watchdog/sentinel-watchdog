# Privileged code (sentineld runs as root)

Applies to changes in `internal/core/{config,state,audit,transport,authz}`,
`internal/platform`, `internal/modules`, `internal/daemon`, `cmd`,
`deploy` and `packaging`, for humans and AI agents alike.

## Untrusted input

Treat as attacker-controlled until validated: configuration files and
directories, environment variables, CLI arguments and socket requests,
file and directory names, symbolic and hard links, files owned by
non-root users, output of external processes, network data, and state
or audit files written by a previous run.

## Required in every change to these paths

The PR description states:

1. **Trust boundary**: which input crosses from less to more privilege.
2. **Abuse scenario**: what a local user, a malicious config author or a
   compromised remote endpoint could try, and why it fails.
3. **Regression test** for that scenario (fails without the protection).
4. **Privilege justification**: which capability, file mode or
   ownership the code needs, and why less is not enough.

## Filesystem

- Open trusted directories once with `os.OpenRoot` and resolve every file
  relative to them; never re-resolve a checked path by name.
- Check ownership (root or the daemon's euid) and write bits on the opened
  descriptor, and on every parent directory up to `/` (writable parents
  only with the sticky bit) — ADR-0015 rule 8.
- Do not follow a symbolic link out of its directory; reject or confine.
- Open with `O_NONBLOCK` (and `O_NOFOLLOW` for files Sentinel creates);
  reject non-regular files before reading.
- Write atomically: temp file in the same directory, `fsync`, rename,
  `fsync` of the directory; explicit modes (`0600` state, `0640` logs).
- Bound every input: bytes per file, files per directory, total bytes,
  nodes walked, growth from expansion. Charge the budget before
  allocating, including on failed reads.

## Processes and the system

- Execute with an argv slice (`exec.Command(path, args...)`), absolute
  paths, explicit environment; never through a shell built from input.
- Every external call has a timeout through `context.Context` and its
  output is size-limited before parsing.
- Changes to the system (restart, firewall) are idempotent where possible,
  audited before they happen, verified after, and roll back on failure
  (ADR-0003, ADR-0011).

## Failure and secrets

- On invalid or incomplete configuration, refuse to start or keep the
  previous valid configuration; never fall back to a permissive default.
- Recover panics only at module boundaries and report them without the
  panic payload (it may contain secrets).
- Error messages never echo secret values (URLs with tokens, headers,
  environment values): use the core redaction package.
