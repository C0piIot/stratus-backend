# syntax=docker/dockerfile:1

# A statically linked ffprobe carrying only what Stratus probes, published so
# the server image can COPY it in one line.
#
# The full static build everyone reaches for is 128 MB on amd64, and Stratus
# never decodes a frame: it runs -show_format -show_streams and reads a
# duration, a codec name, dimensions and container tags. That is demuxer work.
# Everything else in that 128 MB -- decoders, encoders, filters, scalers,
# network protocols -- is weight the image carries and nothing uses.
ARG ALPINE_VERSION=3.24
ARG FFMPEG_VERSION=7.1

FROM alpine:${ALPINE_VERSION} AS build
ARG FFMPEG_VERSION

# A failed download must not become an empty source tree that builds nothing.
SHELL ["/bin/ash", "-o", "pipefail", "-c"]

# Package versions are not pinned: ALPINE_VERSION is, and pinning inside it
# would break the build on every 3.24.x patch release for tools whose output is
# verified by the tests rather than by their version.
# hadolint ignore=DL3018
RUN apk add --no-cache build-base curl nasm tar xz yasm

WORKDIR /src
RUN curl -fsSL "https://ffmpeg.org/releases/ffmpeg-${FFMPEG_VERSION}.tar.xz" | tar xJ --strip-components=1

# THE DEMUXER LIST MIRRORS byExtension IN internal/media/media.go. An extension
# added there without its demuxer here does not fail the build -- it fails the
# probe, in production, for that format only. scripts/smoke.sh asserts one file
# per supported extension so that divergence fails in CI instead.
#
#   mp4 mov m4a m4v -> mov        mkv webm -> matroska      mpg mpeg -> mpegps
#   wma wmv         -> asf        ogg opus -> ogg           and one each for
#                                                           mp3 flac wav aac avi
#
# No decoders and no encoders: a codec name comes from the descriptor table
# that is always compiled in, not from the decoder. The libraries ffprobe does
# not link are switched off outright, which is most of what is left.
RUN ./configure \
      --disable-everything --disable-doc --disable-network --disable-autodetect \
      --disable-shared --enable-static --enable-small \
      --disable-programs --enable-ffprobe \
      --disable-avdevice --disable-avfilter --disable-swscale \
      --disable-swresample --disable-postproc \
      --enable-protocol=file \
      --enable-demuxer=mov,matroska,avi,mpegps,mpegts,asf,mp3,flac,ogg,wav,aac \
      --enable-parser=h264,hevc,mpeg4video,vp8,vp9,aac,mpegaudio,flac,vorbis,opus \
      --extra-ldflags=-static \
 && make -j"$(nproc)" ffprobe \
 && strip ffprobe

# scratch, so the server image copies a file and inherits nothing.
FROM scratch
COPY --from=build /src/ffprobe /ffprobe
