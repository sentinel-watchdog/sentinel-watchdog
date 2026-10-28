# Backlog

Work that is known but not scheduled in a phase of [PLAN.md](../PLAN.md).
An item moves to a phase (and leaves this list) when it is planned; a
finding that is real but outside the change that found it lands here as
**Deferred** ([development workflow](development-workflow.md), section 3).

Deferred findings and follow-ups have an ID (`B-xxx`, never reused),
their origin and a severity that guides priority. Features are grouped by
area; a feature gets an ID when it is picked for planning.

## Deferred findings and follow-ups

| ID | Area | Item | Origin | Severity |
|---|---|---|---|---|
| B-001 | config | Line numbers for semantic configuration problems. YAML-level problems already carry `line N:`; semantic ones (validation of values) carry file and dotted path only. Add a line to `config.Problem` and locate findings from the parsed nodes. | Phase 2c-1 planning, 2026-10-09 | low |
| B-002 | CI | Fuzzing in CI (nightly job), once the fuzz targets' run time is known. | Agent workflow, D-069 | low |
| B-003 | tooling | A Claude Code hook running `task check` (slow; CI enforces the same checks). | Agent workflow, D-069 | low |
| B-004 | daemon | Logging that cannot block sentineld: a log destination that stops reading (a full stderr pipe) blocks every log write, so shutdown is bounded only for blocked routers and the event logger. Needs asynchronous logging with a bounded queue and dropped-record accounting (a logging design change). | Codex review of Phase 2c-1, 2026-10-09 (D-075) | medium |
| B-007 | daemon | `Type=notify-reload`: send `RELOADING=1` with `MONOTONIC_USEC` and `READY=1` around every reload, socket-triggered ones included (needs a hook in `internal/daemon`), and decide how SIGHUP coalescing (D-078) answers systemd's own SIGHUP. Today `ExecReload=sentinelctl reload` covers it. | Phase 2c-4 planning, 2026-10-15 (D-079) | low |
| B-008 | daemon | `WATCHDOG=1` keep-alives for systemd's `WatchdogSec=`, tied to a real liveness signal of the daemon (not a timer that always fires). | Phase 2c-4 planning, 2026-10-15 (D-079) | low |
| B-009 | CI | Reproducibility check in CI: build the snapshot twice (different time zone and directory) and compare `checksums.txt`. Checked by hand in 2c-4. | Phase 2c-4 planning, 2026-10-15 (D-079) | low |
| B-010 | upstream | Report the Ubuntu kernel `7.0.0-1014-aws` AppArmor oops (`update_sk_ctx`, NULL dereference on `write(2)` to a connected AF_UNIX datagram socket from a confined container; unprivileged host DoS) on Launchpad with the reproducer; then track the fix and retest R-024. | Phase 2c-4 test-linux, 2026-10-15 (D-079) | medium |
| B-011 | notify | Log the final failure of a webhook delivery (today only the `Failed` counter shows it): the channel name and the error, redacted (an injected `Sender` has no URL-free contract), without noise for the cancellation at shutdown. | Design review Q5, 2026-10-16 | low |
| B-012 | core | One definition of the module-name grammar (today in `module`, `events`, `state`, and embedded in the `api` command and `audit` action formats) and one table of the core event types (`events.NewRegistry` and `model.CoreEventTypes`). | Design review Q6, Q14, 2026-10-16 | low |
| B-013 | core | "Open non-blocking, check the opened file, read bounded" is written in config, state, audit and tlsconfig with different contracts (audit adds `Lstat` + `O_NOFOLLOW` and streams; config and CA files allow constrained symlinks). Extract only a helper that keeps every caller's guarantees, error classification and wording; never weaken audit's. | Design review Q7 (Codex rejected the blanket helper), 2026-10-16 | low |
| B-014 | archtest | Partly done in 3b-1 (#33): `tests/integration` is an allowed place and nothing outside `tests` may import it. Remaining: refuse imports between test areas (`tests/integration/a` → `tests/integration/b`), and a short amendment of ADR-0015's rules naming `tests/integration`; YAML ownership unchanged. | Design review Q9, 2026-10-16; status 2026-10-20 | low |
| B-015 | daemon | Structure, when nearby work benefits: notification generations (forward, swap, replaceDispatcher) in their own file, `apply` helpers only where they simplify parameters and invariants, audit verification in `verify.go`, `sentineld`'s run loop as `serve`, focused `sentinelctl` renderers. Behaviour-preserving; no new layer. | Design review S1, S2, S4, S5, S8, 2026-10-16 | low |
| B-016 | tests | Replace the wall-clock sleeps and polling in `reload_test.go` and `audit_test.go` with synchronisation or targeted `synctest`, and split `reload_test.go` by topic; real socket and file-system harnesses stay real. | Design review S3, 2026-10-16 | low |
| B-017 | tooling | Evaluate lint rules one at a time with a demonstrated benefit (`gocognit`, `exhaustive`, a `forbidigo` rule for direct `time` calls outside `clock` and tests), not as a bundle with thresholds fitted to today's code. | Design review S6, 2026-10-16 | low |
| B-018 | platform | `sdnotify` retries on `clock.Clock` with an unexported send seam, so the retry policy is tested without filling a kernel queue; keep the real `sendto` adapter tests (D-079). | Design review S7, 2026-10-16 | low |
| B-020 | daemon | A module Start that the daemon abandoned (it did not return in time) and that returns successfully later is left running with no owner, marked `abandoned` (D-078: never a replacement beside it; the lock and state directory are kept at shutdown). The supervisor launches nothing when Start's context has already ended, but a cancellation between its last check and the start of its loops still leaves running processes until sentineld exits (Pdeathsig kills them, the next start cleans up their groups). Have `daemon.call` stop a late-successful abandoned Start within the module's budget, for every module. | Codex review of Phase 3b-2 fixes (R1 residual), 2026-10-22 | low |
| B-021 | supervisor | A memory stop waits for its audit intent before `Stop`; `audit.Log.Record` honours ctx only while acquiring its semaphore, so an intent write stuck in the kernel keeps the over-limit process running, contrary to D-090 (5) "at most 5 s". Bound the wait independently of `Record` returning, with an owned, bounded writer. | Full review 2026-10-28 (Codex C2) | medium |
| B-022 | supervisor | Adopting a running process after `endLoop` may race its exit: a false `service_recovered` or `during_startup` (`runProcess` adoption, `process.go:~96`, and `watch`, `process.go:585-619`). Not verified; needs a synctest scenario. | Full review 2026-10-28 (agy N6) | medium |
| B-023 | docs | `docs/security.md` still describes Phase 2c ("nothing runs through a shell", "the core starts no process") and `docs/configuration.md` says no module is implemented (`:11`) and lists `supervisor` as planned (`:306`); v0.2.0 runs processes and `script:` through `/bin/sh -c`. | Full review 2026-10-28 (F11) | medium |
| B-024 | daemon | `validate` does not read `notifications.channels[].tls.ca_file`: `daemon.Validate` never builds the dispatcher, so a bad bundle is found at start or reload only. | Full review 2026-10-28 (agy N9) | medium |
| B-025 | tests | `internal/core/fstrust` has no direct tests of `CheckLink`, `OpenDir`, `CheckOpened` (covered only through config, tlsconfig, state, transport); `TestClient` never handshakes; `TestStartErrors` has cases that pass on any error; `validate -all` is tested on a file with nothing disabled; `validate` failures assert only `"error: "`; `internal/version` has no test. | Full review 2026-10-28 (lens-tests T-01, T-02, T-03, T-05, T-08, T-12) | low |
| B-026 | tests | More real-time waits to replace (see B-016): `sdnotify_linux_test.go` sleeps, executor helper sleeps, the integration test's 200 ms negative check, `redact_test.go`'s wall-clock budget. | Full review 2026-10-28 (lens-tests T-07, T-09, T-10, T-11) | low |
| B-027 | config | Validation gaps: `/` accepted as `state_dir`, `socket`, `ca_file`, `output.file`; channel header values (`notifications.channels[].headers`) allow control bytes other than CR, LF, NUL; case-variant duplicate header names and out-of-range URL ports pass and fail at delivery. | Full review 2026-10-28 (agy N7, N10) | low |
| B-028 | supervisor | `supervisor status` counts a retired process still running as a pending cleanup (`runningProcesses` reads only `in.services`). | Full review 2026-10-28 (agy N12) | low |
| B-029 | supervisor | `processRedactor` sorts secrets by length only before `slices.Compact`, leaving duplicates (extra passes per output line, redaction correct). | Full review 2026-10-28 (F9) | low |
| B-030 | CI | The CI job runs zizmor without `--persona=auditor`, weaker than `task lint-actions`. | Full review 2026-10-28 (F10) | low |
| B-031 | config | The comment at `section.go:69` credits `checkStructure` with bounding alias expansion; it is yaml.v3's decoder. A block over yaml.v3's alias limit makes `Equal` false, so a reload treats it as changed every time. | Full review 2026-10-28 (F1) | low |
| B-032 | notify | A replaced dispatcher's webhooks keep idle connections until `IdleConnTimeout` (90 s): close them when the dispatcher ends. | Full review 2026-10-28 (F7) | low |
| B-033 | transport | The socket's `<socket>.lock` has the exposure fixed for `sentineld.lock` (D-092): a lock file readable by others lets them hold the `flock` and keep sentineld from starting. It is created 0600 in a checked directory, so an admin's chmod comes first; require 0600 as `state.lock` does. | Full review 2026-10-28, code review R5 | low |

## Features

**Core:** Slack and Teams notification providers; Prometheus metrics
(Sentinel's own telemetry only); local HTTP API; APK package.

**Supervisor:** port, mount, log, advanced resource and `process_group`
services; OpenRC service type; cgroups v2 limits; recovery action
`execute`; container restart actions.

**Firewall:** HTTPS blocklists with checksum/signature (Q-011);
threat-intelligence feeds; Fail2ban integration; containerd adapter; more
WAF/proxy adapters (HAProxy, Caddy, Envoy, generic); admission
integration; eBPF/CNI-specific policies; `DOCKER-USER` / forward rules
(explicit opt-in); advanced remediation workflows; out-of-process
firewall agent (R-015).

**Future directions:** OpenObserve, Prometheus and Grafana integrations;
host integrity; inventory/vulnerability triage; operational/security
correlation. All require discovery (D-067,
[candidate roles](future-modules.md)).
