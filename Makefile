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
VERSION        ?= $(shell git describe --tags --match "v*" --always --dirty 2>/dev/null || echo dev)

# These mirror the interpolation names used by compose.yaml.
STRATUS_PORT      ?= 8080
STRATUS_DATA_PATH ?= ./data
STRATUS_LOG_LEVEL ?= info
BASE              ?= http://localhost:8080
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

## deps: check the modules linked into the binary against deps.allow
deps: build
	@$(GO) version -m dist/stratus | ./scripts/deps.sh

## deps-update: record the modules linked into the binary in deps.allow
deps-update: build
	@$(GO) version -m dist/stratus | ./scripts/deps.sh --update

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

## cover: run tests with coverage and enforce the per-package floors
#
# The services are up for this one: without them the S3 and PostgreSQL suites
# skip, their packages collapse, and scripts/coverage.sh turns those silent skips
# into a failed build.
cover: | $(CACHE_DIR)
	@$(MAKE) --no-print-directory silo-up postgres-up
	@$(GO_SVC) test $(TEST_FLAGS) -covermode=atomic -coverprofile=coverage.out ./...; status=$$?; \
		$(MAKE) --no-print-directory silo-down postgres-down; exit $$status
	@$(GO) tool cover -func=coverage.out | tail -1
	@./scripts/coverage.sh coverage.out

## demo: put the demo media into a running instance (BASE=http://localhost:8080)
#
# The same script CI runs against the test instance after every deploy. It needs
# the credentials that instance was configured with.
demo:
	@BASE="$(BASE)" ./scripts/seed-demo.sh

## smoke: build the image and assert its runtime properties
smoke:
	./scripts/smoke.sh

## smoke-cover: the same suite again, against an instrumented image
#
# What `make cover` cannot see: the container suite drives the binary from the
# outside, so every statement it reaches -- cmd/stratus above all, which no unit
# test imports -- reads as uncovered. This runs it a second time against a
# `go build -cover` twin of the image and converts the counters into a profile.
#
# It is a separate profile rather than one merged into coverage.out on purpose.
# A floor that an end-to-end run can satisfy stops being a statement about unit
# tests, and the number worth having here is what the container exercises.
#
# The directory is 0777 because the containers write it as whoever they run as:
# the host user for the serving cases, the image's nonroot uid for the startup
# failure matrix.
smoke-cover: | $(CACHE_DIR)
	@rm -rf coverage-smoke coverage-smoke.out
	@mkdir -m 777 -p coverage-smoke
	@COVER=1 COVERDIR="$(CURDIR)/coverage-smoke" ./scripts/smoke.sh
	@$(GO) tool covdata textfmt -i=coverage-smoke -o=coverage-smoke.out
	@printf '\n\033[1mContainer coverage\033[0m\n'
	@$(GO) tool covdata percent -i=coverage-smoke | \
		awk '{ sub(/^.*stratus-backend\//, "", $$1); sub(/%$$/, "", $$3); \
		       printf "  %-28s %5s%%\n", $$1, $$3 }'
	@printf '\n  profile: coverage-smoke.out\n\n'

## ci: everything CI runs, in one command
ci: fmt-check vet lint tidy-check test-race test-s3 test-db cover smoke smoke-cover

# --- services for tests ---------------------------------------------------
#
# The conformance suites need the real thing: Silo for the storage port,
# PostgreSQL and MySQL for the metadata port. Without them those tests skip, which is what
# keeps `make test` useful on a machine with nothing running -- and is exactly
# why `make ci` runs the targets below, so a skip never becomes permanent.
#
# Silo rather than MinIO since #116: MinIO's community edition was archived in
# April 2026 with no release since September 2025, so the image pinned here was
# the last one that would ever exist and no security fix was coming. Silo is a
# maintained fork of it, which is why this is an image name and not a rewrite --
# same environment variables, same port, same health endpoint.
#
# What that buys in diff it gives up in independence, and the trade is #20's:
# a fork of MinIO holds MinIO's opinion of what S3 means, so it cannot catch an
# assumption shaped like MinIO's behaviour. Garage was the alternative and is
# written up there.
#
# The client library is a different project and is not going anywhere: minio-go
# is Apache-2.0 and actively released, and the S3 driver keeps using it.

