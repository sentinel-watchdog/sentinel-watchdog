# Development

How to build, test and change Sentinel Watchdog, how CI and the repository
are secured, and the Go patterns the code uses, phase by phase (D-060).

## Repository scope and branches

This repository holds the agent and its local CLI. The dashboard, website
and design have repositories of their own ([project boundaries](project-layout.md)).
Global research and the product roadmap live in the maintainer's private
coordination repository (D-073); no agent build or check reads them. The
plan and its status are in [PLAN.md §7](../PLAN.md#7-phases).

Branch, commit, label and pull request rules are in
[CONTRIBUTING.md](../CONTRIBUTING.md#commits-and-pull-requests). The
step-by-step change workflow (local checks, independent review, findings
turned into tests, AI agents, when a human must review) is in
[development-workflow.md](development-workflow.md).

## Requirements

| Tool | Version | Used for |
|---|---|---|
| Go | 1.27 (from `go.mod`) | everything |
| [Task](https://taskfile.dev) | v3 | task runner (`Taskfile.yml`) |
| [golangci-lint](https://golangci-lint.run) | v2.15.0, run by `task lint` and `task fmt` with `go run` (CI uses golangci-lint-action at the same version); an installed copy is only for editors | lint and import formatting |
| Docker (local or remote engine) | any recent | `task test-linux`, `task dev-shell`, the dev container, `task test-package-scripts`, `task package-verify`, `task package-lint` |
| [uv](https://docs.astral.sh/uv/) | any recent | `task lint-actions` (runs zizmor with `uvx`) |
| [gh](https://cli.github.com) | any recent | `task labels`, pull requests |
| [Vagrant](https://www.vagrantup.com) with VirtualBox | any recent | `task vm`, `task vm-ssh-config` (the VM matrix) |
| `jq`, `file`, `shasum` | any recent | `task snapshot` (its `snapshot-verify` step) |
| [ShellCheck](https://www.shellcheck.net) | any recent (CI uses the runner's; not pinned yet, B-052) | `task lint-shell`, so `task check`; shfmt is pinned and run with `go run` |

## Tasks

| Task | What it does |
|---|---|
| `task check` | Offline CI checks: gofmt, `go mod tidy -diff`, vet, golangci-lint, tests, race tests, build |
| `task vuln` | govulncheck (pinned version, needs network) |
| `task lint-actions` | actionlint and zizmor (auditor persona) on `.github/workflows` |
| `task test-linux` | Unit tests in Linux containers as a non-root user: `golang:1.27` (glibc, race detector) and `golang:1.27-alpine` (musl). On Ubuntu kernel `7.0.0-1014-aws` run it only on commits from D-079 on: older ones hit an AppArmor bug that hangs the host ([PLAN.md §4, R-024](../PLAN.md#4-risks)) |
| `task snapshot` | Release-shaped build in `dist/` (nothing published): tarballs and DEB, RPM and APK packages for linux amd64/arm64, `checksums.txt`, a CycloneDX SBOM per binary, then `snapshot-verify` (D-079, D-102) |
| `task test-release` | Tests the release gates: the tag check in throwaway repositories, and the steps of `release.yml` that check and publish the files, extracted from it and run with stub commands; part of `task check` (D-103) |
| `task release-build` | `task snapshot`, only when `RELEASE_TAG` is vX.Y.Z, the checkout is exactly that tag with no changed, untracked or ignored source file and its commit is on `origin/main`; run by `release.yml` (D-103) |
| `task snapshot-tools` | Builds GoReleaser (`go run`, cached) and installs cyclonedx-gomod in `bin/tools`; afterwards `task snapshot` works offline |
| `task snapshot-check` | `goreleaser check` on `.goreleaser.yaml` |
| `task reproduce` | Builds the snapshot twice from clean copies of the working tree (different paths, time zones and times) and compares every artifact byte for byte |
| `task test-package-scripts` | The package scripts (`packaging/scripts`) against stubbed system commands, as root in throwaway Debian and Alpine containers (`tests/packaging/scripts_test.sh`), and the package version table (`tests/packaging/version_test.sh`, checked with `apk version` and `dpkg --compare-versions`); `--network none` |
| `task lint-shell` | ShellCheck and shfmt (pinned, `go run`) on the package scripts and their tests; part of `task check` |
| `task package-verify` | After `task snapshot`: each package's files, owners, modes, configuration file, the script bound to each event, architecture, version, build host and time, against the list written in `tests/packaging/verify_packages.sh`; in the image of `tests/packaging/Dockerfile` |
| `task package-lint` | lintian and rpmlint on the packages; errors fail. Overrides and filters, each with its reason, are in `packaging/debian/lintian-overrides` and `tests/packaging/rpmlint.toml`; the remaining warnings are listed below |
| `task package-pair`, `SENTINEL_DISPOSABLE_HOST=1 task test-package HOST=… [SSH_CONFIG=…]` | The package lifecycle on a disposable VM (`tests/packaging/lifecycle.sh`): two versions from `package-pair` installed, upgraded running and stopped, removed and purged with the VM's package manager and init system. It deletes `/etc/sentinel` and `/var/lib/sentinel` on the VM and refuses a host that already has them. Never on a shared or production host |
| `task cover` | Coverage report in `coverage.html` |
| `task fuzz` | Every `Fuzz*` target for `FUZZTIME` each (default `10s`) |
| `task fmt` | Format code (gofmt and goimports) |
| `task labels` | Create or update the GitHub labels |
| `task dev-shell` | Interactive shell in the dev container (Linux, Go, task, golangci-lint) with a copy of the working tree |
| `task vm -- <vagrant args>`, `task vm-ssh-config -- <vms>` | The VM matrix of `tests/vm/Vagrantfile` (D-085), with Vagrant's state and the `ssh_config` in `SENTINEL_VM_DIR` (default `~/.cache/sentinel-watchdog/vm`), never in the repository |
| `task test-vm HOST=… [SSH_CONFIG=…]` | Root integration tests (`tests/integration/…`, tag `integration`) on a disposable Linux VM over SSH: cross-compiled `go test -c` binaries copied to a temporary directory and run with `sudo`; the VM needs no Go toolchain (D-056). `SSH_CONFIG` names an ssh_config for VMs behind a jump host (for example Vagrant VMs reached with `ProxyCommand ssh <host> -W %h:%p`). Never run it on a shared or production host |

`task --list` shows the others: the steps of `task check` (`fmt-check`,
`tidy-check`, `vet`, `lint`, `test`, `test-race`, `build`), `tidy`,
`snapshot-verify` and `clean`.

Warnings `task package-lint` leaves, by decision (2026-11-19, per
architecture):

| Tool | Warning | Why it stays |
|---|---|---|
| lintian | `no-manual-page [usr/bin/sentineld]`, `[usr/bin/sentinelctl]` | B-051 |
| rpmlint | `no-manual-page-for-binary sentineld`, `sentinelctl` | B-051 |
| rpmlint | `invalid-license Apache-2.0` | it wants the old Fedora short name; the SPDX name is correct |
| rpmlint | `position-independent-executable-suggested` (both binaries) | static binaries (D-023) |
| rpmlint | `no-documentation` | the documentation is not marked `%doc` (see `tests/packaging/rpmlint.toml`) |
| rpmlint | `percent-in-%post`, `percent-in-%preun`, `percent-in-%postun`, `dangerous-command-in-%postun rpm` | words in the scripts' comments, which nFPM stores without macro expansion |

`task test-linux` streams the working tree (tracked and untracked files
that are not ignored) into the container as a tar archive on stdin, with
no bind mount, so it works with a local engine and with a remote Docker
context alike, and prints which engine it uses. Choose one explicitly with
`DOCKER_CONTEXT=<context> task test-linux`. The code under test is sent
to that engine.

## Editors and dev container

The repository carries shared editor settings; personal ones stay in your
user settings.

- VS Code reads `.vscode/settings.json` (golangci-lint as linter, gopls
  with the `integration` tag so Linux-only and integration test files are
  analysed, the module as local import prefix, format and organise imports
  on save) and `.vscode/extensions.json` (Go, EditorConfig, Dev
  Containers). Git ignores the other files in `.vscode/`. No task runs
  when the folder opens.
- Zed reads `.zed/settings.json`, with the same gopls options and format
  on save. golangci-lint in Zed needs a separate extension and language
  server (optional); `task check` runs it anyway.
- The dev container (`.devcontainer/`) is `golang:1.27` with golangci-lint
  and task at the versions of CI, running as a normal user (`dev`,
  uid 1000) so permission tests behave as on a server. Use it to run what
  only works on Linux, for example `sentineld` with `sentinelctl status`
  (peer credentials, D-076). VS Code (Dev Containers extension) and Zed
  (since v0.218, with limited port forwarding) open it from
  `devcontainer.json`.
- AI agents run on the host. Claude Code and Codex work in the repository
  there and reach Linux through `task test-linux` and `task dev-shell`.
  An editor's agents started inside the container cannot use the host's
  login, and making them work there is not worth the upkeep.

None of these files names the Docker engine: it is the current context
(`docker context use <name>`, or `DOCKER_CONTEXT=<name>` in the
environment that starts the editor or the task). Where the engine runs
decides how the sources get in:

| Engine | How to use the dev container |
|---|---|
| Local (colima, Docker Desktop) | open the folder in the container: the working tree is mounted |
| Remote (a Docker host reached over SSH) | Docker cannot mount a local folder on a remote host. Use VS Code Remote-SSH to the host with a clone there, then "Reopen in Container"; or "Dev Containers: Clone Repository in Container Volume" (the code lives in a volume on that host: push before cleaning up volumes) |
| Any, for a quick Linux shell | `task dev-shell` (or `DOCKER_CONTEXT=<context> task dev-shell`) builds the image on that engine, copies the working tree in and opens a shell; changes made inside are not copied back |

The checks to run before a pull request are in step 4 of
[the loop](development-workflow.md#1-the-loop).

## Test levels

| Level | Where | Needs |
|---|---|---|
| Unit | everywhere (`go test ./...`) | nothing: no root, no systemd, no network |
| Linux unit | `task test-linux`, CI on amd64 and arm64 runners | Docker locally |
| Integration | `tests/integration/<area>/`, build tag `integration` (and `linux`) | real software, root; a suite fails, never skips, without root |
| VMs | `task test-vm HOST=… [SSH_CONFIG=…]` on the ten VMs of D-085 and D-093 (`tests/vm/Vagrantfile`): Debian 12 and 13, Ubuntu 22.04, 24.04 and 26.04, Rocky 8, 9 and 10, openSUSE Leap 15, and Alpine 3.22 (no systemd) | Vagrant with VirtualBox, or any SSH host with `sudo`; every privileged change, on all ten |

`task vm` runs `vagrant` on `tests/vm/Vagrantfile` with its state outside
the repository, in `SENTINEL_VM_DIR` (default
`~/.cache/sentinel-watchdog/vm`), where `task vm-ssh-config` also writes
the `ssh_config`:

```sh
task vm -- up debian13 rocky9                  # names from tests/vm/Vagrantfile
task vm-ssh-config -- debian13 rocky9
task test-vm HOST=debian13 SSH_CONFIG=$HOME/.cache/sentinel-watchdog/vm/ssh_config
task vm -- destroy -f                          # when done
```

VMs elsewhere (another host, a cloud) work the same way with an
`ssh_config` of your own, kept outside the repository, whose host names
reach them, for example through a jump host
(`ProxyCommand ssh <jump> -W %h:%p`).

The supervisor's end-to-end tests (`tests/integration/supervisor/units_test.go`,
D-101) install throwaway units under `/run/systemd/system`, which the
product never does, and start the daemon with only those units, without
`newSetup`'s worker, whose TERM-ignoring leaf would outlive the test
([B-046](backlog.md#deferred-findings-and-follow-ups)). Their services use
`timeout: 2s` because systemd sometimes never answers the first read after
a reload ([B-040](backlog.md#deferred-findings-and-follow-ups)); the
flapping test waits 30 s.

Unit tests do not depend on the developer's umask: packages whose tests
write configuration or state under `t.TempDir()` set umask 022 in a
`TestMain` (`umask_test.go`), because those directories get mode
`0777 &^ umask` and the code under test refuses group-writable
directories.

Privileged tests never run against a developer's or runner's host network:
only in disposable containers or VMs with their own network namespace
(D-043).

## Continuous integration

Workflows in `.github/workflows/`:

| Workflow | Jobs | When |
|---|---|---|
| `ci.yml` | `lint` (gofmt, vet, tidy, golangci-lint), `test (amd64)` and `test (arm64)` (native runners, unit and race tests), `build (linux/amd64)`, `build (linux/arm64)`, `release snapshot` (`task snapshot`, `task test-release`, `task test-package-scripts`, `task package-verify`; uploads the archives, packages, SBOMs and checksums for 7 days), `govulncheck`, `workflow lint` (actionlint, zizmor), and the aggregate `ci-ok` | pull requests, pushes to `main`, manual |
| `release.yml` | `build` (tag check, `task release-build`, `task package-verify`), `sign` (cosign, build provenance), `publish draft` ([Releasing](#releasing), D-103) | tags `v*` |
| `codeql.yml` | CodeQL for Go and for the workflows (`security-and-quality` queries) | pull requests to `main`, pushes to `main`, weekly, manual |
| `scorecard.yml` | OpenSSF Scorecard, results in code scanning and on the public API | pushes to `main`, weekly, ruleset changes, manual |

`ci-ok` is the only required status check of the `main` ruleset. It fails
unless every CI job succeeded, so adding or renaming a job never requires
changing the ruleset.

The rules for workflows (pinned actions, token permissions, tool versions,
Dependabot) are in [guidelines/github-workflows.md](guidelines/github-workflows.md).

## Releasing

A tag `vX.Y.Z` starts `release.yml`, which ends with a **draft** release
(D-103). Only the maintainer does this. Tags `v*` can never be moved or
deleted, so a mistake costs a version number.

1. Check that the tag-creation ruleset and the environment `release` are in
   place ([Repository settings](#repository-settings)).
2. Merge the release PR (CHANGELOG, PLAN) and wait for `ci-ok` on `main`.
3. `git switch main && git pull --ff-only`, then
   `git tag -a vX.Y.Z -m vX.Y.Z && git push origin vX.Y.Z`.
4. When every job has succeeded, check the whole draft, in an empty
   directory:

   ```sh
   gh release download vX.Y.Z --repo sentinel-watchdog/sentinel-watchdog
   # cosign verify-blob as in "Verify a release" (install.md), then:
   sha256sum --strict -c checksums.txt     # every listed file is there
   ls | wc -l                              # the listed files plus two
   wc -l < checksums.txt
   ```

5. Edit the notes if needed and publish the draft.

If a job fails: `build` refuses a tag that is not vX.Y.Z, does not match
the checkout, is not on `main` or shares its commit with another tag; fix
the cause and use the next version, on a new commit if the old one has a tag.
After a failed `sign` or `publish draft`, re-run the failed jobs; `publish
draft` refuses when the tag already has a release, so delete an incomplete
draft first. The jobs' files expire after 7 days: later, re-run all jobs,
which gives the same files with a new signature. Every
signature is recorded for ever in Sigstore's public log, whether or not
the draft is published.

The `sign` and `publish draft` jobs use the environment `release`, which
must exist **before** the first tag, limited to tags `v*`: GitHub creates a
missing environment without any protection.

## Repository settings

Configuration on GitHub (organisation `sentinel-watchdog`, verified
2026-10-06):

- Organisation: two-factor authentication required; Actions limited to
  GitHub-owned actions plus `golangci/golangci-lint-action` and
  `ossf/scorecard-action`, full-SHA pinning required, read-only default
  token, Actions cannot approve pull requests; workflows from fork pull
  requests need approval for all external contributors.
- New public repositories get the organisation security configuration
  `sentinel-baseline` (dependency graph, Dependabot alerts and security
  updates, secret scanning with push protection, private vulnerability
  reporting; no CodeQL default setup).
- Repository: public; merge by squash or rebase only; head branches
  deleted after merge; wiki disabled.
- Ruleset `main` (no bypass): pull request required (0 approvals: one
  human maintainer), stale approvals dismissed, conversations resolved,
  linear history, no force push, no deletion, required check `ci-ok`
  from GitHub Actions with branches up to date.
- Ruleset `release-tags` (no bypass): tags `v*` can be created but never
  moved or deleted.
- To set up before the first release (D-103), not in place yet:
  - a second tag ruleset that restricts the **creation** of `v*` tags to
    the maintainer. The tag decides which copy of `release.yml` runs, so
    whoever can create one on any commit can sign with the release
    identity, whatever the checks in that file say;
  - the environment `release`, deployments limited to tags `v*`.
- Security: private vulnerability reporting, Dependabot alerts and
  security updates, secret scanning with push protection; code scanning
  through `codeql.yml` (the "default setup" stays off).

OpenSSF Scorecard scores the repository's supply-chain practices. The
current result is on
[scorecard.dev](https://scorecard.dev/viewer/?uri=github.com/sentinel-watchdog/sentinel-watchdog)
and in the README badge. The first run, in Phase 1, scored 6.8/10 with
these expected gaps: repository age (Maintained), a single maintainer
(Code-Review, Branch-Protection approvals), no fuzzing, no releases
(Packaging, Signed-Releases) and no Best Practices badge. Since then the
code has `Fuzz*` targets (`task fuzz`) and the preview tags `v0.1.0`,
`v0.2.0` and `v0.3.0`, which publish no artifacts; published, signed releases come
with Phase 3d and the badge with Phase 3e.

## Learning Go

The design principles the code follows are in
[AGENTS.md](../AGENTS.md#design-principles). Useful references, in
reading order:

- [A Tour of Go](https://go.dev/tour/) and
  [Learn Go with Tests](https://quii.gitbook.io/learn-go-with-tests):
  the language, test first.
- [Effective Go](https://go.dev/doc/effective_go) and
  [Go Code Review Comments](https://go.dev/wiki/CodeReviewComments):
  idiomatic Go.
- [Google Go Style Guide](https://google.github.io/styleguide/go/):
  naming, errors, documentation in detail.
- [Go Proverbs](https://go-proverbs.github.io/): the philosophy in one
  page.
- [100 Go Mistakes](https://100go.co/): common traps, with fixes.

## Go patterns used here

Each idiom gets an entry the first time the project uses it (D-060): the
pattern, where it is used and why.

### Phase 1: the Go toolchain

- `go 1.27` in [go.mod](../go.mod) is the minimum language version. CI
  installs the matching toolchain with `go-version-file: go.mod` and sets
  `GOTOOLCHAIN=local` ([ci.yml](../.github/workflows/ci.yml)), so a build
  never downloads a different Go.
- `go mod tidy -diff` (`task tidy-check`, CI `lint`) fails when
  `go.mod`/`go.sum` do not match the imports, without changing files: a
  cheap guard against forgotten or stale dependencies.
- `CGO_ENABLED=0` ([Taskfile.yml](../Taskfile.yml)) builds static
  binaries with no dependency on the system C library, so one build runs
  on glibc and musl (D-023). The race detector needs cgo, so race tests
  set `CGO_ENABLED=1`.
- `GOOS=linux GOARCH=arm64 go build ./...` cross-compiles from any
  machine; CI builds both Linux architectures and runs the tests natively
  on amd64 and arm64.
- `-trimpath` removes local file paths from the binary; with a fixed
  build date (the last commit's, or `SOURCE_DATE_EPOCH`) the build is
  reproducible ([Taskfile.yml](../Taskfile.yml), `.goreleaser.yaml`).
- `-ldflags -X` sets `internal/version.Version`, `Commit` and `Date` at
  link time ([Taskfile.yml](../Taskfile.yml) `LDFLAGS`), so the source
  has no hard-coded version.
- A file starting with `//go:build integration` is compiled only with
  `go test -tags integration` (`tests/integration/`); the same mechanism
  will exclude optional modules (`nokubernetes`, ADR-0015).
- `go run pkg@version` runs a tool at an exact version without adding it
  to `go.mod` (govulncheck, actionlint, golangci-lint, GoReleaser in
  [Taskfile.yml](../Taskfile.yml)), keeping development tools out of the
  runtime dependency graph.

### Phase 2a: interfaces, fakes, reflection and errors

- A small interface with a real and a fake implementation:
  [`clock.Clock`](../internal/core/clock/clock.go) has two methods, and
  code that waits receives it instead of calling `time.Now()`. Tests use
  [`clock.Fake`](../internal/core/clock/fake.go) and move time with
  `Advance`, so a "retry after 5 minutes" test runs in microseconds. Go
  interfaces are satisfied implicitly: `realClock` never says it
  implements `Clock`.
- A mutex and a condition variable in
  [fake.go](../internal/core/clock/fake.go): `Fake` guards its state with
  a `sync.Mutex`, and `BlockUntilTimers` waits on a `sync.Cond` until
  another goroutine has created a timer, so tests never sleep and hope.
  Timer channels have capacity 1 and fire once, so a send never blocks.
- Errors collected as values: `*config.ValidationError` carries every
  `Problem{File, Path, Message}`
  ([problem.go](../internal/core/config/problem.go)), so a file reports
  all its problems at once. Callers recover it with `errors.As`,
  `errors.Join` merges several errors, and `fmt.Errorf("...: %w", err)`
  adds context while keeping the original.
- Reflection for strict decoding:
  [`checkKnownFields`](../internal/core/config/strict.go) walks the YAML
  tree next to the Go type (`reflect.Type`, struct tags, `t.Fields()`) to
  report an unknown key with its line. The walk has a node budget, because
  untrusted structure always needs a bound.
- Small generics: `oneOf[T ~string]`
  ([validate.go](../internal/core/config/validate.go)) validates any
  string-based enum type, and `sortedKeys[V any]`
  ([strict.go](../internal/core/config/strict.go)) sorts the keys of a
  `map[string]V`, for any value type. `~string` means any type whose underlying type is string, such as
  `LogFormat`.
- A value type with a useful zero value: `config.Section`
  ([section.go](../internal/core/config/section.go)) is a small struct
  passed by value, and its zero value is an empty section that decodes to
  nothing. `Decode(v any)` checks with reflection that `v` is a non-nil
  pointer.
- `os.Root` for paths you do not fully trust: the loader opens the
  configuration directory once with `os.OpenRoot` and resolves every file
  relative to it ([loader.go](../internal/core/config/loader.go)), so a
  symbolic link cannot lead outside and the directory that was checked is
  the one that is read. Files are opened with `O_NONBLOCK`, so a named
  pipe cannot block the daemon.
- Typed nil: an interface holding a nil pointer is not `== nil`, so
  `isNil` in [registry.go](../internal/core/module/registry.go) uses
  reflection to catch a module that returns `(*T)(nil)`.
- A type assertion on platform data: `fs.FileInfo.Sys()` returns `any`,
  and `checkOwner` ([fstrust.go](../internal/core/fstrust/fstrust.go)) asserts
  `*syscall.Stat_t` to read the file owner. If the assertion fails the
  check fails too: in security code a check that cannot run must never
  look like a pass.
- An explicit registry instead of `init()`:
  [`module.Registry`](../internal/core/module/registry.go) receives
  factories (`func() Module`) from the daemon. Nothing registers itself at
  import time, so what a binary contains is visible in one place.
- Recovering from a panic: `inspect` (at registration) and
  `safeConfigure` ([registry.go](../internal/core/module/registry.go))
  use `defer` and `recover()` with named results to turn a panicking
  module into an error, which reaches the caller like any other error; at
  start-up it stops sentineld from starting, and nothing crashes. The error never
  includes the panic value, which could carry a secret.
- Functions as values: `readFile`
  ([security.go](../internal/core/config/security.go)) takes a
  `check func(os.FileInfo) error`, so the caller decides what to check on
  the opened file without a one-method interface. `expandNode`
  ([envexpand.go](../internal/core/config/envexpand.go)) wraps the lookup
  function in a closure that also records each distinct value it
  returns: a small decorator.
- Struct embedding: `loader` embeds `problems`
  ([loader.go](../internal/core/config/loader.go)), so `l.errorf(...)`
  and `l.errs` work as if declared on `loader`. This is composition:
  `problems` knows nothing about `loader`.
- A sentinel error: `errExpansionBudget`
  ([envexpand.go](../internal/core/config/envexpand.go)) is a
  package-level value that callers match with `errors.Is`, without
  comparing message strings.
- Black-box tests: `registry_test.go` declares `package module_test`, so
  it can use only the exported API, like a real caller, and exercises the
  registry together with `config.Load`.
- Test helpers used throughout: `t.TempDir()` (removed automatically),
  `t.Cleanup`, `t.Helper()` (failures point at the caller's line),
  `t.Context()` (cancelled when the test ends) and table-driven `t.Run`
  sub-tests.
- Tests that read the build:
  [`internal/archtest`](../internal/archtest/arch_test.go) runs `go list`
  (for the host, and for Linux with the `integration` tag) to get every
  package's imports and checks them against allowlists, so anything not
  explicitly allowed fails and the architecture rules are a test. Each
  package's layer is checked on its own, so even a package that imports
  nothing from the repository is classified.
- Fuzzing: `func FuzzXxx(f *testing.F)` targets (in `fuzz_test.go` files)
  receive random inputs derived from seeds, and `task fuzz` runs each for
  a while. A good target checks a property, not only the absence of a
  panic: `FuzzParseStep` ([cronexpr](../internal/core/cronexpr/fuzz_test.go))
  compares cron steps with an independent oracle and `FuzzExpandString`
  ([config](../internal/core/config/fuzz_test.go)) checks the growth
  budget; both found real bugs. Failing inputs are saved under
  `testdata/fuzz/` and rerun by plain `go test` as regression cases.
- `go test -overlay overlay.json` replaces source files for one build
  only: the Phase 2a audit used it to run new tests against the code
  before a fix, and to add a throw-away package to check `archtest`,
  without editing files.

### Phase 2b: channels, goroutines, generics and HTTP

- A bounded queue is a buffered channel: `make(chan model.Event, n)`
  holds up to `n` events, and `select` with a `default` case sends without
  blocking. When the queue is full, the bus and the dispatcher drop the
  oldest event and count it, so the emitter never waits
  ([bus.go](../internal/core/events/bus.go),
  [dispatcher.go](../internal/core/notify/dispatcher.go)).
- Every goroutine has an owner: `Dispatcher.Run`
  ([dispatcher.go](../internal/core/notify/dispatcher.go)) starts one
  worker per channel with `sync.WaitGroup.Go` and returns only after they
  have stopped (`defer workers.Wait()`). Closing a channel ends a worker's
  `for range` loop; cancelling the context abandons its work.
- Context for cancellation and deadlines: `context.WithTimeout` bounds
  each webhook attempt ([webhook.go](../internal/core/notify/webhook.go)),
  and `select { case <-ctx.Done(): … case <-timer.C(): … }` makes a retry
  delay stop at shutdown.
- Atomic counters: `sync/atomic.Uint64` counts drops and deliveries in one
  goroutine and lets another read them without a lock
  ([bus.go](../internal/core/events/bus.go),
  [dispatcher.go](../internal/core/notify/dispatcher.go)).
- A generic function: `state.Load[T](store)`
  ([store.go](../internal/core/state/store.go)) decodes into a new `T`
  and returns it, so a failed load never leaves a half-filled value; the
  call site names the type, as in `state.Load[supervisorState](s)`.
- Optional behaviour by interface assertion: after decoding, `Load` asks
  whether the value implements the `validator` interface
  (`Validate() error`, [store.go](../internal/core/state/store.go)). A
  state type with that method is validated and others are not, with no
  registration.
- HTTP tests without external services: `net/http/httptest` starts a real server
  on the loopback interface (`NewServer`, `NewTLSServer`), so the webhook
  code is tested end to end, with status codes, redirects, timeouts and
  certificates ([webhook_test.go](../internal/core/notify/webhook_test.go)).
- A fake at the seam: the dispatcher depends on a one-method `Sender`
  interface it declares itself
  ([dispatcher.go](../internal/core/notify/dispatcher.go)); tests pass a
  fake sender and a `clock.Fake`, and drive retries by advancing time.
- A single-use guard: `Dispatcher.Run` starts with
  `if d.ran.Swap(true) { return err }` on an `atomic.Bool`. The first
  caller gets `false` and runs; any later or concurrent caller gets an
  error, so two runs never share (or close twice) the workers' queues.
- Unicode categories: `unicode.In(r, unicode.Cc, unicode.Cf,
  unicode.Zl, unicode.Zp)` tests a rune against several range tables at
  once, where `unicode.IsControl` alone covers only Cc
  ([normalize.go](../internal/core/events/normalize.go)).

### Phase 2c: programs, signals and lifecycle

- A testable `main`: `main` is one line,
  `os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))`, and `run` returns an
  exit code and writes to the writers it receives, so tests call it like
  any function ([sentineld](../cmd/sentineld/main.go),
  [sentinelctl](../cmd/sentinelctl/main.go)). `os.Exit` skips deferred
  calls, which is why it stays out of `run`.
- Subcommands with `flag.FlagSet`: each command gets its own
  `flag.NewFlagSet(name, flag.ContinueOnError)`, so a bad flag returns an
  error (exit 2) instead of ending the process, and tests can parse
  ([sentinelctl](../cmd/sentinelctl/main.go)).
- Signals as a context: `signal.NotifyContext(ctx, SIGTERM, SIGINT)` in
  [main.go](../cmd/sentineld/main.go) returns a context cancelled by the
  first signal. Calling its `stop` afterwards restores the default
  behaviour, so a second signal ends the process at once.
- Re-executing the test binary: `TestMain`
  ([main_test.go](../cmd/sentineld/main_test.go)) checks an environment
  variable and, when it is set, runs `run` instead of the tests. A test
  starts `os.Args[0]` as a child with that variable to send it real
  signals or give it an inherited umask (`sh -c 'umask 000; exec "$0"'`).
- `testing/synctest`: `synctest.Test(t, f)` runs `f` in a bubble with a
  fake clock that advances only when every goroutine in the bubble is
  blocked, and fails if a goroutine is still running at the end, which
  gives a goroutine-leak check and instant timeouts in one
  (`internal/daemon`, `internal/core/transport` tests).
  `synctest.Wait()` waits until the bubble is idle.
- Abandonable calls: `Daemon.call`
  ([lifecycle.go](../internal/daemon/lifecycle.go)) runs a module's
  `Start` or `Stop` in a goroutine that sends its result to a channel with
  a buffer of one, and waits in a `select` with a timer. If the timer
  wins, the call is abandoned, and the late result never blocks the
  goroutine that sends it.
- Recover in the goroutine that panics: `recover` works only in a
  deferred function of the panicking goroutine, so each call recovers in
  its own goroutine; `runtime.Callers` and `runtime.CallersFrames` read
  where the panic happened without formatting the panic value
  ([lifecycle.go](../internal/daemon/lifecycle.go)).
- The process umask: `syscall.Umask(umask)` (`0o027`) at the start of `run`
  ([main.go](../cmd/sentineld/main.go)) sets the bits every file creation
  removes; it can narrow explicit modes, never widen them.
- `encoding/json/v2`: the control protocol
  ([codec.go](../pkg/api/codec.go)) decodes with the v2 package, where
  duplicate member names, invalid UTF-8 and data after the value are
  errors by default, and `json.RejectUnknownMembers(true)` refuses unknown
  fields. `jsontext.Value` keeps a sub-document (the arguments) raw.
- Build-tagged files: [peercred_linux.go](../internal/platform/peercred/peercred_linux.go)
  (the `_linux` suffix is an implicit build constraint) holds the Linux
  system calls, and `peercred_other.go` starts with `//go:build !linux`
  and returns `ErrUnsupported`. The package API is the same everywhere.
- Raw system calls: `syscall` has no wrapper for `SO_PEERGROUPS`, so
  `syscall.Syscall6(SYS_GETSOCKOPT, …)` passes pointers to Go buffers
  (`unsafe.Pointer`), and `SyscallConn().Control` runs it on the socket's
  descriptor without taking it from the runtime poller
  ([peercred_linux.go](../internal/platform/peercred/peercred_linux.go)).
- A semaphore is a buffered channel: `slots := make(chan struct{}, n)` in
  [server.go](../internal/core/transport/server.go). A non-blocking send
  (`select` with `default`) takes a slot or rejects the connection at
  once, and the connection's goroutine gives it back.
- `net.Pipe` inside `synctest`: an in-memory connection honours deadlines
  on the bubble's fake clock, so a test proves that a silent client is cut
  off after exactly 5 seconds without waiting for them
  ([transport_test.go](../internal/core/transport/transport_test.go)).
- `text/tabwriter` aligns the columns of `sentinelctl status`
  ([remote.go](../cmd/sentinelctl/remote.go)).
- Hash the bytes you wrote: the audit log hashes the exact bytes of a
  record before its `hash` member, and the reader checks those bytes
  before decoding, because re-encoding a parsed value could differ in a
  later Go release and break every old hash. `json.Deterministic(true)`
  sorts map keys so the writer is reproducible (golden test)
  ([record.go](../internal/core/audit/record.go)).
- A semaphore that honours a context:
  `select { case sem <- struct{}{}: … case <-ctx.Done(): … }` waits for
  exclusive access at most until the caller's deadline, which a
  `sync.Mutex` cannot do ([log.go](../internal/core/audit/log.go)).
- A token bucket: [admission.go](../internal/core/transport/admission.go)
  refills each user's tokens from the elapsed time on an injected clock
  (`min(burst, tokens + elapsed × rate)`), so the test advances a fake
  clock instead of sleeping.
- Fuzzing past a checksum: a fuzzer cannot forge SHA-256, so `FuzzParse`
  ([fuzz_test.go](../internal/core/audit/fuzz_test.go)) also tries every
  input sealed with its correct hash, to reach the decoder behind the
  check.
- Swapping an immutable value with `atomic.Pointer[T]`: the transport
  reads the access policy on every request, and a reload stores a new one
  with `policy.Store(&p)` ([server.go](../internal/core/transport/server.go)).
  Readers never lock, and a request already authorized keeps the policy
  it read.
- Publishing a snapshot: a reload builds a whole `view` (configuration,
  its redacted JSON, routers) and replaces it under a `sync.RWMutex`
  ([daemon.go](../internal/daemon/daemon.go)). Readers copy the struct and
  release the lock before encoding or routing, so they never see half a
  reload.
- Asking the owner over a channel: only the forwarder goroutine knows
  which dispatcher it feeds. A reload sends it a `swap` carrying a reply
  channel and gets the old generation back
  ([daemon.go](../internal/daemon/daemon.go)), so no pointer is shared
  behind a mutex ("share memory by communicating").
- A repeating signal on a channel: `signal.NotifyContext` suits SIGTERM,
  which happens once. SIGHUP repeats, so
  [main.go](../cmd/sentineld/main.go) uses `signal.Notify(hup, SIGHUP)` on
  a channel with a buffer of one: a burst of signals during a reload
  leaves one pending.
- `context.WithoutCancel`: the outcome of a reload is recorded even when
  the caller's deadline has passed. The new context keeps the caller's
  values but not its cancellation, and gets its own timeout
  ([reload.go](../internal/daemon/reload.go)).
- Range over iterators: `slices.Backward(runs)` walks the modules in
  reverse start order to stop them
  ([reload.go](../internal/daemon/reload.go)), and `strings.Lines(msg)`
  yields the lines of a message, each with its newline
  ([remote.go](../cmd/sentinelctl/remote.go)).
- `context.AfterFunc` runs a function in its own goroutine once a context
  ends, with no goroutine waiting for it meanwhile. The socket client
  moves its connection's deadline to the past when the caller gives up,
  which interrupts a pending read or write
  ([client.go](../internal/core/transport/client.go)), and sentineld
  restores the default signal handling after the first SIGTERM
  ([main.go](../cmd/sentineld/main.go)). The returned `stop` cancels it.
- `sync.Once` for release: `Server.Release`
  ([server.go](../internal/core/transport/server.go)) runs the socket's
  release once, however many callers reach it (`Server.Close` and the
  daemon's Stop). Closing a lock file twice would be a bug, and a flag
  would need its own lock.

### Phase 3a: the first module

- Optional interfaces: a module implements `module.Warner`
  ([module.go](../internal/core/module/module.go)) only when it has
  warnings to report, and the registry asks with a type assertion,
  `if w, ok := c.(Warner); ok { … }`. The contract stays small, and a
  module that needs nothing more implements nothing more; `module.Router`
  of Phase 2c works the same way.
- Draining calls with `sync.WaitGroup`: the daemon counts the module
  commands in flight with a `WaitGroup` per instance and waits for them
  before `Stop` ([control.go](../internal/daemon/control.go),
  [daemon.go](../internal/daemon/daemon.go)). An `Add` from zero must
  happen before `Wait`, so `Add` runs under the daemon's mutex and only
  while the instance is open, and `stop` closes the instance under the
  same mutex before it waits; a later request is refused, not counted.
- A pure transition function: the supervisor's failure policy is
  `step(streak, ok, policy) (streak, transition)`, with no clock, file or
  event inside ([policy.go](../internal/modules/supervisor/policy.go)).
  The loop does the effects around it, and a table test covers every
  sequence without goroutines.
- A timeout that does not abandon the call: a check runs in its own
  goroutine with a result channel of capacity one, and the loop `select`s
  on it, a timer and the module's context. On timeout it keeps the channel
  as `stalled` for the next round, so the call is never counted twice nor
  left unseen, and `Stop` waits for it through a `WaitGroup`
  ([service.go](../internal/modules/supervisor/service.go)).
- Pointer fields tell absent from zero: `*int` and `*config.Duration` in
  a YAML struct stay nil when the key is missing, so the default applies,
  while an explicit `0` is a value to check
  ([config.go](../internal/modules/supervisor/config.go)).
- Coalescing writes with a generation counter: every change bumps `gen`,
  and a save writes the newest snapshot and records the generation it
  holds, so a save requested for an older generation returns at once
  ([module.go](../internal/modules/supervisor/module.go)).
- A custom unmarshaler that lets decoding go on: yaml.v3 collects a
  `*yaml.TypeError` returned by an `UnmarshalYAML` method and decodes the
  rest, while any other error stops the whole decode. `Duration`,
  `ByteSize`, `FileMode` ([scalars.go](../internal/core/config/scalars.go))
  and `Route` ([route.go](../internal/core/config/route.go)) return one,
  so a file with three bad values reports three problems.
- `errors.AsType`, the generic form of `errors.As`, returns the match and
  a boolean without declaring a variable first:
  `if te, ok := errors.AsType[*yaml.TypeError](e); ok { … }`
  ([problem.go](../internal/core/config/problem.go)).
- Validating a generic value or its pointer: `state.Load[T]` accepts a
  state type or a pointer to it. `validate` asks `*T` for a `Validate`
  method (a pointer receiver), then, when `T` itself is a pointer, asks
  the pointer and refuses a nil one (`reflect.Value.IsNil`), so JSON
  `null` never becomes a nil state
  ([store.go](../internal/core/state/store.go)).
- Error kinds that keep the text: `tlsconfig.LoadCA` tags its failures
  with `ErrUnsafe` or `ErrInvalid` through a small type whose
  `Unwrap() []error` returns both the kind and the original error, so
  `errors.Is` finds either while `Error()` stays the original message
  ([tlsconfig.go](../internal/core/tlsconfig/tlsconfig.go)). The
  supervisor matches the kind and prints its own message, which names no
  path.

### Phase 3b: running programs

- A locked thread forks every child: on Linux the parent-death signal
  (`SysProcAttr.Pdeathsig`) belongs to the thread that forked the child,
  and the child is killed if that thread exits. The executor starts every
  child from one goroutine that calls `runtime.LockOSThread` and serves
  start requests over a channel; on `Close` it unlocks the thread before
  returning, so the thread goes back to the runtime's pool and does not
  exit ([executor.go](../internal/platform/executor/executor.go)).
- `syscall.SysProcAttr`: `Setpgid` puts the child in a new process group
  (pgid = pid), so `syscall.Kill(-pgid, sig)` signals the program and
  everything it started. `Credential{Uid, Gid, Groups}` switches identity,
  and a nil `Groups` with `NoSetGroups` false drops every supplementary
  group ([exec_linux.go](../internal/platform/executor/exec_linux.go)).
- Pipes of our own: with `Stdout` set to an `io.Writer`, `exec.Cmd.Wait`
  waits for the copying goroutine, which a grandchild can keep blocked.
  The executor passes the write end of an `os.Pipe` (an `*os.File`, so
  `exec` adds no goroutine), closes its own copy, and bounds the read with
  a timer after the group is killed
  ([output.go](../internal/platform/executor/output.go)).
- A token bucket on the injected clock: `LineWriter` refills
  `elapsed × rate` tokens up to a burst and spends one per line, and tests
  advance a fake clock instead of sleeping
  ([linewriter.go](../internal/platform/executor/linewriter.go)).
- Opening a file without following anything: `os.OpenRoot` confines the
  open to a checked directory, `Lstat` refuses a final symbolic link
  before the open (`os.Root` may follow a link inside the root even with
  `O_NOFOLLOW`), `O_NONBLOCK` makes a FIFO fail instead of hanging, and `Stat` on the
  opened descriptor checks the type and `Nlink`
  ([appendfile.go](../internal/platform/executor/appendfile.go)).
- `sync.OnceValues` computes once a function that returns a value and an
  error, such as the integration tests' helper binary
  ([executor_test.go](../tests/integration/executor/executor_test.go)).
- A file system as an interface: the cleanup of a recorded process group
  reads `/proc` through an `fs.FS` (`os.DirFS("/proc")` in production),
  so its decisions are tested on any platform with `testing/fstest.MapFS`,
  a map from paths to file contents
  ([group.go](../internal/platform/executor/group.go)).
- Parsing from the last delimiter: `/proc/<pid>/stat` puts the command
  name, which any program chooses, between parentheses. The fields after
  it are found from `bytes.LastIndexByte(b, ')')`, so a name holding `) `
  cannot shift them ([group.go](../internal/platform/executor/group.go),
  `FuzzParseStat`).
- A process handle instead of a pid: on Linux `os.FindProcess` holds a
  pidfd when the kernel gives one, so `Process.Signal` reaches that
  process even if its pid is reused, and `Release` closes the descriptor.
  The cleanup re-reads the process after taking the handle and before
  signalling ([group_linux.go](../internal/platform/executor/group_linux.go)).
- Schema migrations as a map of steps: `state.LoadMigrating[T](store,
  steps)` applies `steps[v]` (a `func(json.RawMessage) (json.RawMessage,
  error)`) from the file's version to the current one, then decodes and
  validates once ([store.go](../internal/core/state/store.go)).
- Own before you look at the context: a process loop stores what `Launch`
  returned under the lock before it checks for cancellation, so a launch
  that completes during `Stop` is stopped like any other
  ([process.go](../internal/modules/supervisor/process.go)).
- Method expressions as functions: `time.Time.Compare` is the method
  turned into a function of two `time.Time`s, so it can be passed where a
  comparison is expected, as in `slices.SortFunc(attempts,
  time.Time.Compare)` and `slices.EqualFunc(a, b, time.Time.Equal)`
  ([recovery.go](../internal/modules/supervisor/recovery.go)). `==` on
  `time.Time` would also compare the location and the monotonic reading.
- A child context per item: each supervised service runs in
  `context.WithCancel(moduleCtx)` with a `done` channel closed when its
  loop returns, so a reload ends one service and waits for it while the
  others run, and cancelling the module's context still ends them all
  ([module.go](../internal/modules/supervisor/module.go) `startLoop`).
- Comparing copies with a field zeroed: `sameRun` copies two `Service`
  values, clears `Route` on both and compares the rest with
  `reflect.DeepEqual`, so what is ignored is written once and the other
  fields need no list
  ([reconfigure.go](../internal/modules/supervisor/reconfigure.go)).
- Fresh slices from a reducer: `reserve`
  ([recovery.go](../internal/modules/supervisor/recovery.go)) builds a
  fresh slice (copying the attempts, future ones clamped to now) and
  appends to it, never to `b.Attempts`. `append` may write
  into the caller's backing array when it has spare capacity, and a save
  serialising the old slice without the lock would race with it.
- A one-slot channel as a lock that can be tried or waited for with a
  deadline: `ops := make(chan struct{}, 1)`. `select { case ops <-
  struct{}{}: … default: … }` tries it (an operator's command answers
  "busy" instead of queueing), and a `select` with `<-ctx.Done()` waits
  for it until a deadline (a reload), which `sync.Mutex` cannot do
  ([operate.go](../internal/modules/supervisor/operate.go) `act`,
  [reconfigure.go](../internal/modules/supervisor/reconfigure.go)).
- `sync.OnceFunc` wraps a function so that only its first call runs: a
  loop closes its `decided` channel at its first decision from several
  places, and once more by `defer` when it returns, without a second
  `close` panicking ([module.go](../internal/modules/supervisor/module.go)
  `startLoop`).
- A request-scoped value in a context: `audit.WithCorrelation` puts the
  request's correlation id in the command's context under an unexported
  key type, so no other package can collide with it, and
  `audit.CorrelationFrom` reads it back
  ([context.go](../internal/core/audit/context.go)). Context values carry
  data that crosses API boundaries with one request (here, to link the
  module's audit records to the socket's), never optional parameters.
- An open `/proc/<pid>` as a handle on one process: `os.OpenRoot` on
  `/proc/<pid>` keeps a descriptor of the directory, so every file read
  through `root.FS()` comes from that process, and once it has ended the
  reads fail even if another process has its pid by then. The stat read
  first is checked against the recorded group, then `statm` is read
  through the same root
  ([rss_linux.go](../internal/platform/executor/rss_linux.go)).
- A function value carried by a result: `launched.rss` is a
  `func() (uint64, error)` the launcher sets for each process, so the
  module samples memory without a fourth method on its `launcher` or
  `proc` interfaces, and the fake launcher returns whatever a test needs
  ([launcher.go](../internal/modules/supervisor/launcher.go)).
- A periodic timer from single-shot ones: `clock.Clock` only has
  `NewTimer`, so `sampleTimer` arms a new one after each sample. A nil
  channel (no limit) is never ready in a `select`, so the same loop serves
  services with and without a limit
  ([memory.go](../internal/modules/supervisor/memory.go)).

### Phase 3c: systemd over D-Bus

- Decorating an interface by embedding: `deadlineConn` embeds a
  `net.Conn` and overrides `Write` (a write deadline before each write)
  and `Close` (once, through `sync.Once`); every other method is the
  embedded one's ([bus.go](../internal/platform/systemd/bus.go)).
  godbus writes a call while holding a lock that `Close` also takes, so a
  context alone cannot end a write to a bus that stops reading; the
  deadline can, and closing the socket first unblocks it at once.
- A fake protocol peer on `net.Pipe`: the D-Bus tests run the other end
  of the connection in the test, with the authentication lines and then
  messages decoded and encoded with godbus's own `DecodeMessage` and
  `EncodeTo`. Such a peer can stay silent, stop reading or send something
  unexpected, so the tests exercise the timeouts and the closing paths
  without a bus
  ([peer_test.go](../internal/platform/systemd/peer_test.go)).

### Phase 3c-3: cron run times

- Wall time held as UTC: `wall` turns an instant into its local
  wall-clock minute in a location and stores it as a `time.Time` in UTC
  ([next.go](../internal/core/cronexpr/next.go)). UTC has no daylight
  saving time, so two wall times compare and add as plain calendar values,
  and a jump of the local clock shows up as a gap or a step back between
  consecutive values.
- `t.Round(0)` drops the monotonic reading: a `time.Time` from `time.Now`
  carries one, and values derived with `Add` keep it. `Round(0)` returns
  the same instant without it, so a computed time compares with `==` and
  prints as a plain wall time ([next.go](../internal/core/cronexpr/next.go)).
- A plain reference implementation in the tests: `reference` applies the
  same rules as `Next` minute by minute, with no date jumps and its own
  day matching, and `TestNextMatchesScanOverAYear` and `FuzzNext` compare
  the two ([reference_test.go](../internal/core/cronexpr/reference_test.go)).
  The hand-computed tables remain the proof of the rules; the reference
  catches the shortcuts (date jumps, look-back).
- Benchmarks with `b.Loop()`: `func BenchmarkXxx(b *testing.B)` with
  `for b.Loop() { … }` (Go 1.24 and later) times the body, and the setup
  before the loop is not timed
  ([next_test.go](../internal/core/cronexpr/next_test.go)).
  `go test ./internal/core/cronexpr -run '^$' -bench Next -benchmem` runs it; nothing asserts a
  threshold.

### Phase 3c-4a: cron jobs

- Shared keys with `yaml:",inline"`: `programYAML` holds the keys of a
  program (command, args, user, environment, output, stop), and the YAML
  shapes of process services and jobs embed it as a field tagged
  `yaml:",inline"`, so its keys sit beside the type's own and the strict
  decode still refuses unknown ones
  ([config_process.go](../internal/modules/supervisor/config_process.go)).
- A wall clock moved in a synctest bubble: the job tests need the wall
  clock set back or forward while timers keep elapsed time, as the real
  monotonic timers do. `shiftedClock` implements `clock.Clock` with
  `time.Now()` plus an offset and the real timers, which in a
  `testing/synctest` bubble are fake and advance with `time.Sleep`
  ([job_test.go](../internal/modules/supervisor/job_test.go)).
- One owner for a result: a run's end can come from its exit, its timeout
  or a stop request, and only its `supervise` goroutine `select`s among
  them, so the result is claimed once without a lock or a flag
  ([job_run.go](../internal/modules/supervisor/job_run.go)).

### Phase 3c-5a: reading service logs

- A typed operation instead of a list of commands: `executor.Journal`
  takes a `JournalQuery`, and `journalArgs`, a pure function, is the whole
  allowlist ([journal.go](../internal/platform/executor/journal.go)).
  With one consumer there is no map of programs to allowed flags, and a
  table test and a fuzz target cover every argv it can build.
- A writer that keeps the tail: `tailWriter`
  ([journal.go](../internal/platform/executor/journal.go)) is an
  `io.Writer` that keeps its last bytes and compacts only when twice over
  its bound, so many small writes cost no copy each. Neither it nor
  `capWriter` can fail or wait, which is what a program's pipe needs from
  its sink.
- A slot as a buffered channel: `Reader.slots` is
  `make(chan struct{}, 4)`; a `select` with `default` takes a slot without
  waiting, and receiving gives it back
  ([reader.go](../internal/platform/logsource/reader.go)).
- Waiting for what cannot be interrupted: `Reader.File`
  ([reader.go](../internal/platform/logsource/reader.go)) reads in a
  goroutine and hands the result over a channel with a buffer of one, so
  the goroutine can always finish even if nobody waits any more, and the
  caller `select`s on it and `ctx.Done()`. This is the rare goroutine a
  context cannot stop: the read ends when the kernel says so, and it holds
  its slot until then.
- Reading JSON of several shapes: a journal field may be a string, `null`
  or an array. `jsontext.Value` keeps it undecoded, and its `Kind` says
  which decoding applies
  ([journal.go](../internal/platform/logsource/journal.go)).
- Proving a privilege drop by its effect: `TestRunsWithoutRoot` closes
  the journal to its group; root would still read it, so a refusal proves
  who ran ([logsource_test.go](../tests/integration/logsource/logsource_test.go)).

### Phase 3c-5b: program output in the journal

- A length-prefixed binary encoding: journald's native protocol takes a
  field as `NAME\n`, its length as a little-endian 64-bit integer, the
  value and `\n`. `binary.LittleEndian.AppendUint64` writes the length
  straight into the entry, with no buffer or `binary.Write`
  ([journald.go](../internal/platform/journald/journald.go)).
- A round-trip fuzz target against a decoder of the spec: `FuzzEncode`
  ([journald_test.go](../internal/platform/journald/journald_test.go))
  decodes what `Encode` wrote with a decoder written from the protocol
  and not from the encoder, so the two can disagree: any program line
  must come back as one value and add no field.
- One datagram, one socket: `journald.Send`
  ([send_linux.go](../internal/platform/journald/send_linux.go)) opens a
  socket, sends with `sendto` and closes it, as `sdnotify` does. There is
  no `connect` and no `write(2)` (R-024, D-079), and no descriptor shared
  among senders whose number could be reused after it is closed.
  `net.DialUnix` would connect and could wait in the poller;
  `MSG_DONTWAIT` makes a full socket an immediate `EAGAIN`.
- A sentinel wrapping an errno: `fmt.Errorf("%w: %w", ErrFull, err)` keeps
  both, so a caller matches `errors.Is(err, journald.ErrFull)` and the log
  still shows the errno
  ([send_linux.go](../internal/platform/journald/send_linux.go)).
- A seam set by the composition root: `supervisor.Options.JournalSend` is
  a function value. `cmd/sentineld` decides from its stderr whether to set
  it ([main.go](../cmd/sentineld/main.go);
  [load.go](../internal/daemon/load.go) passes it on), and the module
  never reads the environment, so a test passes a recorder or nil and
  means exactly that.
- State owned by the writer's contract: `outputSink.congested` has no
  lock, because the `LineWriter` calls a sink from the one goroutine that
  copies its stream, and its `Close` emits the last line only after that
  copy ended. What sinks share, the drop count and the warning flag, is
  `sync/atomic` on the instance; `CompareAndSwap` warns once per outage
  and re-arms after a send that worked
  ([output.go](../internal/modules/supervisor/output.go)).
- A bounded retry on an injected clock: the sink waits for a full socket
  with `clock.NewTimer`, never `time.Sleep`, so the tests run it in a
  `synctest` bubble and check how long it waited from the fake's
  timestamps ([output.go](../internal/modules/supervisor/output.go)).
