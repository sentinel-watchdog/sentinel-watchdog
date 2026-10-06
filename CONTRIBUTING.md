# Contributing

Thanks for your interest in Sentinel Watchdog. The project is in early
development; the plan and its status are in [PLAN.md](PLAN.md) and the
architecture in [docs/adr/](docs/adr/README.md) (start with ADR-0015).

## Before you start

- **Security issues:** never in public issues — see [SECURITY.md](SECURITY.md).
- **Larger changes:** open an issue first. Design decisions are recorded
  in [docs/decisions.md](docs/decisions.md) or as an ADR, and a pull
  request should not change one silently.

## Development setup

Requirements, tasks and test levels are described in
[docs/development.md](docs/development.md). In short:

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
- `main` is protected: changes arrive through pull requests with a green
  `ci-ok` check and linear history (squash or rebase).
- Fill in the pull request template, including the test plan.

## License

By contributing you agree that your contributions are licensed under the
[Apache License 2.0](LICENSE).
