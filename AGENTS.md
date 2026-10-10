# Sentinel Watchdog — instructions for AI coding agents

This file is for any agent (Claude Code reads it through `CLAUDE.md`,
Codex and others read it directly). Humans follow the same rules through
[CONTRIBUTING.md](CONTRIBUTING.md).

Linux agent (`sentineld` daemon, `sentinelctl` CLI) written in Go 1.27,
module `github.com/sentinel-watchdog/sentinel-watchdog`. It runs **as root**
and will restart services and change the firewall: treat every change as
security-relevant. This repository holds only the agent
([project layout](docs/project-layout.md)); never add build inputs or links
that depend on sibling directories. Code, comments, docs and commits in
English.

## Where things are

- Plan and status: [PLAN.md](PLAN.md) — work on the first phase not `done`;
  unscheduled work and deferred findings in [docs/backlog.md](docs/backlog.md).
  Decisions: [docs/decisions.md](docs/decisions.md) (D-xxx) and
  [docs/adr/](docs/adr/README.md).
- Layers ([ADR-0015](docs/adr/0015-modules-and-configuration-layout.md)):
  `internal/core` (daemon services, pure libraries) · `internal/platform`
  (OS/external adapters) · `internal/modules/<name>` · `internal/daemon` +
  `cmd/*` (composition root) · `pkg` (public types). `internal/archtest`
  enforces the import rules; a failing archtest is a design error, not a
  test to adjust.
- Working with several agents (review, planning, delegation):
  [docs/development-workflow.md](docs/development-workflow.md); prompt
  templates in [docs/agents/](docs/agents/).

## Commands

```sh
task check                                  # fmt-check, tidy-check, vet, golangci-lint, test, race, build
GOOS=linux GOARCH=arm64 go build ./...      # Linux cross-build
DOCKER_CONTEXT=<context> task test-linux    # glibc+race and musl, non-root, in Docker
task fuzz FUZZTIME=10s                      # every Fuzz* target
task vuln                                   # govulncheck
task lint-actions                           # when .github/ changes
```

## Design principles

The maintainer is learning Go with this project: the code should be a
good example. Apply the classics (Clean Code, Clean Architecture, SOLID,
Design Patterns, The Pragmatic Programmer) **the Go way**, never as
ceremony:

