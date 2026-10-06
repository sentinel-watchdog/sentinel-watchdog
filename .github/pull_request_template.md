## Summary

<!-- What changes and why. Link the issue, PLAN.md phase and decision (D-xxx / ADR) if any. -->

## Type

<!-- feat | fix | refactor | docs | test | chore | perf | ci — the PR title uses the same prefix. -->

## Test plan

<!-- Commands run and what they showed; manual checks; what is not covered. -->

- [ ] `task check`
- [ ] `GOOS=linux GOARCH=arm64 go build ./...`
- [ ] `task lint-actions` (if `.github/workflows` changed)

## Checklist

- [ ] Documentation updated (docs/, README)
- [ ] `CHANGELOG.md` (Unreleased) updated
- [ ] PLAN.md checkboxes / decision log updated
- [ ] No secrets in code, tests, logs or examples
