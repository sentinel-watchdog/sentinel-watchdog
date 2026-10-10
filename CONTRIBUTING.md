# Contributing

Thanks for your interest in Sentinel Watchdog. The project is in early
development; this repository contains the agent and local CLI. Dashboard,
website, design and product coordination have [separate ownership](docs/project-layout.md).
The agent plan and its status are in [PLAN.md](PLAN.md) and the
architecture in [docs/adr/](docs/adr/README.md) (start with ADR-0015).

## Before you start

- **Security issues:** never in public issues — see [SECURITY.md](SECURITY.md).
- **Larger changes:** open an issue first. Design decisions are recorded
  in [docs/decisions.md](docs/decisions.md) or as an ADR, and a pull
  request should not change one silently.

## Development setup

Requirements, tasks and test levels are described in
[docs/development.md](docs/development.md); the change workflow (checks,
independent review, findings, human review, working with AI agents) in
[docs/development-workflow.md](docs/development-workflow.md); AI agents
follow [AGENTS.md](AGENTS.md). In short:

```sh
task check        # format, tidy, vet, lint, tests, race tests, build
task vuln         # govulncheck
task lint-actions # actionlint + zizmor, when you touch .github/workflows
```

## Rules for changes

- Code, comments, docs and commit messages in English.
- Small packages and consumer-defined interfaces; modules never import
  each other (ADR-0015).
- Table-driven tests with the standard library; no real systemd, root or
  network in unit tests. Linux-only integration tests use the
  `integration` build tag.
- Planned but missing features must fail loudly (configuration error or
  `unsupported`); never present a stub as implemented.
- Never log or persist secrets; use the core redaction package.
- New runtime dependencies need a decision-log entry (license, purpose,
  maintenance, size); the standard library is preferred.
- Update the documentation, `CHANGELOG.md` (Unreleased) and the PLAN.md
  checkboxes in the same pull request.

## Commits and pull requests

- [Conventional commits](https://www.conventionalcommits.org/):
  `feat`, `fix`, `refactor`, `docs`, `test`, `chore`, `perf`, `ci`.
  The pull request title follows the same format; it becomes the commit
  message when the pull request is squashed.
- Work on a dedicated branch for each change; do not commit directly to `main`.
- `main` is protected: changes arrive through pull requests with a green
  `ci-ok` check and linear history (squash or rebase).
- Fill in the pull request template, including the test plan and the full
  change from the target branch (`git diff <base>...HEAD`). Cross-project work
  needs separate PRs in the owning repositories; coordination files live in
  the maintainer's private coordination repository and are never part of
  an agent PR (D-073).

## License

By contributing you agree that your contributions are licensed under the
[Apache License 2.0](LICENSE).
