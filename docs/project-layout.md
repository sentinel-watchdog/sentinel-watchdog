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
| `sentinel-watchdog-dashboard` | Fleet backend services, web interface, accounts, fleet storage and dashboard deployment (one monorepo; microservices on Kubernetes) |
| Agent–backend contract repository (future, public) | Versioned protocol between the Remote module and the dashboard; created when Remote work starts |
| Maintainer's private coordination repository | Product roadmap, cross-project decisions, discovery and commercial research |

Design, website and dashboard repositories are private until their
development starts (D-073). The coordination repository stays private. Its
materials are not required to clone, build, test or understand the agent;
no repository-local Markdown links or build inputs depend on it or on
sibling paths. The dashboard keeps frontend and services in one repository.
Additional repositories need a defined responsibility.

## Plans and decisions

`PLAN.md` tracks agent delivery only. Phase 5 owns the agent's Remote
module and integration validation, not dashboard implementation. Product
roadmap changes affecting the agent must be reflected here through a
reviewed decision and PR before changing implementation order.

Agent-specific decisions remain in `docs/decisions.md` and `docs/adr/`.
Cross-project decisions live in the coordination repository.
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
events/status, compatibility, offline behaviour and revocation. It lives in
its own public repository (D-073); its distribution format is a design
decision still to make. Consumers use an explicit version without sibling
build dependencies.

The cross-project contract and agent ADR must agree. The local central file
remains authoritative for module switches, safety gates and command execution
permissions (ADR-0015). A dashboard cannot expand them.

## Workflow and current status

Each repository uses a dedicated branch and PR for its own changes; commits,
checks and release versions are independent. One agent PR cannot deliver
coordination files or changes in another repository.

Design, website and dashboard repositories exist as private scaffolds; no
dashboard implementation exists. This organisation work does not complete a
runtime development phase.
