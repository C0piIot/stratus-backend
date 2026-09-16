# syntax=docker/dockerfile:1

ARG GO_VERSION=1.27.0
ARG ALPINE_VERSION=3.24
# Two statically linked FFmpeg tools, copied in rather than installed. Switching
# the base to alpine or debian for a package would cost the three things this
# image guarantees -- no shell, no coreutils, static binaries -- to gain them.
#
# Ours rather than general-purpose builds: each carries only what Stratus uses,
# which build/ffprobe/Dockerfile and build/ffmpeg/Dockerfile enable by hand
# against the 128 MB a full static build costs. They are published by
# .github/workflows/media-tools.yml when a recipe changes, and this tag has to
# name a version it published -- a tag that was never published fails the build
# here, loudly, which is why nothing else guards it.
ARG FFMPEG_VERSION=7.1

# --platform=$BUILDPLATFORM keeps the toolchain native and cross-compiles to the
# target instead of emulating the whole build stage under QEMU. Go cross-compiles
# for free, so a multi-arch build needs no binfmt setup at all.
FROM ghcr.io/c0piiot/stratus-ffprobe:${FFMPEG_VERSION} AS ffprobe
FROM ghcr.io/c0piiot/stratus-ffmpeg:${FFMPEG_VERSION} AS ffmpeg

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine${ALPINE_VERSION} AS build
WORKDIR /src

# Dependencies first so the module layer survives source edits.
COPY go.mod go.su[m] ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
ARG VERSION=dev
# The commit's date rather than the clock's, so the same source still builds the
# same bytes. Stamped alongside the version and shown beside it.
ARG BUILD_DATE=unknown
ARG TARGETOS
ARG TARGETARCH
# COVER is empty for every image this project publishes, and set only by
# `make smoke-cover`, which builds a throwaway twin so the container suite can
# report what it covers. Instrumentation in a release artifact would be a
# performance cost and a file the server writes that nobody asked for; the
# argument exists so the measured binary is the same build as the shipped one
# in every other respect. atomic to match the mode `make cover` uses.
ARG COVER=
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath ${COVER:+-cover -covermode=atomic} \
      -ldflags "-s -w -X main.version=${VERSION} -X main.buildDate=${BUILD_DATE}" \
      -o /out/stratus ./cmd/stratus

FROM gcr.io/distroless/static:nonroot AS runtime
COPY --from=build /out/stratus /usr/local/bin/stratus

# One tool reads and the other decodes. ffprobe answers the duration of a track
# and the dimensions of a video, which is what the indexer needs; ffmpeg turns a
# HEIC or a video frame into pixels, which is what a thumbnail needs and what no
# pure-Go decoder can do without cgo.
#
# Both are requirements rather than optional extras: half a media library is
# worse than a server that says what it is missing.
COPY --from=ffprobe /ffprobe /usr/local/bin/ffprobe
COPY --from=ffmpeg /ffmpeg /usr/local/bin/ffmpeg

ENV STRATUS_ADDR=":8080" \
    STRATUS_DATA_DIR="/data"
EXPOSE 8080

# No VOLUME: /data is expected to be a bind mount owned by the host user the
# container runs as. Docker does not carry image ownership into a fresh named
# volume, so relying on one would leave /data root-owned and unwritable.
# Numeric rather than "nonroot:nonroot": it needs no passwd lookup, and hadolint
# DL3066 flags the symbolic form. 65532 is distroless's nonroot user.
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/stratus"]
