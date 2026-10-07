## Summary

<!-- What changes and why. Link the issue, PLAN.md phase and decision (D-xxx / ADR) if any. -->

## Type

<!-- feat | fix | refactor | docs | test | chore | perf | ci — the PR title uses the same prefix. -->

## Test plan

<!-- Commands run and what they showed; manual checks; what is not covered. -->

- [ ] `task check`
- [ ] `GOOS=linux GOARCH=arm64 go build ./...`
- [ ] `DOCKER_CONTEXT=… task test-linux` (if Linux behaviour changed)
- [ ] `task fuzz` (if a parser or loader changed)
- [ ] `task lint-actions` (if `.github/workflows` changed)

## Privileged code (delete if not applicable)

<!-- Required when the change touches config loading, files, processes, sockets, state, audit or the firewall
     (docs/guidelines/privileged-code.md). -->

- **Trust boundary:**
- **Abuse scenario considered:**
- **Regression test:**
- **Privileges needed and why:**

## Independent review

<!-- docs/development-workflow.md §3. Who reviewed (agent, session or person) and the range, e.g. main...HEAD and HEAD~1..HEAD for the fix. -->

- Reviewer and range(s):
- Findings: <!-- ID · severity · reproduced / rejected (reason) / deferred (link) · fixing commit -->

## Checklist

- [ ] Documentation updated (docs/, README)
- [ ] `CHANGELOG.md` (Unreleased) updated
- [ ] PLAN.md checkboxes / decision log updated
- [ ] No secrets in code, tests, logs or examples
