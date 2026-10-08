# Development

How to build, test and change Sentinel Watchdog, how CI and the repository
are secured, and — phase by phase — the Go patterns the code uses (D-060).

## Repository scope and branches

This is the agent repository, including its local CLI. Product coordination,
dashboard, website and design delivery follow [project boundaries](project-layout.md).
Keep agent docs, decisions and release plans here; global research and roadmap
live temporarily in the workspace parent. No agent build or check reads them.

Use a dedicated branch and PR for each change. Verify the full diff against
the target branch and include the checks in the PR. Other repositories need
their own branches and PRs; parent documents remain outside Git until a
coordination repository is established. Runtime phase order is unchanged.

The step-by-step change workflow — local checks, independent review,
turning findings into tests, working with AI agents and when a human
review is mandatory — is in [development-workflow.md](development-workflow.md).

## Requirements

| Tool | Version | Used for |
|---|---|---|
| Go | 1.27 (from `go.mod`) | everything |
| [Task](https://taskfile.dev) | v3 | task runner (`Taskfile.yml`) |
| [golangci-lint](https://golangci-lint.run) | v2.14.0 (same as CI) | lint and import formatting |
| Docker (local or remote engine) | any recent | `task test-linux` |
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

`task test-linux` streams the working tree (tracked and untracked,
non-ignored files) into the container as a tar archive on stdin instead of
bind-mounting it, so it works with a local engine and with a remote Docker
context alike; it prints which engine it uses. Choose one explicitly with
`DOCKER_CONTEXT=<context> task test-linux` (the maintainer uses the
`containers01` host). The code under test is sent to that engine.

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
  `*syscall.Stat_t` to read the file owner, and degrades gracefully where
  the assertion fails.
- **Explicit registry instead of `init()`.**
  [`module.Registry`](../internal/core/module/registry.go) receives
  factories (`func() Module`) from the daemon. Nothing registers itself
  at import time, so what a binary contains is visible in one place.
- **Recovering from a panic.** `safeConfigure` uses `defer` + `recover()`
  with named results to turn a panicking module into an error, so one
  broken module cannot stop the others from being validated.
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
  failing test, not a convention.
