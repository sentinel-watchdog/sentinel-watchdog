# Contributing

Thanks for your interest in Sentinel Watchdog. This repository contains
the agent and its local CLI; the dashboard, website, design and product
coordination have [separate ownership](docs/project-layout.md). The plan
and its status are in [PLAN.md](PLAN.md#7-phases) and the architecture in
[docs/adr/](docs/adr/README.md) (start with ADR-0015).

## Before you start

- Report security issues privately, never in public issues; see
  [SECURITY.md](SECURITY.md).
- Open an issue before a larger change. Design decisions are recorded in
  [docs/decisions.md](docs/decisions.md) or as an ADR, and a pull request
  should not change one silently.

## Development setup

Requirements, tasks and test levels are in
[docs/development.md](docs/development.md); the change workflow (checks,
independent review, findings, human review, working with AI agents) is in
[docs/development-workflow.md](docs/development-workflow.md); AI agents
follow [AGENTS.md](AGENTS.md). Run `task check` before every pull request;
the other tasks are listed in [Tasks](docs/development.md#tasks).

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

- Work on a dedicated branch for each change, created from an up-to-date
  `main` and named by type (`feat/`, `fix/`, `docs/`, `chore/`, `ci/`,
  `test/`; `refactor` and `perf` use the closest of these); do not commit
  directly to `main`.
- Commit messages follow [Conventional Commits](https://www.conventionalcommits.org/)
  with the types `feat`, `fix`, `refactor`, `docs`, `test`, `chore`,
  `perf` and `ci`. The pull request title uses the same format; it becomes
  the commit message when the pull request is squashed.
- `main` is protected: changes arrive through pull requests, merged by
  squash or rebase (linear history) once the `ci-ok` check is green.
- Fill in the pull request template, including the test plan and the full
  change from the target branch (`git diff <base>...HEAD`). Cross-project
  work needs separate pull requests in the owning repositories;
  coordination files live in the maintainer's private coordination
  repository and are never part of an agent pull request (D-073).

### Labels

`task labels` creates or updates the label set on GitHub (it needs `gh`
and write access):

| Label | Use |
|---|---|
| `bug` | something does not work as documented |
| `enhancement` | new feature or improvement |
| `documentation` | documentation only |
| `security` | security hardening or vulnerability fix |
| `dependencies` | dependency updates |
| `ci` | CI, workflows and repository automation |
| `module:core` | core: config, events, state, notifications, socket, CLI |
| `module:supervisor` | Supervisor module |
| `module:firewall` | Firewall module |
| `needs-decision` | blocked on a design decision (ADR or decision log) |
| `good first issue` | small, well-scoped task for newcomers |

## License

By contributing you agree that your contributions are licensed under the
[Apache License 2.0](LICENSE).
