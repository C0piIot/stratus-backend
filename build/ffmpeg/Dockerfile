# syntax=docker/dockerfile:1

# A statically linked ffmpeg carrying only what Stratus decodes and encodes,
# published so the server image can COPY it in one line.
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
#
# lame-dev carries a static libmp3lame and openssl-libs-static a static OpenSSL;
# pkgconf is how configure finds the second one's own link flags.
# hadolint ignore=DL3018
RUN apk add --no-cache build-base curl nasm tar xz yasm pkgconf lame-dev openssl-dev openssl-libs-static

# libopus is built here because Alpine packages it shared only, and a static
# binary cannot link a .so. Pinned by digest, which is Xiph's own SHA256SUMS.txt.
ARG OPUS_VERSION=1.5.2
ARG OPUS_SHA256=65c1d2f78b9f2fb20082c38cbe47c951ad5839345876e46941612ee87f9a7ce1
WORKDIR /opus
RUN curl -fsSL -o opus.tar.gz "https://downloads.xiph.org/releases/opus/opus-${OPUS_VERSION}.tar.gz" \
 && echo "${OPUS_SHA256}  opus.tar.gz" | sha256sum -c - \
 && tar xzf opus.tar.gz --strip-components=1 \
 && ./configure --prefix=/usr/local --disable-shared --enable-static \
      --disable-doc --disable-extra-programs \
 && make -j"$(nproc)" install

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
# image2 is not here: JPEG, PNG and the rest are decoded in Go, which is the
# whole reason this binary is only for the pictures Go cannot read.
#
# **For pictures it only decodes and scales.** The JPEG is written in Go, so the
# output is a rawvideo frame already reduced to the size asked for -- about
# 360 KB, against the 48 MB a full-size 12 MP frame would push through the pipe.
# That is why there is no mjpeg encoder and no image2 muxer here.
#
# **For audio it transcodes** (#50), which is what an OpenSubsonic client asks
# for on mobile data. The audio demuxers and decoders mirror the audio half of
# byExtension, the same way the video ones do:
#
#   mp3              -> mp3
#   flac             -> flac
#   m4a aac          -> aac alac, through mov and the raw aac demuxer
#   ogg opus         -> vorbis opus
#   wav              -> pcm, in every width a WAV is written in
#   wma              -> wmav1 wmav2 wmapro, through asf
#
# The encoders are what a client can ask for. aac and flac are FFmpeg's own; mp3
# and opus are not, because FFmpeg has no mp3 encoder at all and its opus one is
# marked experimental and refuses to run without -strict. libmp3lame and libopus
# are the only two libraries this recipe links that FFmpeg does not ship, and
# neither makes the build GPL.
#
# The muxers are the containers those encoders go out in: mp3, adts for a bare
# AAC stream a player can start before the end, ogg and opus, flac, and ipod for
# an m4a. swresample stays for them -- a client asking for opus gets 48 kHz
# whatever the file was.
#
# **And it can read over HTTP, TLS included.** A source that is not a local file
# -- a blob in a bucket -- is read by ranges rather than copied first: an m4a
# whose moov atom sits at the end cannot be read from a pipe, and a transcode
# that waited for a whole track to download would start late on every play.
# OpenSSL rather than a smaller TLS library because it is the one FFmpeg is
# tested against most; it is what --enable-version3 is for, and the binary is
# LGPLv3 rather than 2.1. **FFmpeg does not verify a certificate unless it is
# told to**: a caller
# pointing it at https passes -tls_verify 1 and the image's CA bundle, and one
# that does not is trusting the network.
#
# transpose, hflip and vflip are not used by any command this project writes.
# They are what ffmpeg itself inserts when it straightens a frame whose
# container says it was recorded rotated, which every phone held upright does --
# and without them that is not a sideways thumbnail, it is a failed one: the
# filter does not exist, so the command errors out.
#
# thumbnail is used, and it is what stops a thumbnail being a black rectangle.
# Measured on a film that opens with two seconds of black: the first frame has
# an average brightness of 0.0 and so does the frame one second in, which is
# where this project used to ask for one. The filter scores a batch of frames
# against their own average and hands back the least ordinary of them, which on
# the same file is 125.3.
RUN ./configure \
      --disable-everything --disable-doc --disable-network --disable-autodetect \
      --disable-shared --enable-static --enable-small \
      --disable-programs --enable-ffmpeg \
      --disable-avdevice --disable-postproc \
      --enable-version3 --enable-libmp3lame --enable-libopus --enable-openssl \
      --pkg-config-flags=--static \
      --enable-network --enable-protocol=file,pipe,tcp,tls,http,https \
      --enable-demuxer=mov,matroska,avi,mpegps,mpegts,asf,mp3,flac,ogg,wav,aac \
      --enable-decoder=hevc,h264,vp8,vp9,mpeg2video,mpeg4,msmpeg4v3,vc1,wmv3 \
      --enable-decoder=mp3,mp3float,flac,aac,alac,vorbis,opus,wmav1,wmav2,wmapro \
      --enable-decoder=pcm_s16le,pcm_s24le,pcm_s32le,pcm_f32le,pcm_u8 \
      --enable-parser=h264,hevc,mpeg4video,vp8,vp9,mpegaudio,flac,aac,opus,vorbis \
      --enable-filter=scale,format,null,transpose,hflip,vflip,thumbnail \
      --enable-filter=aresample,aformat,anull \
      --enable-encoder=rawvideo --enable-muxer=rawvideo \
      --enable-encoder=aac,flac,libmp3lame,libopus \
      --enable-muxer=mp3,adts,ogg,opus,flac,ipod \
      --enable-bsf=aac_adtstoasc \
      --extra-cflags=-I/usr/local/include --extra-ldflags="-static -L/usr/local/lib" \
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
    for filter in scale transpose hflip vflip thumbnail; do \
      ./ffmpeg -hide_banner -filters 2>/dev/null | grep -qE " ${filter} " \
        || { echo "the ${filter} filter is not in this build"; exit 1; }; \
    done; \
    for decoder in mp3float flac aac alac vorbis opus wmav2 wmapro pcm_s16le pcm_s24le; do \
      ./ffmpeg -hide_banner -decoders 2>/dev/null | grep -qE "^ A[.A-Z]* +${decoder} " \
        || { echo "the ${decoder} decoder is not in this build"; exit 1; }; \
    done; \
    for encoder in aac flac libmp3lame libopus; do \
      ./ffmpeg -hide_banner -encoders 2>/dev/null | grep -qE "^ A[.A-Z]* +${encoder} " \
        || { echo "the ${encoder} encoder is not in this build"; exit 1; }; \
    done; \
    for muxer in rawvideo mp3 adts ogg opus flac ipod; do \
      ./ffmpeg -hide_banner -muxers 2>/dev/null | grep -qE " ${muxer} " \
        || { echo "the ${muxer} muxer is not in this build"; exit 1; }; \
    done; \
    for protocol in http https; do \
      ./ffmpeg -hide_banner -protocols 2>/dev/null | sed -n '/^Input:/,/^Output:/p' | grep -qx " *${protocol}" \
        || { echo "the ${protocol} protocol is not in this build"; exit 1; }; \
    done; \
    for demuxer in "mov,mp4" "matroska,webm" "avi" "mpegts" "mp3" "flac" "ogg" "wav" "aac"; do \
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

