# syntax=docker/dockerfile:1

# golangci-lint, run against the toolchain this repository pins rather than the
# one its own image happens to carry.
#
# The published image bundles a Go of its own, and it lags: v2.14.0 ships
# go1.27.1, so with GOTOOLCHAIN=local -- which is not negotiable, see the
# Makefile -- it refuses a go.mod that asks for 1.27.2. The binary itself is
# fine with it; what was wrong was the toolchain underneath.
#
# This is also the shape CI runs in, where setup-go provides the toolchain and
# the action provides the binary. Keeping the two the same is the point: the
# version was already pinned in one file, and it was the *toolchain* that
# drifted, which cost a red build nobody could reproduce locally.
ARG GO_VERSION=1.27.2
ARG ALPINE_VERSION=3.24
ARG LINT_VERSION=v2.14.0

FROM golangci/golangci-lint:${LINT_VERSION}-alpine AS lint

FROM golang:${GO_VERSION}-alpine${ALPINE_VERSION}
COPY --from=lint /usr/bin/golangci-lint /usr/bin/golangci-lint
ENTRYPOINT ["golangci-lint"]
