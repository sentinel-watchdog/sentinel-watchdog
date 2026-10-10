# Development

How to build, test and change Sentinel Watchdog, how CI and the repository
are secured, and — phase by phase — the Go patterns the code uses (D-060).

## Repository scope and branches

This is the agent repository, including its local CLI. Product coordination,
dashboard, website and design delivery follow [project boundaries](project-layout.md).
Keep agent docs, decisions and release plans here; global research and roadmap
live in the maintainer's private coordination repository (D-073). No agent
build or check reads them.

Use a dedicated branch and PR for each change. Verify the full diff against
the target branch and include the checks in the PR. Other repositories need
their own branches and PRs. Runtime phase order is unchanged.

The step-by-step change workflow — local checks, independent review,
turning findings into tests, working with AI agents and when a human
review is mandatory — is in [development-workflow.md](development-workflow.md).

## Requirements

| Tool | Version | Used for |
|---|---|---|
| Go | 1.27 (from `go.mod`) | everything |
| [Task](https://taskfile.dev) | v3 | task runner (`Taskfile.yml`) |
| [golangci-lint](https://golangci-lint.run) | v2.14.0 (same as CI) | lint and import formatting |
| Docker (local or remote engine) | any recent | `task test-linux`, `task dev-shell`, the dev container |
| [uv](https://docs.astral.sh/uv/) | any recent | `task lint-actions` (runs zizmor) |
| [gh](https://cli.github.com) | any recent | `task labels`, pull requests |

## Tasks

| Task | What it does |
|---|---|
| `task check` | Offline CI checks: gofmt, `go mod tidy -diff`, vet, golangci-lint, tests, race tests, build |
| `task vuln` | govulncheck (pinned version, needs network) |
| `task lint-actions` | actionlint and zizmor (auditor persona) on `.github/workflows` |
| `task test-linux` | Unit tests in Linux containers as a non-root user: `golang:1.27` (glibc, race detector) and `golang:1.27-alpine` (musl) |
| `task cover` | Coverage report in `coverage.html` |
| `task fuzz` | Every `Fuzz*` target for `FUZZTIME` each (default `10s`) |
| `task fmt` | Format code (gofmt + goimports) |
| `task labels` | Create or update the GitHub labels |
| `task dev-shell` | Interactive shell in the dev container (Linux, Go, task, golangci-lint) with a copy of the working tree |

`task test-linux` streams the working tree (tracked and untracked,
non-ignored files) into the container as a tar archive on stdin instead of
bind-mounting it, so it works with a local engine and with a remote Docker
context alike; it prints which engine it uses. Choose one explicitly with
`DOCKER_CONTEXT=<context> task test-linux` (the maintainer uses the
`containers01` host). The code under test is sent to that engine.

## Editors and dev container

The repository carries shared editor settings; personal ones stay in your
user settings.

- **VS Code:** `.vscode/settings.json` (golangci-lint as linter, gopls with
  the `integration` tag so Linux-only and integration test files are
  analysed, the module as local import prefix, format and organise imports
  on save) and `.vscode/extensions.json` (Go, EditorConfig, Dev
  Containers). Other files in `.vscode/` are ignored by Git. No task runs
  when the folder opens.
- **Zed:** `.zed/settings.json` with the same gopls options and format on
  save. golangci-lint in Zed needs a separate extension and language
  server (optional); `task check` runs it anyway.
- **Dev container** (`.devcontainer/`): `golang:1.27` with golangci-lint
  and task at the versions of CI, as a normal user (`dev`, uid 1000) so
  permission tests behave as on a server. It is the way to run what only
  works on Linux, for example `sentineld` with `sentinelctl status` (peer
  credentials, D-076). VS Code (Dev Containers extension) and Zed (since
  v0.218; limited port forwarding) open it from `devcontainer.json`.

The Docker engine is never written in these files: it is the current
context (`docker context use <name>`, or `DOCKER_CONTEXT=<name>` in the
environment that starts the editor or the task). Where it runs changes how
the sources get in:

| Engine | How to use the dev container |
|---|---|
| Local (colima, Docker Desktop) | open the folder in the container: the working tree is mounted |
| Remote (for example `containers01`) | Docker cannot mount a local folder on a remote host: use **VS Code Remote-SSH** to the host with a clone there, then "Reopen in Container"; or **"Dev Containers: Clone Repository in Container Volume"** (the code lives in a volume on that host: push before cleaning up volumes) |
| Any, for a quick Linux shell | `task dev-shell` (or `DOCKER_CONTEXT=containers01 task dev-shell`): builds the image on that engine, copies the working tree in and opens a shell; changes made inside are not copied back |

Before opening a pull request: `task check` and
`GOOS=linux GOARCH=arm64 go build ./...`; add `task lint-actions` when a
workflow changed.

## Test levels

| Level | Where | Needs |
|---|---|---|
| Unit | everywhere (`go test ./...`) | nothing: no root, no systemd, no network |
| Linux unit | `task test-linux`, CI on amd64 and arm64 runners | Docker locally |
| Integration | `tests/integration/<area>/`, build tags `integration[,privileged]` | real software; from Phase 3b |
| VMs | `task test-vm HOST=…` on the Debian/Ubuntu/Rocky VMs (D-056) | from Phase 3b |

Privileged tests never run against a developer's or runner's host network:
only in disposable containers or VMs with their own network namespace
(D-043).

## Continuous integration

Workflows in `.github/workflows/`:

| Workflow | Jobs | When |
|---|---|---|
| `ci.yml` | `lint` (gofmt, vet, tidy, golangci-lint), `test (amd64)`, `test (arm64)` (native arm64 runner, unit + race), `build (linux/amd64)`, `build (linux/arm64)`, `govulncheck`, `workflow lint` (actionlint, zizmor), and the aggregate `ci-ok` | pull requests, pushes to `main` |
| `codeql.yml` | CodeQL for Go and for the workflows (`security-and-quality` queries) | pull requests, `main`, weekly |
| `scorecard.yml` | OpenSSF Scorecard, results in code scanning and on the public API | `main`, weekly, ruleset changes |

`ci-ok` is the **only required status check** of the `main` ruleset: it
fails unless every CI job succeeded, so adding or renaming jobs never
requires changing the ruleset.

### Supply-chain rules for workflows

- Every action is pinned to a full commit SHA, with the version in a
  comment; Dependabot updates them weekly in one grouped pull request.
- Default token permissions are `contents: read`; jobs that need more
  declare it, with a comment saying why.
- `actions/checkout` always uses `persist-credentials: false`.
- Tools run by CI have pinned versions (`GOLANGCI_LINT_VERSION`,
  `GOVULNCHECK_VERSION`, `ACTIONLINT_VERSION`, `ZIZMOR_VERSION` in
  `ci.yml`, mirrored in `Taskfile.yml`).
- Third-party actions are kept to a minimum; tools that can run with
  `go run` or `pipx run` do so.
- Dependabot waits 7 days before proposing a new version (cooldown);
  security updates are not delayed.

## Repository settings

Configuration on GitHub (organisation `sentinel-watchdog`, verified
2026-10-06):

- Organisation: two-factor authentication required; Actions limited to
  GitHub-owned actions plus `golangci/golangci-lint-action` and
  `ossf/scorecard-action`, full-SHA pinning required, read-only default
  token, Actions cannot approve pull requests; workflows from fork pull
  requests need approval for **all** external contributors.
- New public repositories get the organisation security configuration
  `sentinel-baseline` (dependency graph, Dependabot alerts and security
  updates, secret scanning with push protection, private vulnerability
  reporting; no CodeQL default setup).
- Repository: public; merge by squash or rebase only; head branches
  deleted after merge; wiki disabled.
- Ruleset `main` (no bypass): pull request required (0 approvals — one
  human maintainer), stale approvals dismissed, conversations resolved,
  linear history, no force push, no deletion, required check `ci-ok`
  from GitHub Actions with branches up to date.
- Ruleset `release-tags` (no bypass): tags `v*` can be created but never
  moved or deleted.
- Security: private vulnerability reporting, Dependabot alerts and
  security updates, secret scanning with push protection; code scanning
  through `codeql.yml` (the "default setup" stays off).

OpenSSF Scorecard after the first run: 6.8/10. Expected gaps: repository
age (Maintained), single maintainer (Code-Review, Branch-Protection
approvals), no fuzzing yet (Phase 3e), no releases yet (Packaging,
Signed-Releases), no OpenSSF Best Practices badge yet (planned for
Phase 3e).

## Branches and commits

- Work on a branch, open a pull request, merge when `ci-ok` is green.
- Conventional commits (`feat`, `fix`, `refactor`, `docs`, `test`,
  `chore`, `perf`, `ci`); the pull request title uses the same format.
- Labels: `task labels` creates the set (`bug`, `enhancement`,
  `documentation`, `security`, `dependencies`, `ci`, `module:core`,
  `module:supervisor`, `module:firewall`, `needs-decision`,
  `good first issue`).

## Learning Go

The design principles the code follows are in
[AGENTS.md](../AGENTS.md#design-principles). Useful references, in
reading order:

- [A Tour of Go](https://go.dev/tour/) and
  [Learn Go with Tests](https://quii.gitbook.io/learn-go-with-tests) —
  the language, test first.
- [Effective Go](https://go.dev/doc/effective_go) and
  [Go Code Review Comments](https://go.dev/wiki/CodeReviewComments) —
  idiomatic Go.
- [Google Go Style Guide](https://google.github.io/styleguide/go/) —
  naming, errors, documentation in detail.
- [Go Proverbs](https://go-proverbs.github.io/) — the philosophy in one
  page.
- [100 Go Mistakes](https://100go.co/) — common traps, with fixes.

## Go patterns used here

Each idiom is explained here the first time the project uses it (D-060).

### Phase 1 — the Go toolchain

- **`go.mod` and the `go` directive.** `go 1.27` is the minimum language
  version. CI installs the matching toolchain with `go-version-file:
  go.mod` and sets `GOTOOLCHAIN=local`, so a build never silently
  downloads a different Go.
- **`go mod tidy -diff`.** Fails if `go.mod`/`go.sum` do not match the
  imports, without modifying files — a cheap guard against forgotten or
  stale dependencies.
- **Static binaries: `CGO_ENABLED=0`.** Without cgo the binary has no
  dependency on the system C library, so one build runs on glibc (RHEL,
  Debian, Ubuntu) and musl (Alpine) (D-023). The race detector is the
  exception: it needs cgo, which is why race tests set `CGO_ENABLED=1`.
- **Cross-compilation: `GOOS` and `GOARCH`.** `GOOS=linux GOARCH=arm64 go
  build ./...` builds for another platform from any machine; CI builds
  both Linux architectures and runs the tests natively on amd64 and arm64.
- **Reproducible builds: `-trimpath`** removes local file paths from the
  binary; the release pipeline adds a fixed build date.
- **Version injection with `-ldflags -X`.** `Taskfile.yml` sets
  `internal/version.Version`, `Commit` and `Date` at link time, so the
  source has no hard-coded version.
- **Build tags.** A file starting with `//go:build integration` is only
  compiled with `go test -tags integration`; the same mechanism will
  exclude optional modules (`nokubernetes`, ADR-0015).
- **`go run pkg@version`.** Runs a tool at an exact version without
  adding it to `go.mod` (used for govulncheck and actionlint), keeping
  development tools out of the runtime dependency graph.

### Phase 2a — interfaces, fakes, reflection and errors

- **Small interfaces with a real and a fake implementation.**
  [`clock.Clock`](../internal/core/clock/clock.go) has two methods; code
  that waits receives it instead of calling `time.Now()`. Tests use
  [`clock.Fake`](../internal/core/clock/fake.go) and move time with
  `Advance`, so a "retry after 5 minutes" test runs in microseconds.
  Go interfaces are satisfied implicitly: `realClock` never says
  "implements Clock".
- **Mutex and condition variable.** `Fake` protects its state with a
  `sync.Mutex`; `BlockUntilTimers` waits on a `sync.Cond` until another
  goroutine has created a timer, instead of sleeping and hoping. Timer
  channels have capacity 1 and fire once, so a send can never block.
- **Errors as values, collected.** Configuration problems are not
  returned one at a time: `*config.ValidationError` carries every
  `Problem{File, Path, Message}`
  ([problem.go](../internal/core/config/problem.go)). Callers recover it
  with `errors.As`; `errors.Join` merges several errors into one;
  `fmt.Errorf("...: %w", err)` adds context while keeping the original.
- **Reflection for strict decoding.**
  [`checkKnownFields`](../internal/core/config/strict.go) walks the YAML
  tree next to the Go type (`reflect.Type`, struct tags, `t.Fields()`),
  so an unknown key is reported with its line. The walk has a node budget:
  untrusted structure always needs a bound.
- **Small generics.** `oneOf[T ~string]` validates any string-based enum
  type and `sortedKeys[V any]` sorts the keys of any map
  ([validate.go](../internal/core/config/validate.go),
  [strict.go](../internal/core/config/strict.go)). `~string` means "any
  type whose underlying type is string", such as `LogFormat`.
- **Value types with methods.** `config.Section` is a small struct passed
  by value whose zero value is useful (an empty section that decodes to
  nothing). Its `Decode(v any)` checks with reflection that `v` is a
  non-nil pointer.
- **`os.Root` for paths you do not fully trust.** The loader opens the
  configuration directory once with `os.OpenRoot` and resolves every file
  relative to it ([loader.go](../internal/core/config/loader.go)): a
  symbolic link cannot lead outside, and the directory that was checked
  is the one that is read. Files are opened with `O_NONBLOCK` so that a
  named pipe cannot block the daemon.
- **Typed nil.** An interface holding a nil pointer is not `== nil`.
  `isNil` in [registry.go](../internal/core/module/registry.go) uses
  reflection to catch a module that returns `(*T)(nil)` as a `Configured`.
- **Type assertion on platform data.** `fs.FileInfo.Sys()` returns `any`;
  [security.go](../internal/core/config/security.go) asserts
  `*syscall.Stat_t` to read the file owner. If the assertion fails the
  check fails too: in security code a check that cannot run must never
  look like a pass ("fail closed").
- **Explicit registry instead of `init()`.**
  [`module.Registry`](../internal/core/module/registry.go) receives
  factories (`func() Module`) from the daemon. Nothing registers itself
  at import time, so what a binary contains is visible in one place.
- **Recovering from a panic.** `safeConfigure` and `moduleName` use
  `defer` + `recover()` with named results to turn a panicking module
  into an error, both when it is registered and when it is configured,
  so one broken module cannot stop the daemon. The error never includes
  the panic value, which could carry a secret.
- **Functions as values.** `readFile` takes a `check func(os.FileInfo)
  error`: the caller decides what to check on the opened file, without an
  interface for a single method. `expandNode` wraps the lookup function in
  a closure that also records each value it returns — a decorator in
  three lines.
- **Struct embedding.** `loader` embeds `problems`, so `l.errorf(...)`
  and `l.errs` work as if they were declared on `loader`. It is
  composition, not inheritance: `problems` knows nothing about `loader`.
- **Sentinel errors.** `errExpansionBudget` is a package-level error value
  that callers recognise with `errors.Is(err, errExpansionBudget)`,
  instead of comparing message strings.
- **Black-box tests.** `registry_test.go` declares `package module_test`:
  it can use only the exported API, like a real caller, and exercises the
  registry together with `config.Load`.
- **Test helpers.** `t.TempDir()` (removed automatically), `t.Cleanup`,
  `t.Helper()` (failures point at the caller's line), `t.Context()`
  (cancelled when the test ends) and table-driven `t.Run` sub-tests.
- **Tests that read the build.** [`internal/archtest`](../internal/archtest/arch_test.go)
  runs `go list` (for the host and for Linux with the `integration` tag)
  to get every package's imports, and checks them against **allowlists**:
  anything not explicitly allowed fails. Architecture rules become a
  failing test, not a convention. A package's place is checked on its
  own, so even a package that imports nothing from the repository is
  classified.
- **Fuzzing.** `func FuzzXxx(f *testing.F)` targets (in `fuzz_test.go`)
  receive random inputs derived from seeds; `task fuzz` runs each for a
  while. A good target checks a property, not only "no panic":
  `FuzzParseStep` compares cron steps with an independent oracle and
  `FuzzExpandString` checks the growth budget — both found real bugs.
  Failing inputs are saved under `testdata/fuzz/` and rerun by plain
  `go test` as regression cases.
- **Reproducing a bug without editing files.** `go test -overlay
  overlay.json` replaces source files only for one build: the Phase 2a
  audit used it to run new tests against the code before a fix, and to
  add a throw-away package to check `archtest`.

### Phase 2b — channels, goroutines, generics and HTTP

- **A bounded queue is a buffered channel.** `make(chan model.Event, n)`
  holds up to `n` events. `select` with a `default` case tries a send
  without blocking; when the queue is full, the bus and the dispatcher
  take the oldest event out and count it, so the emitter never waits
  ([bus.go](../internal/core/events/bus.go)).
- **Every goroutine has an owner.** `Dispatcher.Run` starts one worker per
  channel with `sync.WaitGroup.Go` and does not return before they have
  stopped (`defer workers.Wait()`); closing a channel (`close(queue)`) ends
  a worker's `for range` loop, cancelling the context abandons its work.
- **Context for cancellation and deadlines.** `context.WithTimeout` bounds
  each webhook attempt; `select { case <-ctx.Done(): … case <-timer.C(): … }`
  makes a retry delay stop at shutdown.
- **Atomic counters.** `sync/atomic.Uint64` counts drops and deliveries
  from one goroutine and reads them from another without a lock.
- **Generic functions.** `state.Load[T](store)` decodes into a new `T`
  and returns it, so a failed load can never leave a half-filled value;
  the call site names the type: `state.Load[supervisorState](s)`.
- **Optional behaviour by interface assertion.** After decoding, `Load`
  checks `value.(interface{ Validate() error })`: a state type that has a
  `Validate` method is validated, others are not — no registration needed.
- **Testing HTTP without a network.** `net/http/httptest` starts a real
  server on the loopback interface (`NewServer`, `NewTLSServer`) so the
  webhook code is tested end to end: status codes, redirects, timeouts,
  certificates.
- **Fakes at the seam.** The dispatcher depends on a one-method `Sender`
  interface it declares itself; tests pass a fake sender and a
  `clock.Fake`, and drive retries by advancing time instead of sleeping.
- **Single-use guard.** `Dispatcher.Run` starts with
  `if d.ran.Swap(true) { return err }` on an `atomic.Bool`: the first
  caller gets `false` and runs, any later or concurrent caller gets an
  error instead of sharing (or closing twice) the workers' queues.
- **Unicode categories.** `unicode.In(r, unicode.Cc, unicode.Cf,
  unicode.Zl, unicode.Zp)` tests a rune against several range tables at
  once; `unicode.IsControl` alone covers only Cc.

### Phase 2c — programs, signals and lifecycle

- **A testable `main`.** `main` is one line, `os.Exit(run(os.Args[1:],
  os.Stdout, os.Stderr))`: `run` returns an exit code and writes to the
  writers it receives, so tests call it like any function. `os.Exit`
  skips deferred calls, which is why it stays out of `run`.
- **Subcommands with `flag.FlagSet`.** Each command gets its own
  `flag.NewFlagSet(name, flag.ContinueOnError)`, so a bad flag returns an
  error (exit 2) instead of ending the process, and tests can parse.
- **Signals as a context.** `signal.NotifyContext(ctx, SIGTERM, SIGINT)`
  returns a context cancelled by the first signal; calling its `stop`
  afterwards restores the default behaviour, so a second signal ends the
  process at once.
- **Re-executing the test binary.** `TestMain` checks an environment
  variable and, when set, runs `run` instead of the tests: a test starts
  `os.Args[0]` as a child with that variable to send it real signals or
  to give it an inherited umask (`sh -c 'umask 000; exec "$0"'`).
- **`testing/synctest`.** `synctest.Test(t, f)` runs `f` in a bubble with
  a fake clock that advances only when every goroutine in the bubble is
  blocked, and fails if a goroutine is still running at the end: a
  goroutine-leak check and instant timeouts in one. `synctest.Wait()`
  waits until the bubble is idle.
- **Abandonable calls.** `daemon.call` runs a module's `Start` or `Stop`
  in a goroutine that sends its result to a channel with a buffer of one
  and waits on a `select` with a timer: if the timer wins, the call is
  abandoned and the late result never blocks the goroutine that sends it.
- **Recover in the goroutine that panics.** `recover` only works in a
  deferred function of the panicking goroutine, so each call recovers in
  its own goroutine; `runtime.Callers` and `runtime.CallersFrames` read
  where the panic happened without formatting the panic value.
- **Process umask.** `syscall.Umask(0o027)` at the start of `run` sets the
  bits every file creation removes; explicit modes can be narrowed by it,
  never widened.
- **`encoding/json/v2`.** The control protocol decodes with the v2 package:
  duplicate member names, invalid UTF-8 and data after the value are
  errors by default, and `json.RejectUnknownMembers(true)` refuses unknown
  fields; `jsontext.Value` keeps a sub-document (the arguments) raw.
- **Build-tagged files.** `peercred_linux.go` (the `_linux` suffix is an
  implicit build constraint) holds the Linux system calls;
  `peercred_other.go` starts with `//go:build !linux` and returns
  `ErrUnsupported`. The package API is the same everywhere.
- **Raw system calls.** `syscall` has no wrapper for `SO_PEERGROUPS`, so
  `syscall.Syscall6(SYS_GETSOCKOPT, …)` passes pointers to Go buffers
  (`unsafe.Pointer`); `SyscallConn().Control` runs it on the socket's file
  descriptor without taking it from the runtime poller.
- **A semaphore is a buffered channel.** `slots := make(chan struct{}, n)`:
  a non-blocking send (`select` with `default`) takes a slot or rejects
  the connection at once; the connection's goroutine gives it back.
- **`net.Pipe` inside `synctest`.** An in-memory connection honours
  deadlines on the bubble's fake clock, so a test proves that a silent
  client is cut off after exactly 5 seconds without waiting for them.
- **`text/tabwriter`.** Aligns the columns of `sentinelctl status`.
