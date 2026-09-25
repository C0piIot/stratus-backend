# syntax=docker/dockerfile:1

# A statically linked ffprobe carrying only what Stratus probes, published so
# the server image can COPY it in one line.
#
# The full static build everyone reaches for is 128 MB on amd64, and Stratus
# never decodes a frame: it runs -show_format -show_streams and reads a
# duration, a codec name, dimensions and container tags. That is demuxer work,
# plus the three audio decoders below. Everything else in that 128 MB --
# video decoders, encoders, filters, scalers, network protocols -- is weight
# the image carries and nothing uses.
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
# No encoders, and three decoders, all for audio. A codec name comes from the
# descriptor table that is always compiled in, not from the decoder -- but the
# width of a lossless track and the profile of an AAC one do not (#207): ffprobe
# learns them by opening a decoder on the track's configuration, and without
# one it prints N/A. Measured on this build, flac, alac and aac cost 0.4 MB, and
# without them every FLAC in a library is indexed with no bit depth, which is
# the number that says whether a client can take it as it is. The video ones
# are not worth the same trade: they are megabytes each, and the in-place
# readers answer for MP4 and Matroska without ffprobe at all.
#
# --enable-small is also why a profile comes out as a number rather than a
# name: it drops the name tables. internal/media maps the numbers back, which
# costs nothing here.
#
# The libraries ffprobe does not link are switched off outright, which is most
# of what is left.
RUN ./configure \
      --disable-everything --disable-doc --disable-network --disable-autodetect \
      --disable-shared --enable-static --enable-small \
      --disable-programs --enable-ffprobe \
      --disable-avdevice --disable-avfilter --disable-swscale \
      --disable-swresample --disable-postproc \
      --enable-protocol=file \
      --enable-demuxer=mov,matroska,avi,mpegps,mpegts,asf,mp3,flac,ogg,wav,aac \
      --enable-parser=h264,hevc,mpeg4video,vp8,vp9,aac,mpegaudio,flac,vorbis,opus \
      --enable-decoder=flac,alac,aac \
      --extra-ldflags=-static \
 && make -j"$(nproc)" ffprobe \
 && strip ffprobe

# The decoders are there for what they make ffprobe say, so that is what is
# asserted: a sixteen-bit FLAC has to come out sixteen bits. Without the flac
# decoder this prints N/A and nothing else fails.
COPY testdata/tiny.flac /src/tiny.flac
RUN set -eu; \
    depth="$(./ffprobe -v error -select_streams a:0 -show_entries stream=bits_per_raw_sample \
      -of default=noprint_wrappers=1:nokey=1 tiny.flac)"; \
    [ "$depth" = "16" ] || { echo "the FLAC probed as $depth bits, want 16"; exit 1; }

# scratch, so the server image copies a file and inherits nothing.
FROM scratch
COPY --from=build /src/ffprobe /ffprobe
