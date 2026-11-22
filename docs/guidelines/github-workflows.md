# GitHub workflows and repository automation

Applies to changes in `.github/`. The workflows themselves are listed in
[development.md](../development.md#continuous-integration).

- Pin every action to a full commit SHA with the version in a comment:
  `uses: owner/repo@<40-hex sha> # vX.Y.Z`. Find the SHA with
  `gh api repos/<owner>/<repo>/commits/<tag> --jq .sha`.
- Keep third-party actions to a minimum: a tool that can run with
  `go run` or `pipx run` runs that way. An action not owned by GitHub
  must be in the organisation allowlist (Settings → Actions → General);
  adding one needs maintainer approval.
- Default `permissions: contents: read` (or `{}`); a job that needs more
  declares it with a comment saying why.
- `actions/checkout` always uses `persist-credentials: false`.
- Never interpolate `${{ github.event.* }}` or other untrusted context
  directly into `run:`; pass it through `env:`.
- Tool versions are pinned, and every copy of a version changes in the
  same commit. `GOLANGCI_LINT_VERSION`, `GOVULNCHECK_VERSION`,
  `ACTIONLINT_VERSION` and `ZIZMOR_VERSION` are in the `env:` of
  `ci.yml` and in `Taskfile.yml`, and golangci-lint is also pinned in
  `.devcontainer/Dockerfile`. Task is pinned as `TASK_VERSION` in
  `ci.yml` and the Dockerfile. The release tools (`GORELEASER_VERSION`,
  `CYCLONEDX_GOMOD_VERSION`) and shfmt (`SHFMT_VERSION`) are pinned only
  in `Taskfile.yml`, because CI runs them through Task (`task snapshot`,
  `task lint-shell`; D-079, D-102), as are the package test images
  (`PACKAGE_IMAGE_DEBIAN`, `PACKAGE_IMAGE_ALPINE`, by digest).
- Dependabot (`.github/dependabot.yml`) proposes Go module and action
  updates weekly, actions in one grouped pull request. It waits 7 days
  after a release before proposing it (cooldown); security updates are
  not delayed.
- New CI jobs are added to the `needs:` list of `ci-ok`; never add a
  required check to the ruleset directly.
- Run `task lint-actions` (actionlint and zizmor, auditor persona) before
  committing; it must report no findings.
