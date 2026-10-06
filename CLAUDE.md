# Sentinel — agent instructions

Read [PLAN.md](PLAN.md) first: it holds the phases, their status, risks
(R-xxx) and open questions. Decisions (D-xxx) are in
[docs/decisions.md](docs/decisions.md), larger ones in
[docs/adr/](docs/adr/README.md); the architecture is
[ADR-0015](docs/adr/0015-modules-and-configuration-layout.md). Continue
with the first phase not marked `done` and update PLAN.md (and the
decision log) before ending a session.

## Conventions

- Project "Sentinel Watchdog"; Go 1.27, module
  `github.com/sentinel-watchdog/sentinel-watchdog`; binaries `sentineld`,
  `sentinelctl` (PLAN.md D-057). Code, comments and docs in English.
- Runtime dependencies: `go.yaml.in/yaml/v3` (client-go is planned for
  Phase 4d, D-055). Prefer the standard library; justify any new
  dependency in the decision log.
- The maintainer is learning Go: explain the Go idioms each phase
  introduces in `docs/development.md` and prefer clear, idiomatic code
  (D-060). Keep configuration simple with safe defaults (D-058).
- Layers (ADR-0015): `internal/core` (daemon services, pure libraries),
  `internal/platform` (shared OS/external adapters),
  `internal/modules/<name>` (one module each). Modules never import each
  other; only `internal/daemon` and `cmd/*` import modules.
- Small interfaces, defined by the consumer. One responsibility per package.
- Only the central configuration file configures the core and arms
  enforcing modules; module directories describe content only.
- Errors wrapped with `%w` and context. Never log or persist secrets: use
  the core redaction package.
- Planned-but-missing features must fail loudly (config error or
  `unsupported` status). Never mark a stub as implemented.
- Tests: table-driven, standard library only, no real systemd/root needed.
  Linux-only integration tests go behind `//go:build integration`.
- Inject time (`func() time.Time` or a Clock) wherever timing matters.

## Before finishing any change

```sh
task check   # fmt-check, vet, golangci-lint, test, test-race, build
GOOS=linux GOARCH=arm64 go build ./...
```

Then update docs (docs/*.md, README), CHANGELOG (Unreleased), PLAN.md
checkboxes and, for new decisions, docs/decisions.md. Commits follow
conventional commits.
