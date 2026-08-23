# Stratus backend.
#
# Go does not need to be installed on the host: by default every toolchain
# command runs in a container, so `docker` is the only hard prerequisite. If a
# matching Go is on PATH it is used directly instead (see GO_MODE).
#
# Precedence for settings: command line > .env > defaults below.
#   make up STRATUS_PORT=9000 STRATUS_DATA_PATH=/srv/stratus

-include .env

GO_VERSION     ?= $(shell awk '/^go /{print $$2}' go.mod)
ALPINE_VERSION ?= 3.24
DEBIAN_SUITE   ?= trixie
IMAGE          ?= stratus-backend
VERSION        ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# These mirror the interpolation names used by compose.yaml.
STRATUS_PORT      ?= 8080
STRATUS_DATA_PATH ?= ./data
STRATUS_LOG_LEVEL ?= info
STRATUS_UID       ?= $(shell id -u)
STRATUS_GID       ?= $(shell id -g)
STRATUS_VERSION   := $(VERSION)

export

COMPOSE := docker compose

# ---------------------------------------------------------------------------
# Toolchain: native go when it matches GO_VERSION exactly, container otherwise.
#
# The exact version match is what makes autodetection safe. Without it a stale
# local toolchain silently replaces the pinned one. CI sets GO_MODE=native
# explicitly so a broken setup-go step fails loudly instead of quietly falling
# back to Docker and merely getting slower.
# ---------------------------------------------------------------------------
GO_MODE ?= auto
ifeq ($(GO_MODE),auto)
  GO_MODE := $(if $(filter go$(GO_VERSION),$(shell go env GOVERSION 2>/dev/null)),native,docker)
endif

GO_IMAGE      := golang:$(GO_VERSION)-alpine$(ALPINE_VERSION)
GO_RACE_IMAGE := golang:$(GO_VERSION)-$(DEBIAN_SUITE)
LINT_VERSION  := $(shell cat .golangci-version)
GOVULNCHECK_VERSION ?= v1.7.0

# Caches are bind mounts under .cache/, not named volumes: a fresh named volume
# is created root-owned and the toolchain runs as the invoking user, so it could
# not write to it. Same failure mode that made /data a bind mount.
CACHE_DIR := $(CURDIR)/.cache

# Run the toolchain as the invoking user. As root it leaves build output in the
# working tree owned by root, unremovable without sudo, and it also makes the
# unwritable-directory tests silently pass since root ignores mode bits.
#
# No -t: a TTY injects carriage returns that break $(shell ...) captures.
DOCKER_RUN = docker run --rm \
	-u $(STRATUS_UID):$(STRATUS_GID) \
	-v "$(CURDIR)":/src -w /src \
	-v "$(CACHE_DIR)/go-mod":/gomodcache \
	-v "$(CACHE_DIR)/go-build":/gobuild \
	-e HOME=/tmp -e GOMODCACHE=/gomodcache -e GOCACHE=/gobuild \
	-e GOFLAGS=-buildvcs=false -e GOTOOLCHAIN=local

ifeq ($(GO_MODE),native)
  GO      := go
  GO_RACE := go
  GOFMT   := gofmt
else
  GO      := $(DOCKER_RUN) -e CGO_ENABLED=0 $(GO_IMAGE) go
  GOFMT   := $(DOCKER_RUN) $(GO_IMAGE) gofmt
  # -race needs the TSan runtime, which needs cgo and a C toolchain. The alpine
  # image has neither, so the race target uses the Debian image. This never
  # reaches the shipped artifact: the Dockerfile stays CGO_ENABLED=0.
  GO_RACE := $(DOCKER_RUN) -e CGO_ENABLED=1 $(GO_RACE_IMAGE) go
endif

LINT := docker run --rm -t -u $(STRATUS_UID):$(STRATUS_GID) \
	-v "$(CURDIR)":/src -w /src \
	-v "$(CACHE_DIR)/go-mod":/gomodcache \
	-v "$(CACHE_DIR)/go-build":/gobuild \
	-v "$(CACHE_DIR)/golangci":/lintcache \
	-e HOME=/tmp -e GOMODCACHE=/gomodcache -e GOCACHE=/gobuild \
	-e GOLANGCI_LINT_CACHE=/lintcache -e GOTOOLCHAIN=local \
	golangci/golangci-lint:$(LINT_VERSION)-alpine golangci-lint

# gofmt takes paths, not package patterns, so unlike the go tool it does NOT
# skip dot-directories -- it would walk .cache/ and flag vendored testdata.
# Paths are relative, so the same list is valid inside the container.
GO_FILES := $(shell find . -name '*.go' -not -path './.cache/*' -not -path './dist/*' 2>/dev/null)

# -shuffle=on catches tests that depend on execution order.
TEST_FLAGS ?= -shuffle=on -count=1

.DEFAULT_GOAL := help

## help: show this help
help:
	@awk 'BEGIN{FS=": "} /^## /{sub(/^## /,""); printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