SILO_IMAGE     ?= pgsty/silo:RELEASE.2026-09-03T13-18-01Z
POSTGRES_IMAGE ?= postgres:18-alpine
MYSQL_IMAGE    ?= mysql:8.4

TEST_NET       := stratus-test
SILO_NAME      := stratus-test-silo
SILO_USER      := stratus
SILO_PASS      := stratus-test-secret
POSTGRES_NAME  := stratus-test-postgres
POSTGRES_USER  := stratus
POSTGRES_PASS  := stratus-test-secret
# MySQL creates a database per case, which needs an account that can, so this
# one is root. It is a container the target below creates and destroys.
MYSQL_NAME     := stratus-test-mysql
MYSQL_USER     := root
MYSQL_PASS     := stratus-test-secret

# CI has a native toolchain and reaches the services on their published ports;
# the container toolchain reaches them by name on a shared docker network.
ifeq ($(GO_MODE),native)
  S3_ENDPOINT   := 127.0.0.1:9000
  POSTGRES_HOST := 127.0.0.1:5432
  MYSQL_HOST    := 127.0.0.1:3306
  GO_SVC        := go
else
  S3_ENDPOINT   := $(SILO_NAME):9000
  POSTGRES_HOST := $(POSTGRES_NAME):5432
  MYSQL_HOST    := $(MYSQL_NAME):3306
  GO_SVC         = $(DOCKER_RUN) --network $(TEST_NET) -e CGO_ENABLED=0 \
                     -e STRATUS_TEST_S3_ENDPOINT=$(SILO_NAME):9000 \
                     -e STRATUS_TEST_S3_ACCESS_KEY=$(SILO_USER) \
                     -e STRATUS_TEST_S3_SECRET_KEY=$(SILO_PASS) \
                     -e STRATUS_TEST_POSTGRES_DSN=$(POSTGRES_DSN) \
                     -e STRATUS_TEST_MYSQL_DSN=$(MYSQL_DSN) \
                     $(GO_IMAGE) go
endif

POSTGRES_DSN   := postgres://$(POSTGRES_USER):$(POSTGRES_PASS)@$(POSTGRES_HOST)/postgres?sslmode=disable
MYSQL_DSN      := mysql://$(MYSQL_USER):$(MYSQL_PASS)@$(MYSQL_HOST)/mysql

# Target-specific, not global: exporting these everywhere would stop `make test`
# from skipping and make it fail instead whenever the services are not running.
test-s3 test-db cover: export STRATUS_TEST_S3_ENDPOINT := $(S3_ENDPOINT)
test-s3 test-db cover: export STRATUS_TEST_S3_ACCESS_KEY := $(SILO_USER)
test-s3 test-db cover: export STRATUS_TEST_S3_SECRET_KEY := $(SILO_PASS)
test-s3 test-db cover: export STRATUS_TEST_POSTGRES_DSN := $(POSTGRES_DSN)
test-s3 test-db cover: export STRATUS_TEST_MYSQL_DSN := $(MYSQL_DSN)

## test-s3: run the storage conformance suite against a throwaway Silo
test-s3: | $(CACHE_DIR)
	@$(MAKE) --no-print-directory silo-up
	@$(GO_SVC) test $(TEST_FLAGS) ./internal/storage/s3/...; status=$$?; \
		$(MAKE) --no-print-directory silo-down; exit $$status

## test-db: run the metadata conformance suite against PostgreSQL and MySQL
test-db: | $(CACHE_DIR)
	@$(MAKE) --no-print-directory postgres-up mysql-up
	@$(GO_SVC) test $(TEST_FLAGS) ./internal/db/...; status=$$?; \
		$(MAKE) --no-print-directory postgres-down mysql-down; exit $$status

