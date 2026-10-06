# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project
uses [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- Phase 1 foundation:
  - YAML configuration model (`version: 1`) with main file and `conf.d/`
    fragments, deterministic load order, strict unknown-key detection with
    line numbers, `${VARIABLE}` expansion, defaults and rigorous validation.
  - Cron expression parser used for configuration validation.
  - Structured logging with text, JSON and journald formats and secret
    redaction.
  - Public event, monitor state and capability status model.
  - JSON state file with atomic writes, schema versioning, corruption
    quarantine and retention.
  - Taskfile, golangci-lint configuration, documentation and development
    plan.
