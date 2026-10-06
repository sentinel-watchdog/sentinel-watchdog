# Project boundaries

Sentinel Watchdog is split into three independent projects:

| Project | Owns |
|---|---|
| `sentinel-watchdog` | Go runtime, daemon, CLI, config, tests, packaging, ADRs and technical docs |
| `sentinel-watchdog-design` | Visual identity, logo masters and exports, icons and mockups |
| `sentinel-watchdog-website` | Presentation website, docs navigation and publication |

The design project is named `design` rather than `ui` because it contains
design resources. A future dashboard may become a separate UI project.

## Documentation

Technical docs remain in this repository and are reviewed with the code.
The website imports them from explicit release tags and publishes versioned
docs. Development docs must be clearly labelled. Marketing copy and site
navigation remain in the website project. D-059's website implementation
and publishing tasks belong to that project; core roadmap milestones track
coordination without claiming that a website already exists.

## Visual assets

The design project owns the canonical logo and its generation provenance.
This repository retains `assets/branding/sentinel-watchdog.png` as a local
export for README rendering. The initial export was copied unchanged to
the design project on 2026-10-06; no design release tag exists yet. Future
updates should record the source design tag or commit in `docs/branding.md`.

Consumers use local exports, not sibling-path references or Git submodules.
Each repository must work independently after cloning.

## Current status

The core repository already exists. The design and website directories
contain initial documentation; the design project also contains the PNG.
No remote repositories, hosting or additional Git repositories have been
created. This split does not complete any runtime development phase.