- **Go idioms first** ([Effective Go](https://go.dev/doc/effective_go),
  [Go Code Review Comments](https://go.dev/wiki/CodeReviewComments),
  [Go Proverbs](https://go-proverbs.github.io/)): clear is better than
  clever; accept interfaces, return concrete types; composition, not
  hierarchies; errors are values; make the zero value useful; a little
  copying is better than a little dependency.
- **Clean Architecture** is ADR-0015: dependencies point inward (modules
  → core, never core → modules); logic does not import OS adapters;
  `cmd/` and `internal/daemon` wire everything; `internal/archtest`
  enforces it.
- **SOLID in Go:** one responsibility per package and per type; small
  interfaces owned by their consumer (interface segregation, dependency
  inversion); extension through the module registry, not inheritance.
- **Clean Code:** names short in small scopes and descriptive across
  packages, without stutter (`config.Load`, not `config.ConfigLoader`);
  functions that do one thing; comments that explain why; no dead code.
- **Pragmatic Programmer:** DRY applies to knowledge, not to code that
  looks alike (abstract on the third repetition); orthogonal modules;
  tracer bullets (a thin end-to-end slice first); crash early on invalid
  state; validate contracts at the boundaries.
- **KISS and YAGNI:** use a design pattern only for a problem present
  today — strategy as an interface or a function value, functional
  options, adapters for the platform. Avoid getters and setters,
  factories for a single type, `I`-prefixed interfaces, `util`/`common`
  packages and deep type hierarchies.
- When principles conflict, choose the simpler code and write down why.
- A Go idiom used for the first time gets an entry in "Go patterns used
  here" in [docs/development.md](docs/development.md) (D-060).

## Changing code

- Before a non-trivial change: state assumptions, the plan and verifiable
  acceptance criteria; get approval when the plan changes a decision,
  an ADR, a dependency or privileged behaviour.
- Make the smallest change that meets the criteria; no unrelated edits in
  the diff.
- A new interface, registry, factory, adapter or layer needs a named
  variation it isolates or a test it enables; write that reason in its doc
  comment. Otherwise use a concrete type.
- Interfaces live in the consuming package and have 1–3 methods. The
  exception is a shared abstraction with a real and a fake
  implementation used by many packages, such as `clock.Clock`: it lives
  with its implementations.
- Inject time (`clock.Clock`) and external effects (runners, clients);
  logic must be testable without root, systemd, network or real time.
- Every goroutine has an owner that stops it through `context.Context`;
  every blocking I/O has a timeout or a context.
- Planned-but-missing features fail loudly (config error or `unsupported`);
  never leave a stub that looks implemented.
- No `panic` in normal control flow; recover only at module boundaries.

## Dependencies

- Standard library first. A new module in `go.mod` requires a decision
  entry (license, purpose, maintenance, size) and explicit approval.
- Only `internal/core/config` imports the YAML library (archtest).
- Development tools run with `go run pkg@vX.Y.Z`; their versions are pinned
  in `Taskfile.yml`, `.github/workflows/ci.yml` and
  `.devcontainer/Dockerfile` and changed together.

## Errors, logging, configuration

- Return errors with context: `fmt.Errorf("read %s: %w", path, err)`;
  match with `errors.Is/As`. Never discard an error without a comment
  saying why.
- Configuration problems are collected into `*config.ValidationError`
  (file, path, line); report all problems, not the first.
- Log with `log/slog` through `internal/core/logging`; never log or persist
  secrets — values pass through the core redaction package.
- Only the central configuration file configures the core and arms
  enforcing modules (ADR-0015 rule 1); every key has a safe default (D-058).

## Tests

- Table-driven tests with `t.Run`, standard library only.
- Unit tests need no root, systemd, network or real time; Linux-only or
  real-software tests use `//go:build integration` under
  `tests/integration/<area>/`.
- `go test -race` passes for every package (part of `task check`).
- Every parser or loader of untrusted input has a `Fuzz*` target.
- A fixed bug or review finding gets a regression test that fails before
  the fix.

## Security

- Privileged code follows
  [docs/guidelines/privileged-code.md](docs/guidelines/privileged-code.md)
  (read it before changing `internal/core/{config,state,audit,transport,authz}`,
  `internal/platform`, `internal/modules`, `internal/daemon`, `cmd`,
  `deploy`, `packaging`); workflows follow
  [docs/guidelines/github-workflows.md](docs/guidelines/github-workflows.md).
- Never read, print or send credentials (`.env*`, `*.pem`, `*.key`,
  `~/.ssh`, `~/.aws`, agent auth files, tokens), including in prompts to
  another agent. If one may have been exposed, say so and recommend
  rotation.
- Name the Docker context explicitly (`DOCKER_CONTEXT=…`) before any
  `docker` command.

## Git

- One branch per change from up-to-date `main`; never commit to `main`.
- Commit, push, open or merge a PR only when the maintainer asks; never
  force-push to shared branches.
- Conventional commits (`feat|fix|refactor|docs|test|chore|perf|ci`), no
  AI attribution trailers.
- Each PR updates CHANGELOG (Unreleased), PLAN checkboxes and the docs it
  affects; new decisions go to docs/decisions.md.

## Review

- Changes to privileged code, `.github/`, `go.mod` or agent instructions
  are reviewed by someone who did not write them: another agent, a fresh
  session with [docs/agents/review.md](docs/agents/review.md), or the
  maintainer. For other changes a review is recommended.
- A reviewing or planning agent works read-only.
- Each finding is reproduced with a failing test and fixed, or rejected
  with a written reason.

## Done

A change is done when `task check` and the Linux cross-build pass (plus
`test-linux`, `fuzz` or `lint-actions` when relevant), the diff contains
only the change, docs/CHANGELOG/PLAN match the code, and review findings
are resolved. Say explicitly what failed or was skipped. At the end of a
non-trivial task, summarise the changes, the commands run with their
results, review findings, risks and decisions the maintainer must take.