$(TEST_NET):
	@docker network inspect $(TEST_NET) >/dev/null 2>&1 || docker network create $(TEST_NET) >/dev/null

## silo-up: start the throwaway Silo used by test-s3
#
# MINIO_ROOT_USER and the health path are the fork's inheritance, not a typo:
# Silo keeps MinIO's interface, which is the whole reason it was chosen.
silo-up: $(TEST_NET)
	@docker rm -f $(SILO_NAME) >/dev/null 2>&1 || true
	@docker run -d --name $(SILO_NAME) --network $(TEST_NET) -p 127.0.0.1:9000:9000 \
		-e MINIO_ROOT_USER=$(SILO_USER) -e MINIO_ROOT_PASSWORD=$(SILO_PASS) \
		$(SILO_IMAGE) server /data >/dev/null
	@for i in $$(seq 1 100); do \
		curl -fsS http://127.0.0.1:9000/minio/health/live >/dev/null 2>&1 && \
			{ echo "silo ready on $(S3_ENDPOINT)"; exit 0; }; \
		sleep 0.2; \
	done; \
	echo "silo did not become healthy:"; docker logs $(SILO_NAME); exit 1

## silo-down: remove the throwaway Silo
silo-down:
	@docker rm -f $(SILO_NAME) >/dev/null 2>&1 || true

## postgres-up: start the throwaway PostgreSQL used by test-db
postgres-up: $(TEST_NET)
	@docker rm -f $(POSTGRES_NAME) >/dev/null 2>&1 || true
	@docker run -d --name $(POSTGRES_NAME) --network $(TEST_NET) -p 127.0.0.1:5432:5432 \
		-e POSTGRES_USER=$(POSTGRES_USER) -e POSTGRES_PASSWORD=$(POSTGRES_PASS) \
		-e POSTGRES_DB=postgres \
		$(POSTGRES_IMAGE) >/dev/null
	@for i in $$(seq 1 150); do \
		docker exec $(POSTGRES_NAME) pg_isready -q -U $(POSTGRES_USER) >/dev/null 2>&1 && \
			{ echo "postgres ready on $(POSTGRES_HOST)"; exit 0; }; \
		sleep 0.2; \
	done; \
	echo "postgres did not become ready:"; docker logs $(POSTGRES_NAME); exit 1

## mysql-up: start the throwaway MySQL used by test-db
#
# Slower to come up than the others, which is what the longer wait is for:
# MySQL initialises a data directory on first start, and a suite that began
# before it finished would fail as a connection error rather than as a test.
mysql-up: $(TEST_NET)
	@docker rm -f $(MYSQL_NAME) >/dev/null 2>&1 || true
	@docker run -d --name $(MYSQL_NAME) --network $(TEST_NET) -p 127.0.0.1:3306:3306 \
		-e MYSQL_ROOT_PASSWORD=$(MYSQL_PASS) -e MYSQL_DATABASE=stratus \
		$(MYSQL_IMAGE) >/dev/null
	@for i in $$(seq 1 300); do \
		docker exec $(MYSQL_NAME) mysqladmin ping -h 127.0.0.1 --silent >/dev/null 2>&1 && \
			{ echo "mysql ready on $(MYSQL_HOST)"; exit 0; }; \
		sleep 0.5; \
	done; \
	echo "mysql did not become ready:"; docker logs $(MYSQL_NAME); exit 1

## mysql-down: remove the throwaway MySQL
mysql-down:
	@docker rm -f $(MYSQL_NAME) >/dev/null 2>&1 || true

## postgres-down: remove the throwaway PostgreSQL
postgres-down:
	@docker rm -f $(POSTGRES_NAME) >/dev/null 2>&1 || true

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
        lint tidy tidy-check vuln deps deps-update demo test test-race test-s3 test-db silo-up \
        silo-down postgres-up postgres-down cover smoke smoke-cover ci shell clean \
        clean-data version
