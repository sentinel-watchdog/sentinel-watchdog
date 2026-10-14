# Development container: Linux with Go and the tools of `task check`, for the
# parts of Sentinel that only run on Linux (peer credentials, the control
# socket's permissions, later systemd and processes). Used by VS Code and Zed
# (devcontainer.json) and by `task dev-shell` on any Docker context.
#
# Versions follow Taskfile.yml and .github/workflows/ci.yml: change them
# together (AGENTS.md, Dependencies).
FROM golang:1.27

ARG GOLANGCI_LINT_VERSION=v2.14.0
ARG TASK_VERSION=v3.54.0

# go install builds from source checked against the Go checksum database. Its
# module and build caches go to a throwaway path, removed in the same layer:
# root must not own the caches the dev user writes to.
RUN export GOPATH=/tmp/gotools GOCACHE=/tmp/gotools-cache GOBIN=/usr/local/bin \
    && go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@${GOLANGCI_LINT_VERSION} \
    && go install github.com/go-task/task/v3/cmd/task@${TASK_VERSION} \
    && rm -rf /tmp/gotools /tmp/gotools-cache

# A normal user, as in `task test-linux`: tests that check permissions must
# not run as root. Its Go caches are its own (the image's GOPATH, /go, is
# root's).
RUN useradd --create-home --uid 1000 --shell /bin/bash dev
USER dev
ENV CGO_ENABLED=0 GOTOOLCHAIN=local GOPATH=/home/dev/go
WORKDIR /home/dev
