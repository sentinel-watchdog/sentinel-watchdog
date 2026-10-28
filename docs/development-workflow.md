# Development workflow with AI agents

How a change goes from idea to `main` when AI agents help. Any agent and
any editor can be used: Claude Code, Codex, Antigravity or others. All
of them follow the same instructions in [AGENTS.md](../AGENTS.md)
(Claude Code imports it from `CLAUDE.md`) and can use the prompt
templates in [docs/agents/](agents/):

| Template | Use |
|---|---|
| [review.md](agents/review.md) | independent, read-only review of a diff or of existing code |
| [plan.md](agents/plan.md) | implementation plan for a brief, or critique of another plan |
| [implement.md](agents/implement.md) | implementation of a task delegated by another agent |

Working material (briefs, plans, review reports) goes in
`.plans/<slug>/`, which is gitignored. Development tools run as you, on
your machine: these rules protect the project, not against your own
environment.

## 1. The loop

1. Pick the work from [PLAN.md](../PLAN.md); write assumptions and
   acceptance criteria that a test or a command can verify.
2. Branch from an up-to-date `main` (`feat/`, `fix/`, `docs/`, `chore/`,
   `ci/`, `test/`).
3. Implement; privileged code also follows
   [guidelines/privileged-code.md](guidelines/privileged-code.md).
4. Check:

   ```sh
   task check                                   # fmt, tidy, vet, lint, test, race, build
   GOOS=linux GOARCH=arm64 go build ./...
   DOCKER_CONTEXT=<context> task test-linux     # when Linux behaviour matters
   task fuzz FUZZTIME=10s                       # when a parser or loader changed
   task lint-actions                            # when .github/ changed
   git diff main...HEAD                         # read it: only the intended change
   ```

5. Review (section 3), then commit and open the PR — agents only when
   asked.

## 2. Ways of working

Any combination works; pick per task: one agent alone, several in
parallel on their own branches or worktrees, or one agent calling others
for plans and reviews.

How you start and call agents (an editor, a CLI, a skill, a script of
your own) is your choice and stays on your machine. The repository
ignores `.agents/`, `.claude/skills/`, `.claude/commands/` and
`.claude/agents/`, so personal skills, commands and their settings are
never committed (D-091). Whatever you use:

- gives other agents the templates above for reviews, plans and
  delegated implementation; a tool may add prompts of its own, not
  replace these;
- keeps planning and reviewing agents read-only, and delegated
  implementation in its own worktree (section 5);
- keeps working material in `.plans/<slug>/`;
- never puts a secret in a prompt (section 7);
- never makes the author of a change its only reviewer; for privileged
  code this is required (AGENTS.md, "Review").

## 3. Review and findings

Review the whole branch (`main...HEAD`), or only the fix
(`HEAD~1..HEAD`) after a previous review. To include uncommitted new
files in `git diff`, mark them with `git add -N <file>`.

For each finding, write down one outcome:

| Outcome | When | What to do |
|---|---|---|
| **Reproduced** | a test fails on the current code as described | write the test, see it fail, fix, rerun the checks |
| **Rejected** | impossible or out of scope (threat model, decision) | write the reason in the PR |
| **Deferred** | real but outside this change | entry in [backlog.md](backlog.md) (or an issue) with its severity |

Severity guides priority, not truth. After fixing, review the fix itself
once more (`HEAD~1..HEAD`). List findings and outcomes in the PR
("Independent review").

## 4. Two plans for important changes

For an ADR, a loader, the recovery engine or firewall transactions it can
help to get two independent plans:

1. Write `.plans/<slug>/brief.md`: goal, constraints, acceptance
   criteria, out of scope — the problem, not the solution.
2. Each agent writes its plan with [plan.md](agents/plan.md)
   (`plan-<agent>.md`) **without reading the other**.
3. Each critiques the other's plan (same template, "Critiquing another
   agent's plan").
4. One agent writes `comparison.md`: criteria, disagreements, a merged
   proposal naming the source of each choice. The maintainer decides.

## 5. Delegating to another agent

Delegated work happens in its own worktree, so it never mixes with yours:

```sh
git worktree add ../sentinel-watchdog-<slug> -b feat/<slug> main
# write .plans/<slug>/task.md: what, acceptance criteria, scope, what not to touch
# run the other agent there with docs/agents/implement.md, then review:
git -C ../sentinel-watchdog-<slug> add -N .     # show new files in the diff
git -C ../sentinel-watchdog-<slug> diff HEAD
```

If the result is good, commit on that branch and open the PR as usual;
otherwise `git worktree remove --force ../sentinel-watchdog-<slug>` and
`git branch -D feat/<slug>`.

## 6. When the maintainer must review the diff

Before merge, read the diff (not only the summary) when the change:

- touches privileged code (configuration loading, files, processes,
  socket and authorization, audit, state, firewall, Kubernetes);
- changes `.github/`, `Taskfile.yml`, `.golangci.yml`, `go.mod`/`go.sum`,
  `AGENTS.md`, `CLAUDE.md`, `.claude/` or `docs/agents/`;
- adds or changes an ADR, a decision, the threat model or `SECURITY.md`;
- leaves a medium-or-higher finding rejected or deferred;
- prepares a release or a tag (`v*` tags can never be moved).

## 7. Secrets

- Agents never read credentials: `.env*`, `*.pem`, `*.key`, SSH keys,
  `~/.ssh`, `~/.aws`, agent auth files (`~/.codex/auth.json`),
  `~/.config/gh/hosts.yml`, tokens in history or environment.
- `.claude/settings.json` denies Claude Code's file tools on these paths
  and force or mirror pushes. Textual rules do not catch every shell
  form; GitHub branch protection is the real barrier for `main`. Codex
  and other agents rely on AGENTS.md and their own sandbox settings.
- A prompt sent to an agent leaves your machine: never include secrets.
  If one is exposed, rotate it.
