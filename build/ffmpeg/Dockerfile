# syntax=docker/dockerfile:1

# A statically linked ffmpeg carrying only what Stratus decodes, published so
# the server image can COPY it in one line.
#
# Its sibling build/ffprobe/Dockerfile reads files and this one turns them into
# pixels. Two recipes rather than one: a single configure run would give ffprobe
# the decoders it has no use for, and two small static binaries weigh less than
# two large ones.
#
# What needs it, and why it is built before any of them: HEIC, which is what an
# iPhone records and which no pure-Go decoder can read without cgo; a frame out
# of a video; the picture embedded in a track; and the audio transcoding that
# OpenSubsonic clients ask for on mobile data.
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

# THE DECODER LIST MIRRORS byExtension IN internal/media/media.go, the same way
# the demuxer list does for ffprobe. An extension added there without its
# decoder here does not fail the build -- it fails the thumbnail, in production,
# for that format only.
#
#   heic heif        -> hevc, through the mov demuxer: HEIF is ISOBMFF, which
#                       is the sort of thing no flag name tells you
#   mp4 mov m4v mkv  -> h264 hevc vp8 vp9
#   webm             -> vp8 vp9
#   mpg mpeg         -> mpeg2video
#   avi              -> mpeg4 msmpeg4v3
#   wmv              -> vc1 wmv3
#   mts m2ts m2t     -> mpegts, which is AVCHD out of a camcorder and what a
#                       set-top box records; the video inside is h264, or vc1
#                       and mpeg2video off a disc, and all three are above
#
# Only the image and video containers. The audio ones ffprobe enables are not
# here because nothing decodes audio yet -- that arrives with transcoding -- and
# neither is image2: JPEG, PNG and the rest are decoded in Go, which is the
# whole reason this binary is only for the formats Go cannot read.
#
# **This binary only decodes and scales.** The JPEG is written in Go, so the
# output is a rawvideo frame already reduced to the size asked for -- about
# 360 KB, against the 48 MB a full-size 12 MP frame would push through the pipe.
# That is why there is no mjpeg encoder and no image2 muxer here.
#
# swscale stays (the scale filter is the whole point) and swresample goes: no
# audio is decoded yet. It arrives with transcoding, along with an encoder and
# libmp3lame, and the list here grows with it.
#
# transpose, hflip and vflip are not used by any command this project writes.
# They are what ffmpeg itself inserts when it straightens a frame whose
# container says it was recorded rotated, which every phone held upright does --
# and without them that is not a sideways thumbnail, it is a failed one: the
# filter does not exist, so the command errors out.
RUN ./configure \
      --disable-everything --disable-doc --disable-network --disable-autodetect \
      --disable-shared --enable-static --enable-small \
      --disable-programs --enable-ffmpeg \
      --disable-avdevice --disable-swresample --disable-postproc \
      --enable-protocol=file,pipe \
      --enable-demuxer=mov,matroska,avi,mpegps,mpegts,asf \
      --enable-decoder=hevc,h264,vp8,vp9,mpeg2video,mpeg4,msmpeg4v3,vc1,wmv3 \
      --enable-parser=h264,hevc,mpeg4video,vp8,vp9 \
      --enable-filter=scale,format,null,transpose,hflip,vflip \
      --enable-encoder=rawvideo --enable-muxer=rawvideo \
      --extra-ldflags=-static \
 && make -j"$(nproc)" ffmpeg \
 && strip ffmpeg

# A HEIF image of 96x64, 423 bytes, HEVC Main 10 -- ten bits rather than the
# eight a phone writes, so the pixel format conversion is exercised rather than
# skipped. It is in the build context so that the assertion below runs on both
# architectures, natively, in CI: a smoke check against the server image could
# only reach whichever one built it.
COPY testdata/tiny.heic /src/tiny.heic

# The build asserts what it was asked for. A typo in a configure flag is
# otherwise paid for a year later, by the first thumbnail of that format: a
# disabled decoder is not a build error, it is a missing entry in a table.
#
# The names below are the decoders' own and not always the configure options':
# --enable-decoder=msmpeg4v3 registers a decoder called msmpeg4. This check
# caught that on its first run, which is the sort of thing it is for.
RUN set -eu; \
    for decoder in hevc h264 vp8 vp9 mpeg2video mpeg4 msmpeg4 vc1 wmv3; do \
      ./ffmpeg -hide_banner -decoders 2>/dev/null | grep -qE "^ V[.A-Z]* +${decoder} " \
        || { echo "the ${decoder} decoder is not in this build"; exit 1; }; \
    done; \
    for filter in scale transpose hflip vflip; do \
      ./ffmpeg -hide_banner -filters 2>/dev/null | grep -qE " ${filter} " \
        || { echo "the ${filter} filter is not in this build"; exit 1; }; \
    done; \
    ./ffmpeg -hide_banner -muxers 2>/dev/null | grep -qE " rawvideo " \
      || { echo "the rawvideo muxer is not in this build"; exit 1; }; \
    for demuxer in "mov,mp4" "matroska,webm" "avi" "mpegts"; do \
      ./ffmpeg -hide_banner -demuxers 2>/dev/null | grep -qE " ${demuxer}" \
        || { echo "the ${demuxer} demuxer is not in this build"; exit 1; }; \
    done

# And then it does the job it was built for, on a real file: decode a HEIC and
# emit the scaled RGBA a thumbnail is made from. 32 x 21 x 4 bytes, because the
# fixture is 96x64 and -1 keeps the aspect ratio.
#
# This is the assertion that matters. Every list above can be right while the
# binary still cannot read the format somebody actually uploads -- HEIF is read
# through the mov demuxer, which is the kind of thing no flag name tells you.
RUN set -eu; \
    bytes="$(./ffmpeg -hide_banner -v error -i tiny.heic -vf scale=32:-1 \
      -f rawvideo -pix_fmt rgba - | wc -c)"; \
    [ "$bytes" -eq 2688 ] || { echo "the HEIC decoded to $bytes bytes, want 2688"; exit 1; }

# scratch, so the server image copies a file and inherits nothing.
FROM scratch
COPY --from=build /src/ffmpeg /ffmpeg
