# Agent repository and product boundaries

This repository owns the Sentinel Watchdog **agent**: `sentineld`, the
local `sentinelctl`, core services, platform adapters, compiled modules,
configuration, tests, packaging and agent technical documentation.
`internal/core` is an agent layer, not a backend shared by all products.

## Ownership

| Project / location | Owns |
|---|---|
| `sentinel-watchdog` | Agent code, local CLI, agent PLAN, releases, ADRs, security model, configuration and operations docs |
| `sentinel-watchdog-design` | Brand, canonical visual assets, exports and design resources |
| `sentinel-watchdog-website` | Public website, presentation content, docs navigation and publication |
| Future dashboard project(s) | Fleet backend, web interface, accounts, fleet storage and dashboard deployment |
| Workspace parent, temporarily | Product roadmap, cross-project decisions, discovery and commercial research |

The workspace parent currently holds `README.md`, `ROADMAP.md`,
`DECISIONS.md` and `docs/`. It is not a Git repository. These local product
materials may move into a coordination repository later. They are not
required to clone, build, test or understand the agent; no repository-local
Markdown links or build inputs depend on their sibling paths.
Dashboard repository names and any frontend/backend split remain undecided.
Start with one dashboard repository unless independent ownership or release
cycles justify more. Additional repositories need a defined responsibility.

## Plans and decisions

`PLAN.md` tracks agent delivery only. Phase 5 owns the agent's Remote
module and integration validation, not dashboard implementation. Product
roadmap changes affecting the agent must be reflected here through a
reviewed decision and PR before changing implementation order.

Agent-specific decisions remain in `docs/decisions.md` and `docs/adr/`.
Cross-project decisions live in the workspace coordination documents.
Existing D-xxx records remain as history; D-068 clarifies their scope.
Product research is summarised locally only where it constrains agent work.

## Documentation and visual assets

Agent technical docs are reviewed with the code and versioned with agent
releases. The website imports them from explicit release tags; development
docs are labelled separately. Future dashboard docs belong to its project
and follow its releases. Website copy and navigation belong to the website.

The design project owns canonical visual assets and provenance. Consumers
retain selected local exports, such as `assets/branding/sentinel-watchdog.png`,
with a source design tag or commit recorded in `docs/branding.md`. The initial
PNG was copied unchanged on 2026-10-06; no design release tag exists yet.
No sibling-path references or Git submodules are needed for these assets.

## Agent–backend contract

The Remote module implements the agent side of an outbound connection.
The backend and web interface belong to dashboard project(s). The local
Unix-socket API in `pkg/api` is not implicitly the fleet protocol.

Before Phase 5 implementation, agree one authoritative, versioned contract:
enrollment, identity/authentication, bounded messages, configuration delivery,
events/status, compatibility, offline behaviour and revocation. Its owning
repository and distribution format are a design decision still to make;
consumers must use an explicit version without sibling build dependencies.

The cross-project contract and agent ADR must agree. The local central file
remains authoritative for module switches, safety gates and command execution
permissions (ADR-0015). A dashboard cannot expand them.

## Workflow and current status

Each repository uses a dedicated branch and PR for its own changes; commits,
checks and release versions are independent. One agent PR cannot deliver
workspace-only files or changes in another repository. Until coordination
has a repository, parent documents require separate review and backup.

Only the agent has a Git repository here today. Design and website are local
scaffolds; no dashboard implementation or repository has been created. This
organisation work does not complete a runtime development phase.