# And the same for audio: a two-second stereo FLAC at 44.1 kHz, turned into each
# thing a client can ask for. A list of encoders can be complete while the one a
# phone asks for still cannot be reached from the file it asked about.
#
# The mp3 is also sought: starting a second in has to give about half the bytes,
# because -ss before -i is how a transcoded track is sought at all (the
# transcodeOffset extension), and a seek that silently started from zero would
# be a player jumping back to the beginning every time the bar is dragged.
COPY testdata/tiny.flac /src/tiny.flac
RUN set -eu; \
    size() { ./ffmpeg -hide_banner -v error "$@" - | wc -c; }; \
    whole="$(size -i tiny.flac -c:a libmp3lame -b:a 128k -f mp3)"; \
    half="$(size -ss 1 -i tiny.flac -c:a libmp3lame -b:a 128k -f mp3)"; \
    [ "$whole" -gt 30000 ] || { echo "the mp3 is $whole bytes, want two seconds at 128k"; exit 1; }; \
    [ "$half" -lt $((whole * 3 / 4)) ] || { echo "seeking a second in gave $half of $whole bytes"; exit 1; }; \
    for target in libopus:ogg aac:adts flac:flac; do \
      encoder="${target%:*}"; muxer="${target#*:}"; \
      bytes="$(size -i tiny.flac -c:a "$encoder" -f "$muxer")"; \
      [ "$bytes" -gt 1000 ] || { echo "$encoder in $muxer gave $bytes bytes"; exit 1; }; \
    done

# scratch, so the server image copies a file and inherits nothing.
FROM scratch
COPY --from=build /src/ffmpeg /ffmpeg
