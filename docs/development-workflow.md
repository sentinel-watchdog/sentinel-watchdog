# Development workflow with AI agents

How a change goes from idea to `main` when AI agents help. Any agent and
any editor can be used: Claude Code, Codex, both, or others. All of them
follow the same instructions in [AGENTS.md](../AGENTS.md) (Claude Code
imports it from `CLAUDE.md`) and can use the prompt templates in
[docs/agents/](agents/):

| Template | Use |
|---|---|
| [review.md](agents/review.md) | independent, read-only review of a diff |
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

All of these are fine; pick per task.

| Mode | Who implements | Who reviews |
|---|---|---|
| Claude Code only | Claude Code | a fresh Claude Code session, or the maintainer |
| Claude Code, calling Codex | Claude Code (or Codex on request) | Codex (or Claude Code if Codex implemented) |
| Claude Code and Codex in parallel | each on its own branch or worktree | the other agent |
| Codex, calling Claude Code | Codex (or Claude Code on request) | Claude Code (or Codex if Claude implemented) |
| Codex only | Codex | a fresh Codex session (`/review`), or the maintainer |

In every mode the one who wrote a change is not its only reviewer; for
privileged code this is required (AGENTS.md, "Review").

### Calling the other agent from the command line

Use these from a terminal at the repository root, or ask your agent to
run them. Close stdin (`< /dev/null`) so that neither CLI waits for
input, and never put secrets in a prompt.

**Codex, from Claude Code or a shell:**

```sh
# Review (read-only); or simply: codex review --base main
codex exec --sandbox read-only -o "$PWD/.plans/<slug>/review-codex.md" \
  "$(cat docs/agents/review.md) Review: git diff main...HEAD" < /dev/null

# Plan (read-only)
codex exec --sandbox read-only -o "$PWD/.plans/<slug>/plan-codex.md" \
  "$(cat docs/agents/plan.md .plans/<slug>/brief.md)" < /dev/null

# Implement in a worktree (section 5)
codex exec --sandbox workspace-write -C ../sentinel-watchdog-<slug> \
  -o "$PWD/.plans/<slug>/implementation-codex.md" \
  "$(cat docs/agents/implement.md .plans/<slug>/task.md)" < /dev/null
```

**Claude Code, from Codex or a shell** (the prompt goes before the
options, because `--allowedTools` takes several values):

```sh
# Review or plan (plan mode is read-only)
claude -p "$(cat docs/agents/review.md) Review: git diff main...HEAD" \
  --permission-mode plan --allowedTools "Bash(git diff *)" "Bash(git log *)" \
  < /dev/null > .plans/<slug>/review-claude.md

# Implement in a worktree (section 5)
prompt="$(cat docs/agents/implement.md .plans/<slug>/task.md)"
out="$PWD/.plans/<slug>/implementation-claude.md"
(cd ../sentinel-watchdog-<slug> && claude -p "$prompt" \
  --permission-mode acceptEdits --allowedTools "Bash(go *)" "Bash(task *)" \
  < /dev/null > "$out")
```

In an editor (the VS Code extensions or the interactive CLIs) do the same
by pasting the template and the request into a new session.

## 3. Review and findings

Review the whole branch (`main...HEAD`), or only the fix
(`HEAD~1..HEAD`) after a previous review. To include uncommitted new
files in `git diff`, mark them with `git add -N <file>`.

For each finding, write down one outcome:

| Outcome | When | What to do |
|---|---|---|
| **Reproduced** | a test fails on the current code as described | write the test, see it fail, fix, rerun the checks |
| **Rejected** | impossible or out of scope (threat model, decision) | write the reason in the PR |
| **Deferred** | real but outside this change | issue or PLAN.md entry with its severity |

Severity guides priority, not truth. After fixing, review the fix itself
once more (`HEAD~1..HEAD`). List findings and outcomes in the PR
("Independent review").

## 4. Two plans for important changes

For an ADR, a loader, the recovery engine or firewall transactions it can
help to get two independent plans:

1. Write `.plans/<slug>/brief.md`: goal, constraints, acceptance
   criteria, out of scope — the problem, not the solution.
2. Each agent writes its plan with [plan.md](agents/plan.md)
   (`plan-claude.md`, `plan-codex.md`) **without reading the other**.
3. Each critiques the other's plan (same template, "Critiquing another
   agent's plan").
4. One agent writes `comparison.md`: criteria, disagreements, a merged
   proposal naming the source of each choice. The maintainer decides.

## 5. Delegating to another agent

Delegated work happens in its own worktree, so it never mixes with yours:

```sh
git worktree add ../sentinel-watchdog-<slug> -b feat/<slug> main
# write .plans/<slug>/task.md: what, acceptance criteria, scope, what not to touch
# run the other agent there (section 2), then review its work:
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
