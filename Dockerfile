# syntax=docker/dockerfile:1

ARG GO_VERSION=1.27.0
ARG ALPINE_VERSION=3.24

FROM golang:${GO_VERSION}-alpine${ALPINE_VERSION} AS build
WORKDIR /src

# Dependencies first so the module layer survives source edits.
COPY go.mod go.su[m] ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/stratus ./cmd/stratus

FROM gcr.io/distroless/static:nonroot AS runtime
COPY --from=build /out/stratus /usr/local/bin/stratus

ENV STRATUS_ADDR=":8080" \
    STRATUS_DATA_DIR="/data"
EXPOSE 8080

# No VOLUME: /data is expected to be a bind mount owned by the host user the
# container runs as. Docker does not carry image ownership into a fresh named
# volume, so relying on one would leave /data root-owned and unwritable.
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/stratus"]