## env: print the resolved toolchain and settings
env:
	@echo "GO_MODE       $(GO_MODE)"
	@echo "GO_VERSION    $(GO_VERSION)"
	@echo "VERSION       $(VERSION)"
	@echo "LINT_VERSION  $(LINT_VERSION)"
	@echo "arch          $(shell uname -m)"
	@echo "uid:gid       $(STRATUS_UID):$(STRATUS_GID)"
	@echo "data path     $(STRATUS_DATA_PATH)"

$(CACHE_DIR):
	@mkdir -p "$(CACHE_DIR)/go-mod" "$(CACHE_DIR)/go-build" "$(CACHE_DIR)/golangci"

# --- run ------------------------------------------------------------------

## up: build the image and start the backend in the background
up: $(STRATUS_DATA_PATH)
	$(COMPOSE) up -d --build --wait
	@echo "stratus $(VERSION) on http://localhost:$(STRATUS_PORT) (make logs / make health)"

# Created by the invoking user, which is who the container runs as.
$(STRATUS_DATA_PATH):
	@mkdir -p "$(STRATUS_DATA_PATH)"

## down: stop the backend, keeping the data directory
down:
	$(COMPOSE) down

## restart: recreate the backend container
restart: down up

## logs: follow backend logs
logs:
	$(COMPOSE) logs -f

## ps: show compose service status
ps:
	$(COMPOSE) ps

## health: curl the health endpoint
health:
	@curl -fsS http://localhost:$(STRATUS_PORT)/healthz >/dev/null && echo healthy || (echo unhealthy; exit 1)

# --- build ----------------------------------------------------------------

## image: build the runtime image without starting it
image:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) -t $(IMAGE):latest .

## build: compile the binary to ./dist/stratus
build: | $(CACHE_DIR)
	$(GO) build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o dist/stratus ./cmd/stratus

# --- quality --------------------------------------------------------------

## fmt: rewrite source with gofmt
fmt: | $(CACHE_DIR)
	@test -n "$(GO_FILES)" || { echo "no Go files"; exit 0; }
	$(GOFMT) -w -l $(GO_FILES)

## fmt-check: fail if any file is not gofmt-clean
fmt-check: | $(CACHE_DIR)
	@test -n "$(GO_FILES)" || { echo "no Go files"; exit 0; }
	@files=$$($(GOFMT) -l $(GO_FILES)); \
	if [ -n "$$files" ]; then echo "not gofmt-clean:"; echo "$$files"; exit 1; fi; \
	echo "gofmt clean ($(words $(GO_FILES)) files)"

## vet: run go vet
vet: | $(CACHE_DIR)
	$(GO) vet ./...

## lint: run golangci-lint
lint: | $(CACHE_DIR)
	$(LINT) run

## tidy: sync go.mod / go.sum
tidy: | $(CACHE_DIR)
	$(GO) mod tidy

## tidy-check: fail if go.mod / go.sum are not tidy
tidy-check: | $(CACHE_DIR)
	$(GO) mod tidy -diff

## vuln: scan for known vulnerabilities
vuln: | $(CACHE_DIR)
	$(GO) run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

# --- test -----------------------------------------------------------------

## test: run tests
test: | $(CACHE_DIR)
	$(GO) test $(TEST_FLAGS) ./...

## test-race: run tests under the race detector
test-race: | $(CACHE_DIR)
	$(GO_RACE) test -race $(TEST_FLAGS) ./...

## cover: run tests and report coverage
cover: | $(CACHE_DIR)
	$(GO) test $(TEST_FLAGS) -covermode=atomic -coverprofile=coverage.out ./...
	@$(GO) tool cover -func=coverage.out | tail -1

## smoke: build the image and assert its runtime properties
smoke:
	./scripts/smoke.sh

## ci: everything CI runs, in one command
ci: fmt-check vet lint tidy-check test-race smoke

# --- misc -----------------------------------------------------------------

## shell: open a shell in the Go toolchain container
shell: | $(CACHE_DIR)
	docker run --rm -it -u $(STRATUS_UID):$(STRATUS_GID) \
		-v "$(CURDIR)":/src -w /src -e HOME=/tmp \
		-v "$(CACHE_DIR)/go-mod":/gomodcache -v "$(CACHE_DIR)/go-build":/gobuild \
		-e GOMODCACHE=/gomodcache -e GOCACHE=/gobuild \
		$(GO_IMAGE) sh

## clean: remove build output and toolchain caches
clean:
	rm -rf dist coverage.out "$(CACHE_DIR)"

## clean-data: DESTRUCTIVE, stop and delete the data directory
clean-data: down
	rm -rf "$(STRATUS_DATA_PATH)"

## version: print the version this build would use
version:
	@echo $(VERSION)

.PHONY: help env up down restart logs ps health image build fmt fmt-check vet \
        lint tidy tidy-check vuln test test-race cover smoke ci shell clean \
        clean-data version
