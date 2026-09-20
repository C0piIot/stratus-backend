# syntax=docker/dockerfile:1

# litmus, the WebDAV protocol compliance suite, built because nobody ships it.
#
# It is the canary #3 asked for and #173 filed: our own tests check that the
# server does what we think WebDAV is, and litmus checks what WebDAV actually
# is. The difference is the only thing that catches an assumption -- a
# SQLITE_BUSY under two writers went unseen by everything here and showed up in
# its first run.
#
# There is no official image, no release on any forge and no distribution
# package worth pinning: the last release is from 2011, at webdav.org, as a
# tarball. So it is built here the way ffmpeg is, with the source pinned by
# digest rather than by a name somebody could move.
ARG DEBIAN_VERSION=trixie-slim
ARG LITMUS_VERSION=0.13

FROM debian:${DEBIAN_VERSION} AS build
ARG LITMUS_VERSION

# Debian and not Alpine, unlike the other two images here, and the reason is
# worth a line: litmus carries its own getopt for systems that have none, and
# on musl that declaration collides with the one in unistd.h. This never ships
# -- it is a test tool that runs beside the container and not inside it -- so
# glibc costs nothing that matters.
# hadolint ignore=DL3008
RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential ca-certificates curl libexpat1-dev \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src
# The digest is the pin. A tarball on a 2011 web page is not a release anybody
# is keeping immutable for us.
ARG LITMUS_SHA256=09d615958121706444db67e09c40df5f753ccf1fa14846fdeb439298aa9ac3ff
RUN curl -fsSLo litmus.tar.gz "http://www.webdav.org/neon/litmus/litmus-${LITMUS_VERSION}.tar.gz" \
    && echo "${LITMUS_SHA256}  litmus.tar.gz" | sha256sum -c - \
    && tar xzf litmus.tar.gz --strip-components=1 \
    && rm litmus.tar.gz

# Written before C had rules about declaring things, and built by a compiler
# that has them. Without these the 2011 autoconf tests fail in ways that read
# like a missing library -- "could not find library containing socket" is
# really "this test program does not compile any more".
RUN CFLAGS="-Wno-implicit-function-declaration -Wno-implicit-int" \
    ./configure --with-expat \
    && make

# The suites are separate programs, which is litmus's own design: each takes a
# URL and optional credentials.
FROM debian:${DEBIAN_VERSION}
# hadolint ignore=DL3008
RUN apt-get update && apt-get install -y --no-install-recommends libexpat1 \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /src/basic /src/copymove /src/http /src/locks /src/props /usr/local/bin/

# The suites read a fixture out of htdocs/ in the working directory, so the
# working directory is part of the image rather than something a caller has to
# know to arrange.
COPY --from=build /src/htdocs /litmus/htdocs
WORKDIR /litmus

# No entrypoint: scripts/litmus.sh runs the suites one at a time so it can hold
# each to the number of tests it is expected to pass.
CMD ["/bin/sh"]
