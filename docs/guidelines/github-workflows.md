# GitHub workflows and repository automation

Applies to changes in `.github/`.

- Pin every action to a full commit SHA with the version in a comment:
  `uses: owner/repo@<40-hex sha> # vX.Y.Z`. Find the SHA with
  `gh api repos/<owner>/<repo>/commits/<tag> --jq .sha`.
- An action not owned by GitHub must be in the organisation allowlist
  (Settings → Actions → General); adding one needs maintainer approval.
- Default `permissions: contents: read` (or `{}`); a job that needs more
  declares it with a comment saying why.
- `actions/checkout` always uses `persist-credentials: false`.
- Never interpolate `${{ github.event.* }}` or other untrusted context
  directly into `run:`; pass it through `env:`.
- Tool versions are pinned in `env:` and mirrored in `Taskfile.yml`;
  change both in the same commit.
- New CI jobs are added to the `needs:` list of `ci-ok`; never add a
  required check to the ruleset directly.
- Run `task lint-actions` (actionlint + zizmor, auditor persona) before
  committing; zero findings is required.
