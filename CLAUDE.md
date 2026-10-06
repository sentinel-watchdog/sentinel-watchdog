# Sentinel — agent instructions

Read [PLAN.md](PLAN.md) first: it holds the phases, their status, the
decision log (D-xxx), risks (R-xxx) and open questions. Continue with the
first phase not marked `done` and update PLAN.md before ending a session.

## Conventions

- Go 1.27, module `github.com/sentinel-watchdog/sentinel` (placeholder
  owner, see PLAN.md Q-003). Code, comments and docs in English.
- Only external dependency: `go.yaml.in/yaml/v3`. Prefer the standard
  library; justify any new dependency in the decision log.
- Small interfaces, defined by the consumer. One responsibility per package;
  one monitor type per package under `internal/monitor/`.
- Errors wrapped with `%w` and context. Never log or persist secrets: use
  `internal/redact`.
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

Then update docs (docs/*.md, README feature table), CHANGELOG
(Unreleased) and PLAN.md checkboxes.
