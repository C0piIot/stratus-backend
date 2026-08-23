# Stratus backend.
#
# Go is deliberately NOT installed on the host: every toolchain command runs in a
# throwaway container, so `docker` is the only prerequisite.
#
# Precedence for settings: command line > .env > defaults below.
#   make up STRATUS_PORT=9000 STRATUS_DATA_PATH=/srv/stratus

-include .env

GO_VERSION     ?= 1.27.0
ALPINE_VERSION ?= 3.24
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

COMPOSE  := docker compose
GO_IMAGE := golang:$(GO_VERSION)-alpine$(ALPINE_VERSION)

# Module and build caches live in named volumes so repeat runs stay fast.
# -buildvcs=false keeps go from poking at the bind-mounted .git as another user.
GO := docker run --rm -t \
	-v "$(CURDIR)":/src -w /src \
	-v stratus-go-mod:/go/pkg/mod \
	-v stratus-go-build:/root/.cache/go-build \
	-e CGO_ENABLED=0 -e GOFLAGS=-buildvcs=false \
	$(GO_IMAGE) go

.DEFAULT_GOAL := help

## help: show this help
help:
	@awk 'BEGIN{FS=": "} /^## /{sub(/^## /,""); printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

## up: build the image and start the backend in the background
up: $(STRATUS_DATA_PATH)
	$(COMPOSE) up -d --build
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
	@curl -fsS http://localhost:$(STRATUS_PORT)/healthz >/dev/null && echo "healthy" || (echo "unhealthy"; exit 1)

## image: build the runtime image without starting it
image:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) -t $(IMAGE):latest .

## build: compile the binary to ./dist/stratus
build:
	$(GO) build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o dist/stratus ./cmd/stratus

## test: run the test suite
test:
	$(GO) test ./...

## fmt: format all Go source
fmt:
	$(GO) fmt ./...

## vet: run go vet
vet:
	$(GO) vet ./...

## tidy: sync go.mod / go.sum
tidy:
	$(GO) mod tidy

## shell: open a shell in the Go toolchain container
shell:
	docker run --rm -it -v "$(CURDIR)":/src -w /src \
		-v stratus-go-mod:/go/pkg/mod -v stratus-go-build:/root/.cache/go-build \
		$(GO_IMAGE) sh

## clean: remove build output and toolchain caches
clean:
	rm -rf dist
	-docker volume rm stratus-go-mod stratus-go-build

## clean-data: DESTRUCTIVE, stop and delete the data directory
clean-data: down
	rm -rf "$(STRATUS_DATA_PATH)"

## version: print the version this build would use
version:
	@echo $(VERSION)

.PHONY: help up down restart logs ps health image build test fmt vet tidy shell clean clean-data version
