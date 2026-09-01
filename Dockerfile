# syntax=docker/dockerfile:1

ARG GO_VERSION=1.27.0
ARG ALPINE_VERSION=3.24
# A statically linked ffprobe, copied in rather than installed. Switching the
# base to alpine or debian for a package would cost the three things this image
# guarantees -- no shell, no coreutils, one static binary -- to gain one tool.
ARG FFMPEG_VERSION=7.1

# --platform=$BUILDPLATFORM keeps the toolchain native and cross-compiles to the
# target instead of emulating the whole build stage under QEMU. Go cross-compiles
# for free, so a multi-arch build needs no binfmt setup at all.
FROM mwader/static-ffmpeg:${FFMPEG_VERSION} AS ffmpeg

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine${ALPINE_VERSION} AS build
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

# ffprobe reads the duration of a track and the dimensions of a video. It is a
# requirement rather than an optional extra: half a media library is worse than
# a server that says what it is missing. Only ffprobe, not ffmpeg -- the encoder
# arrives with thumbnails, and it is another forty megabytes.
COPY --from=ffmpeg /ffprobe /usr/local/bin/ffprobe

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
