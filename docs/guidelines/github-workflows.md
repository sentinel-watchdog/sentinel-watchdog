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
  `ci.yml`, `release.yml` and the Dockerfile. cosign is pinned only in
  `release.yml`, as `COSIGN_VERSION` and `COSIGN_SHA256`: take the hash of
  `cosign-linux-amd64` from the release's `cosign_checksums.txt` and change
  both together (Dependabot does not see them). The release tools (`GORELEASER_VERSION`,
  `CYCLONEDX_GOMOD_VERSION`), shfmt (`SHFMT_VERSION`) and ShellCheck (`SHELLCHECK_VERSION` and its
  SHA-256 per platform, changed together) are pinned only
  in `Taskfile.yml`, because CI runs them through Task (`task snapshot`,
  `task lint-shell`; D-079, D-102), as are the package test images
  (`PACKAGE_IMAGE_DEBIAN`, `PACKAGE_IMAGE_ALPINE`, by digest).
- Dependabot (`.github/dependabot.yml`) proposes Go module and action
  updates weekly, actions in one grouped pull request. It waits 7 days
  after a release before proposing it (cooldown); security updates are
  not delayed.
- In `release.yml`, a job with a write or `id-token` permission checks out
  nothing and runs no repository code; a job that builds has neither. No
  job uses a cache. Its gates are tested by `task test-release`, which
  finds steps by job and name (D-103).
- New CI jobs are added to the `needs:` list of `ci-ok`; never add a
  required check to the ruleset directly.
- Run `task lint-actions` (actionlint and zizmor, auditor persona) before
  committing; it must report no findings.
