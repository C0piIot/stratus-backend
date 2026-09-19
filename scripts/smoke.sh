#!/usr/bin/env bash
#
# Smoke tests for the built container image.
#
# These assert deployment properties that unit tests cannot reach: that the
# binary is static, that the image has no shell, that it runs non-root under a
# read-only root filesystem, and above all that a data directory it cannot write
# to fails at startup rather than at the first upload.
#
# Usage: scripts/smoke.sh   (or: make smoke)

set -euo pipefail

IMAGE="${IMAGE:-stratus-backend}"
TAG="${TAG:-smoke}"
REF="$IMAGE:$TAG"

# COVER=1 builds a second image from the same Dockerfile with `go build -cover`
# and points the runtime assertions at that one, writing counters into COVERDIR.
# Everything claimed about the artefact itself -- its size, its layer count, its
# lack of a shell -- keeps being measured against $REF, because an instrumented
# binary is not what anybody runs and a suite that says otherwise is measuring
# the wrong thing. See `make smoke-cover`.
COVER="${COVER:-}"
COVER_REF="$IMAGE:$TAG-cover"
COVERDIR="${COVERDIR:-}"
RUN_REF="$REF"
cover_args=()
if [ -n "$COVER" ]; then
  [ -n "$COVERDIR" ] || { printf 'COVER=1 needs COVERDIR set to a writable directory\n' >&2; exit 2; }
  cover_args=(-e GOCOVERDIR=/cover -v "$COVERDIR:/cover")
fi

# Budget on the binary rather than the image: with the containerd image store
# (Docker 24+ default) `docker image inspect .Size` reports the COMPRESSED size
# while `docker image ls` reports the uncompressed one, so a byte budget on the
# image means different things on different hosts. The binary size is what we
# actually control, and it is what grows if symbols stop being stripped or
# something gets vendored in.
# Raised from 12 MB when the metadata seam landed: modernc.org/sqlite is a full
# SQLite transpiled to Go and pgx is not small, and between them the binary went
# from 8.3 MB to 15.9 MB. The budget exists to catch growth nobody decided on,
# so it moves when a decision fills it -- and only then.
# Raised again from 20 MB with the web UI, which took it from 16.9 MB to 21.0.
# Measured rather than assumed: 0.3 MB is Bootstrap embedded instead of fetched
# from a CDN, and the rest is html/template, which costs about 3 MB on its own
# in a net/http program and a little more here. That is the price of the one
# decision this budget exists to make visible -- a server-rendered UI from the
# standard library -- and the remaining increments of it are templates, not
# packages.
BIN_SIZE_FAIL=$((25 * 1024 * 1024))
# ffprobe and ffmpeg together, which are trimmed builds of our own: 1.8 MB and
# 4.1 MB today against the 128 MB one general-purpose static FFmpeg costs.
TOOLS_SIZE_FAIL=$((10 * 1024 * 1024))

BASE_IMAGE="gcr.io/distroless/static:nonroot"

pass=0
fail=0
tmpdirs=()
containers=()
volumes=()

green() { printf '\033[32m%s\033[0m' "$1"; }
red()   { printf '\033[31m%s\033[0m' "$1"; }

ok()   { pass=$((pass + 1)); printf '  %s %s\n' "$(green ✓)" "$1"; }
bad()  { fail=$((fail + 1)); printf '  %s %s\n' "$(red ✗)" "$1"; [ $# -gt 1 ] && printf '      %s\n' "$2"; return 0; }

section() { printf '\n\033[1m%s\033[0m\n' "$1"; }

# Coverage counters are written when the process exits, and `docker rm -f` is a
# SIGKILL, which takes them with it. Under COVER every container is asked to
# stop first -- app.Run drains on SIGTERM and returns, which is an exit Go
# flushes.
stop_container() {
  # || true, not &&: the failure cases exit on their own under --rm, so by the
  # time this runs there is often nothing left to stop, and errexit would take
  # the suite down with it.
  if [ -n "$COVER" ]; then docker stop -t 10 "$1" >/dev/null 2>&1 || true; fi
  docker rm -f "$1" >/dev/null 2>&1 || true
}

cleanup() {
  for c in ${containers[@]+"${containers[@]}"}; do stop_container "$c"; done
  for v in ${volumes[@]+"${volumes[@]}"};    do docker volume rm -f "$v" >/dev/null 2>&1 || true; done
  for d in ${tmpdirs[@]+"${tmpdirs[@]}"};    do rm -rf "$d" 2>/dev/null || true; done
}
trap cleanup EXIT

mktmp() { local d; d="$(mktemp -d)"; tmpdirs+=("$d"); printf '%s' "$d"; }

# Start detached with a host-assigned port and wait until /healthz answers.
#
# Note on healthchecks: `docker run --health-cmd` wraps the string in /bin/sh,
# which distroless does not have, so a CLI healthcheck can never pass here. The
# exec-form healthcheck in compose.yaml is the one that works, and it gets its
# own test below. From the CLI we poll the endpoint directly instead.
run_detached() {
  local name="$1"; shift
  containers+=("$name")
  docker run -d --name "$name" -p "127.0.0.1::8080" \
    ${cover_args[@]+"${cover_args[@]}"} "$@" "$RUN_REF" >/dev/null
}

wait_serving() {
  local name="$1" deadline=$((SECONDS + ${2:-25})) hostport
  hostport="$(docker port "$name" 8080/tcp 2>/dev/null | head -1)" || return 1
  [ -n "$hostport" ] || return 1
  while [ $SECONDS -lt $deadline ]; do
    if curl -fsS "http://$hostport/healthz" >/dev/null 2>&1; then
      return 0
    fi
    [ "$(docker inspect -f '{{.State.Running}}' "$name" 2>/dev/null)" = "true" ] || return 1
    sleep 0.5
  done
  return 1
}

# ---------------------------------------------------------------------------
section "Build"
# ---------------------------------------------------------------------------
VERSION="$(git describe --tags --match "v*" --always --dirty 2>/dev/null || echo dev)"
BUILD_DATE="$(TZ=UTC git show -s --date=format-local:%Y-%m-%dT%H:%M:%SZ --format=%cd 2>/dev/null || echo unknown)"
if docker build --build-arg "VERSION=$VERSION" --build-arg "BUILD_DATE=$BUILD_DATE" \
     -t "$REF" . >/dev/null 2>&1; then
  ok "image builds ($REF)"
else
  bad "image builds" "docker build failed; rerun without -q to see why"
  echo; echo "aborting: nothing to test"; exit 1
fi

# Not an assertion: this image is a measuring instrument, not something the
# project ships, so nothing here claims anything about it.
if [ -n "$COVER" ]; then
  docker build --build-arg "VERSION=$VERSION" --build-arg "BUILD_DATE=$BUILD_DATE" \
    --build-arg COVER=1 -t "$COVER_REF" . >/dev/null 2>&1 ||
    { echo; echo "aborting: the instrumented image did not build"; exit 1; }
  RUN_REF="$COVER_REF"
  printf '  runtime assertions run against %s, counters into %s\n' "$COVER_REF" "$COVERDIR"
fi

# ---------------------------------------------------------------------------
section "Image properties"
# ---------------------------------------------------------------------------
# The image must add exactly one layer to the base: the binary and nothing else.
# A second layer means a stray COPY or a leaked source tree.
docker pull -q "$BASE_IMAGE" >/dev/null 2>&1 || true
base_layers="$(docker image inspect -f '{{len .RootFS.Layers}}' "$BASE_IMAGE" 2>/dev/null || echo 0)"
img_layers="$(docker image inspect -f '{{len .RootFS.Layers}}' "$REF")"
# Three layers now: the binary, ffprobe and ffmpeg. The number matters less than
# the fact that it is counted -- a base swapped for something with a package
# manager in it would show up here first.
if [ "$base_layers" -gt 0 ] && [ "$((img_layers - base_layers))" -eq 3 ]; then
  ok "adds exactly three layers over the base ($base_layers -> $img_layers)"
else
  bad "adds exactly three layers over the base" "base=$base_layers image=$img_layers"
fi

# Both media tools are requirements, so an absence has to fail here rather than
# at the first video somebody uploads or the first thumbnail somebody opens.
for tool in ffprobe ffmpeg; do
  if docker run --rm --entrypoint "/usr/local/bin/$tool" "$REF" -version >/dev/null 2>&1; then
    ok "$tool runs inside the image"
  else
    bad "$tool runs inside the image" "it is missing or not executable"
  fi
done

user="$(docker image inspect -f '{{.Config.User}}' "$REF")"
if [ "$user" = "65532:65532" ]; then
  ok "image config user is $user (numeric, no passwd lookup needed)"
else
  bad "image config user" "got '$user', want 65532:65532"
fi

# No shell, no coreutils: nothing to pivot to if the process is compromised.
shell_found=""
for exe in /bin/sh /bin/bash /bin/busybox /bin/cat /usr/bin/env; do
  if docker run --rm --entrypoint "$exe" "$REF" -c true >/dev/null 2>&1; then
    shell_found="$shell_found $exe"
  fi
done
if [ -z "$shell_found" ]; then
  ok "no shell or coreutils in the image"
else
  bad "no shell in the image" "found:$shell_found"
fi

# ---------------------------------------------------------------------------
section "Binary"
# ---------------------------------------------------------------------------
bindir="$(mktmp)"
cid="$(docker create "$REF")"
containers+=("$cid")
docker cp "$cid:/usr/local/bin/stratus" "$bindir/stratus" >/dev/null

bin_size="$(stat -c '%s' "$bindir/stratus")"
if [ "$bin_size" -le "$BIN_SIZE_FAIL" ]; then
  ok "binary is $((bin_size / 1024 / 1024)) MB, within budget"
else
  bad "binary within budget" "$((bin_size / 1024 / 1024)) MB exceeds $((BIN_SIZE_FAIL / 1024 / 1024)) MB"
fi

# The media tools, measured rather than trusted. This is the number the README
# quotes, and the field it quotes has drifted before: a general-purpose static
# FFmpeg is 128 MB, so a build that silently stopped being trimmed -- an
# --enable-everything, a library autodetected in the build image -- would show
# up here as a size and not as a mystery.
docker cp "$cid:/usr/local/bin/ffprobe" "$bindir/ffprobe" >/dev/null
docker cp "$cid:/usr/local/bin/ffmpeg" "$bindir/ffmpeg" >/dev/null
tools_size=$(( $(stat -c '%s' "$bindir/ffprobe") + $(stat -c '%s' "$bindir/ffmpeg") ))
if [ "$tools_size" -le "$TOOLS_SIZE_FAIL" ]; then
  ok "ffprobe and ffmpeg are $((tools_size / 1024 / 1024)) MB together, within budget"
else
  bad "media tools within budget" \
    "$((tools_size / 1024 / 1024)) MB exceeds $((TOOLS_SIZE_FAIL / 1024 / 1024)) MB"
fi

buildinfo="$(docker run --rm -v "$bindir:/b:ro" "golang:1.27.0-alpine3.24" go version -m /b/stratus 2>/dev/null || true)"
if grep -q 'CGO_ENABLED=0' <<<"$buildinfo"; then
  ok "binary built with CGO_ENABLED=0"
else
  bad "binary built with CGO_ENABLED=0" "build settings did not report it"
fi
if grep -q '\-trimpath' <<<"$buildinfo"; then
  ok "binary built with -trimpath"
else
  bad "binary built with -trimpath" "not in build settings"
fi

# The dependency budget (#5). Measured on the artifact rather than on go.mod,
# which requires two modules that never reach it: what matters is the code that
# actually runs on somebody's machine. scripts/deps.sh explains the list.
if deps_out="$(printf '%s\n' "$buildinfo" | ./scripts/deps.sh 2>&1)"; then
  ok "every module in the binary is one deps.allow names ($deps_out)"
else
  bad "every module in the binary is one deps.allow names" "$(tr '\n' ' ' <<<"$deps_out")"
fi

# A static binary has no PT_INTERP segment. readelf comes with the Debian image.
if docker run --rm -v "$bindir:/b:ro" "golang:1.27.0-trixie" \
     sh -c 'readelf -l /b/stratus 2>/dev/null | grep -q INTERP'; then
  bad "binary is statically linked" "an INTERP segment is present, so it needs a dynamic loader"
else
  ok "binary is statically linked (no INTERP segment)"
fi

# ---------------------------------------------------------------------------
section "Flags"
# ---------------------------------------------------------------------------
out="$(docker run --rm ${cover_args[@]+"${cover_args[@]}"} "$RUN_REF" -version 2>&1 || true)"
if [ "$out" = "stratus $VERSION (built $BUILD_DATE)" ]; then
  ok "-version reports the injected version and build date"
else
  bad "-version reports the injected version" "got '$out', want 'stratus $VERSION'"
fi

# The healthcheck must fail when nothing is listening, otherwise it can never
# mark a wedged container unhealthy.
if docker run --rm ${cover_args[@]+"${cover_args[@]}"} "$RUN_REF" -healthcheck >/dev/null 2>&1; then
  bad "-healthcheck fails with nothing listening" "it succeeded, so the probe proves nothing"
else
  ok "-healthcheck fails with nothing listening"
fi

# ---------------------------------------------------------------------------
section "Startup: happy path"
# ---------------------------------------------------------------------------
datadir="$(mktmp)"
name="stratus-smoke-ok"
run_detached "$name" -u "$(id -u):$(id -g)" -v "$datadir:/data"
if wait_serving "$name"; then
  ok "container starts and serves /healthz"
else
  bad "container starts and serves /healthz" "$(docker logs "$name" 2>&1 | tail -3)"
fi

logs="$(docker logs "$name" 2>&1)"
logged_uid="$(sed -n 's/.*"uid":\([0-9]*\).*/\1/p' <<<"$logs" | head -1)"
if [ -n "$logged_uid" ] && [ "$logged_uid" != "0" ]; then
  ok "runs as non-root at runtime (uid $logged_uid)"
else
  bad "runs as non-root at runtime" "startup log reported uid '$logged_uid'"
fi

if docker exec "$name" /usr/local/bin/stratus -healthcheck >/dev/null 2>&1; then
  ok "healthcheck succeeds via docker exec (no shell needed)"
else
  bad "healthcheck succeeds via docker exec" "$(docker logs "$name" 2>&1 | tail -3)"
fi

# Both write probes are startup artefacts. The data directory now legitimately
# holds the blob store, so the assertion is that nothing *else* survived: no
# probe file beside it, and no probe object inside it.
# The data directory legitimately holds the blob store, the database and the
# indexer's spool directory. The assertion is that nothing *else* survived: no
# write probe beside them, and nothing left inside them.
leftovers="$(find "$datadir" -mindepth 1 \
  -not -path "$datadir/blobs" -not -path "$datadir/blobs/.tmp" \
  -not -path "$datadir/blobs/.uploads" \
  -not -path "$datadir/.index" \
  -not -name 'stratus.db*' | tr '\n' ' ')"
if [ -z "$leftovers" ]; then
  ok "neither write probe is left behind"
else
  bad "neither write probe is left behind" "found: $leftovers"
fi

# This container has no credentials configured, so there is nobody to sign in
# as and the UI is not mounted at all -- the same rule as WebDAV and Subsonic.
hostport="$(docker port "$name" 8080/tcp | head -1)"
code="$(curl -s -o /dev/null -w '%{http_code}' "http://$hostport/login")"
if [ "$code" = "404" ]; then
  ok "no credentials, no web UI"
else
  bad "no credentials, no web UI" "GET /login = $code"
fi
stop_container "$name"

# ---------------------------------------------------------------------------
section "Startup: hardened runtime"
# ---------------------------------------------------------------------------
# Mirrors compose.yaml. Regression guard for anything that later writes outside
# /data -- SQLite wanting a spill directory will trip this first.
datadir="$(mktmp)"
name="stratus-smoke-hardened"
run_detached "$name" -u "$(id -u):$(id -g)" -v "$datadir:/data" \
  --read-only --tmpfs /tmp --cap-drop ALL --security-opt no-new-privileges
if wait_serving "$name"; then
  ok "serves with read-only rootfs, all caps dropped, no-new-privileges"
else
  bad "serves under hardening flags" "$(docker logs "$name" 2>&1 | tail -3)"
fi
stop_container "$name"

# ---------------------------------------------------------------------------
section "Data directory failure matrix"
# ---------------------------------------------------------------------------
# This is the regression suite for a bug that shipped: the container started and
# reported healthy while being unable to write a single byte. Each case must
# fail fast, non-zero, with a message an operator can act on.

# A timeout is essential here, not defensive padding: if the validation
# regresses, the server starts and this docker run never returns. Without the
# timeout the suite hangs instead of failing, which in CI means a 20-minute
# job timeout rather than a legible red.
STARTUP_DEADLINE=10

expect_startup_failure() {
  local what="$1" want="$2"; shift 2
  local out rc start elapsed name
  name="stratus-smoke-fail-$$-$RANDOM"
  containers+=("$name")
  start=$SECONDS
  set +e
  out="$(timeout "$STARTUP_DEADLINE" docker run --rm --name "$name" \
    ${cover_args[@]+"${cover_args[@]}"} "$@" "$RUN_REF" 2>&1)"
  rc=$?
  set -e
  elapsed=$((SECONDS - start))
  stop_container "$name"

  if [ "$rc" -eq 124 ]; then
    bad "$what" "still running after ${STARTUP_DEADLINE}s; it should refuse to start"
    return
  fi
  if [ "$rc" -eq 0 ]; then
    bad "$what" "exited 0; it should refuse to start"
    return
  fi
  if ! grep -qi "$want" <<<"$out"; then
    bad "$what" "exit $rc but message lacks '$want': $(tail -1 <<<"$out")"
    return
  fi
  if [ "$elapsed" -ge "$STARTUP_DEADLINE" ]; then
    bad "$what" "took ${elapsed}s; startup validation must fail fast"
    return
  fi
  ok "$what (exit $rc in ${elapsed}s)"
}

# The original bug: Docker does not carry image ownership into a fresh named
# volume, so /data arrives root-owned and the nonroot user cannot write.
vol="stratus-smoke-vol-$$"
volumes+=("$vol")
docker volume create "$vol" >/dev/null
expect_startup_failure "fresh named volume as nonroot refuses to start" "not writable" \
  -v "$vol:/data"

foreign="$(mktmp)"
docker run --rm -v "$foreign:/d" alpine:3.24 sh -c 'chown 0:0 /d && chmod 755 /d' >/dev/null 2>&1
expect_startup_failure "bind mount owned by another uid refuses to start" "not writable" \
  -u "$(id -u):$(id -g)" -v "$foreign:/data"

rodir="$(mktmp)"
expect_startup_failure "read-only bind mount refuses to start" "not writable" \
  -u "$(id -u):$(id -g)" -v "$rodir:/data:ro"

filedir="$(mktmp)"
: > "$filedir/afile"
expect_startup_failure "a regular file as the data dir refuses to start" "data dir" \
  -u "$(id -u):$(id -g)" -v "$filedir/afile:/data"

# A probe file left by a previous crash must not wedge startup.
staledir="$(mktmp)"
printf 'left over\n' > "$staledir/.stratus-write-probe"
name="stratus-smoke-stale"
run_detached "$name" -u "$(id -u):$(id -g)" -v "$staledir:/data"
if wait_serving "$name"; then
  ok "a stale write probe does not block startup"
else
  bad "a stale write probe does not block startup" "$(docker logs "$name" 2>&1 | tail -3)"
fi
stop_container "$name"

# ---------------------------------------------------------------------------
section "Protocol surfaces"
# ---------------------------------------------------------------------------
# The surfaces that need credentials, in one container because they share them.
# Unit tests drive the handlers; this drives the shipped image with a real
# client, over a real port.

davdir="$(mktmp)"
davname="stratus-smoke-dav"
davuser="edu"
davpass="an example password for the smoke tests"
run_detached "$davname" -u "$(id -u):$(id -g)" -v "$davdir:/data" \
  -e STRATUS_USERNAME="$davuser" -e STRATUS_PASSWORD="$davpass" \
  -e STRATUS_INDEX_INTERVAL=10m

if wait_serving "$davname"; then
  davhost="$(docker port "$davname" 8080/tcp | head -1)"

  # Readiness against the real backends: the disk store and the SQLite file the
  # container actually opened. A unit test can only assert this against ones it
  # built itself.
  ready="$(curl -s "http://$davhost/readyz")"
  if [ "$ready" = "$(printf 'database: ok\nstorage: ok')" ]; then
    ok "/readyz reports both dependencies"
  else
    bad "/readyz reports both dependencies" "got $(printf '%s' "$ready" | tr '\n' ' ')"
  fi

  code="$(curl -s -o /dev/null -w '%{http_code}' -X PUT --data-binary 'smoke' "http://$davhost/dav/notes.txt")"
  if [ "$code" = "401" ]; then
    ok "an unauthenticated PUT is refused"
  else
    bad "an unauthenticated PUT is refused" "got $code"
  fi

  code="$(curl -s -o /dev/null -w '%{http_code}' -u "$davuser:$davpass" \
    -X PUT --data-binary 'smoke' "http://$davhost/dav/notes.txt")"
  if [ "$code" = "201" ]; then
    ok "PUT stores a file"
  else
    bad "PUT stores a file" "got $code"
  fi

  body="$(curl -fsS -u "$davuser:$davpass" "http://$davhost/dav/notes.txt" 2>/dev/null || true)"
  if [ "$body" = "smoke" ]; then
    ok "GET reads it back through storage and the database"
  else
    bad "GET reads it back" "got '$body'"
  fi

  # The blob is in the store under an opaque name, and the tree is in the
  # database: that split is the whole architecture, so it is worth asserting
  # from outside the process.
  if [ -n "$(find "$davdir/blobs" -type f -not -path '*/.tmp/*' 2>/dev/null)" ] && [ -f "$davdir/stratus.db" ]; then
    ok "the bytes are a blob and the name is a row"
  else
    bad "the bytes are a blob and the name is a row" "$(find "$davdir" -maxdepth 2 | tr '\n' ' ')"
  fi

  # Finder mounts read-only unless the server says class 2, so the header is
  # asserted rather than assumed.
  dav_header="$(curl -s -o /dev/null -D - -u "$davuser:$davpass" -X OPTIONS "http://$davhost/dav/" | grep -i '^dav:' | tr -d '\r')"
  case "$dav_header" in
    *2*) ok "OPTIONS advertises locking" ;;
    *)   bad "OPTIONS advertises locking" "got '$dav_header'" ;;
  esac

  lock_status="$(curl -s -o /dev/null -w '%{http_code}' -u "$davuser:$davpass" -X LOCK \
    -H 'Content-Type: application/xml' \
    --data '<?xml version="1.0"?><D:lockinfo xmlns:D="DAV:"><D:lockscope><D:exclusive/></D:lockscope><D:locktype><D:write/></D:locktype></D:lockinfo>' \
    "http://$davhost/dav/notes.txt")"
  if [ "$lock_status" = "200" ]; then
    ok "LOCK answers with a token"
  else
    bad "LOCK answers with a token" "got $lock_status"
  fi

  code="$(curl -s -o /dev/null -w '%{http_code}' -u "$davuser:$davpass" \
    -H 'Depth: 1' -X PROPFIND "http://$davhost/dav/")"
  if [ "$code" = "207" ]; then
    ok "PROPFIND answers a multistatus"
  else
    bad "PROPFIND answers a multistatus" "got $code"
  fi

  # And the whole tree at once is refused, in the words RFC 4918 14.5 has for
  # it: a client that meant Depth: infinity has to learn to walk a level at a
  # time, which it cannot do from a timeout or a dead server. Asserted from
  # outside because it is a header this project reads for itself -- the library
  # would have answered 207 and built the answer in memory first.
  finite="$(mktmp)/body"
  refused="$(curl -s -u "$davuser:$davpass" -X PROPFIND "http://$davhost/dav/" \
    -o "$finite" -w '%{http_code}')"
  if [ "$refused" = "403" ] && grep -q 'propfind-finite-depth' "$finite"; then
    ok "a PROPFIND for the whole tree is refused with a precondition"
  else
    bad "a PROPFIND for the whole tree is refused with a precondition" \
      "got $refused: $(head -c 200 "$finite")"
  fi

  # The body this time, because #126 was a 207 with the wrong thing in it: a
  # Depth 1 listing has to carry the collection it was asked about, not only its
  # members. Matched on the element content rather than a namespace prefix the
  # library is free to change, and /dav/ is the one href in the document that is
  # exactly that -- every member is /dav/something.
  propfind="$(curl -fsS -u "$davuser:$davpass" -H 'Depth: 1' \
    -X PROPFIND "http://$davhost/dav/" 2>/dev/null || true)"
  case "$propfind" in
    *'>/dav/<'*) ok "a PROPFIND listing includes the collection itself" ;;
    *)           bad "a PROPFIND listing includes the collection itself" "no self entry in the multistatus" ;;
  esac

  # A resumable upload, cut in half, against the shipped image. The point is not
  # that two PATCHes work -- the unit tests cover that -- but that an upload
  # survives being interrupted in the one place it has to: between requests,
  # with nothing held in memory and the offset read back from the server.
  tus_meta="filename $(printf '%s' 'resumed.txt' | base64 -w0),filetype $(printf '%s' 'text/plain' | base64 -w0)"
  tus_location="$(curl -fsS -D - -o /dev/null -u "$davuser:$davpass" -X POST \
    -H 'Tus-Resumable: 1.0.0' -H 'Upload-Length: 10' -H "Upload-Metadata: $tus_meta" \
    "http://$davhost/tus/" 2>/dev/null | tr -d '\r' | sed -n 's/^[Ll]ocation: //p')"
  if [ -n "$tus_location" ]; then
    ok "a tus upload can be created"
  else
    bad "a tus upload can be created" "no Location header came back"
  fi

  curl -fsS -o /dev/null -u "$davuser:$davpass" -X PATCH \
    -H 'Tus-Resumable: 1.0.0' -H 'Content-Type: application/offset+octet-stream' \
    -H 'Upload-Offset: 0' --data-binary 'half ' \
    "http://$davhost$tus_location" >/dev/null 2>&1 || true

  # What a client asks after anything at all went wrong, and the answer that
  # makes the whole protocol worth having.
  tus_offset="$(curl -fsS -D - -o /dev/null -u "$davuser:$davpass" -X HEAD \
    -H 'Tus-Resumable: 1.0.0' "http://$davhost$tus_location" 2>/dev/null |
    tr -d '\r' | sed -n 's/^[Uu]pload-[Oo]ffset: //p')"
  if [ "$tus_offset" = "5" ]; then
    ok "a tus upload reports where it got to"
  else
    bad "a tus upload reports where it got to" "Upload-Offset is '$tus_offset', want 5"
  fi

  curl -fsS -o /dev/null -u "$davuser:$davpass" -X PATCH \
    -H 'Tus-Resumable: 1.0.0' -H 'Content-Type: application/offset+octet-stream' \
    -H "Upload-Offset: $tus_offset" --data-binary 'again' \
    "http://$davhost$tus_location" >/dev/null 2>&1 || true

  # And the file is a file, over the protocol that did not upload it.
  resumed="$(curl -fsS -u "$davuser:$davpass" "http://$davhost/dav/resumed.txt" 2>/dev/null || true)"
  if [ "$resumed" = "half again" ]; then
    ok "a resumed upload lands as a file WebDAV can read"
  else
    bad "a resumed upload lands as a file WebDAV can read" "got '$resumed'"
  fi

  # The other protocol surface, in the image that has to serve it. Token auth
  # rather than the password, because it is the scheme every current client
  # uses and md5(password + salt) is the whole reason the password is held as
  # configured rather than hashed.
  salt="smoke"
  token="$(printf '%s' "$davpass$salt" | md5sum | cut -d' ' -f1)"
  body="$(curl -fsS "http://$davhost/rest/ping.view?c=smoke&u=$davuser&t=$token&s=$salt" 2>/dev/null || true)"
  case "$body" in
    *'status="ok"'*) ok "OpenSubsonic answers a token login" ;;
    *)               bad "OpenSubsonic answers a token login" "got '$body'" ;;
  esac

  # An error is an HTTP 200 with a code inside it. That is the protocol's own
  # design and not a bug to fix: a client handed a transport error cannot read
  # the reason for it.
  refused="$(curl -s -w '|%{http_code}' "http://$davhost/rest/ping.view?c=smoke&u=$davuser&p=wrong")"
  case "$refused" in
    *'code="40"'*'|200') ok "an OpenSubsonic error travels inside a 200" ;;
    *)                   bad "an OpenSubsonic error travels inside a 200" "got '$refused'" ;;
  esac

  # Browsing and playing, through the shipped image, over a real port. The
  # upload above is a text file with a .txt name, so this puts a track in with
  # an extension the indexer recognises and waits for it to be read: what is
  # asserted is the whole loop -- upload, extract, browse, stream.
  curl -fsS -u "$davuser:$davpass" -X PUT --data-binary @- \
    "http://$davhost/dav/track.mp3" >/dev/null 2>&1 <<'TRACK'
not really an mp3, and that is the point: the row is what browsing reads
TRACK
  browsed=""
  for _ in $(seq 1 50); do
    body="$(curl -fsS "http://$davhost/rest/getIndexes.view?c=smoke&u=$davuser&t=$token&s=$salt" 2>/dev/null || true)"
    case "$body" in
      *'track.mp3'*) browsed=yes; break ;;
    esac
    sleep 0.2
  done
  if [ -n "$browsed" ]; then
    ok "OpenSubsonic browses what was uploaded over WebDAV"
  else
    bad "OpenSubsonic browses what was uploaded over WebDAV" "got '$body'"
  fi

  # The id comes out of the listing rather than being built here: a client only
  # ever sends back an id the server gave it, and so does this.
  song_id="$(printf '%s' "$body" | tr '<' '\n' | grep 'title="track.mp3"' | sed -n 's/.*id="\([^"]*\)".*/\1/p' | head -1)"
  streamed="$(curl -fsS "http://$davhost/rest/stream.view?c=smoke&u=$davuser&t=$token&s=$salt&id=$song_id" 2>/dev/null || true)"
  case "$streamed" in
    *'not really an mp3'*) ok "OpenSubsonic streams the stored bytes" ;;
    *)                     bad "OpenSubsonic streams the stored bytes" "id '$song_id' gave '$streamed'" ;;
  esac

  # And found by searching, which is the endpoint a client's search box is. The
  # empty query is the one the specification requires: it is how a client
  # downloads a library to browse with no network.
  #
  # Waited for like the listing above rather than asked once: what is being
  # asserted is that a search finds an indexed file, and waiting for the
  # indexer is part of getting one, not part of what is under test.
  searched=""
  for _ in $(seq 1 50); do
    body="$(curl -fsS "http://$davhost/rest/search3.view?c=smoke&u=$davuser&t=$token&s=$salt&query=" 2>/dev/null || true)"
    case "$body" in
      *'track.mp3'*) searched=yes; break ;;
    esac
    sleep 0.2
  done
  if [ -n "$searched" ]; then
    ok "OpenSubsonic finds it with an empty search"
  else
    bad "OpenSubsonic finds it with an empty search" "got '$body'"
  fi

  # Cover art, which is a thumbnail made on demand and kept in the blob store.
  # A real 400x400 JPEG uploaded over WebDAV like anything else, so what is
  # asserted is the whole path from outside the container: a picture found in a
  # folder by name, decoded, reduced and served.
  curl -fsS -u "$davuser:$davpass" -X PUT --data-binary "@scripts/testdata/cover.jpg" \
    "http://$davhost/dav/cover.jpg" >/dev/null 2>&1
  coverfile="$(mktmp)/cover.jpg"
  code="$(curl -s -o "$coverfile" -w '%{http_code} %{content_type}' \
    "http://$davhost/rest/getCoverArt.view?c=smoke&u=$davuser&t=$token&s=$salt&id=d-&size=96")"
  # Smaller than the 2 KB that went in, because it was reduced to 96 pixels,
  # and still a JPEG: a passthrough of the original would be the same size.
  coversize="$(stat -c '%s' "$coverfile" 2>/dev/null || echo 0)"
  case "$code" in
    "200 image/jpeg"*)
      if [ "$coversize" -gt 100 ] && [ "$coversize" -lt 2102 ]; then
        ok "getCoverArt reduces a picture found beside the music ($coversize bytes)"
      else
        bad "getCoverArt reduces a picture found beside the music" "$coversize bytes"
      fi
      ;;
    *) bad "getCoverArt reduces a picture found beside the music" "got '$code'" ;;
  esac

  # The web UI, driven the way a browser drives it: a cookie jar, a form post
  # and a redirect. Its session is signed rather than stored, so what is asserted
  # here is the whole round trip through the shipped binary.
  jar="$(mktmp)/cookies"

  code="$(curl -s -o /dev/null -w '%{http_code} %{redirect_url}' "http://$davhost/files/")"
  case "$code" in
    "303 http://$davhost/login?next=%2Ffiles%2F") ok "the tree asks a browser to sign in" ;;
    *) bad "the tree asks a browser to sign in" "got '$code'" ;;
  esac

  body="$(curl -fsS "http://$davhost/login" 2>/dev/null || true)"
  case "$body" in
    *'name="password"'*) ok "the login form is served" ;;
    *)                   bad "the login form is served" "got '$(head -c 120 <<<"$body")'" ;;
  esac

  refused="$(curl -s -o /dev/null -w '%{http_code}' -c "$jar" \
    --data-urlencode "username=$davuser" --data-urlencode "password=not it" \
    "http://$davhost/login")"
  if [ "$refused" = "401" ] && ! grep -q stratus_session "$jar" 2>/dev/null; then
    ok "a wrong password gets no session"
  else
    bad "a wrong password gets no session" "status $refused"
  fi

  # -D -: the attributes are the assertion, and a cookie jar does not show them.
  headers="$(curl -s -D - -o /dev/null -c "$jar" \
    --data-urlencode "username=$davuser" --data-urlencode "password=$davpass" \
    "http://$davhost/login")"
  cookie="$(grep -i '^set-cookie: stratus_session=' <<<"$headers" | tr -d '\r')"
  case "$cookie" in
    # Secure first: this request arrived over plain HTTP, and a cookie the
    # browser would never send back is worse than no cookie at all.
    *Secure*) bad "signing in sets a session cookie" "marked Secure over plain HTTP: $cookie" ;;
    *HttpOnly*SameSite=Lax*) ok "signing in sets an HttpOnly, SameSite=Lax cookie" ;;
    *) bad "signing in sets a session cookie" "got '$cookie'" ;;
  esac

  # -L: the root is a signpost to the tree, so following it is the assertion
  # that both halves are wired. What comes back is the listing of files this
  # very script put in over WebDAV -- the same tree, reached two ways.
  body="$(curl -fsSL -b "$jar" "http://$davhost/" 2>/dev/null || true)"
  missing=""
  for f in notes.txt track.mp3 cover.jpg; do
    case "$body" in *">$f<"*) ;; *) missing="$missing $f" ;; esac
  done
  signed_in=""
  case "$body" in *">$davuser<"*) signed_in=yes ;; esac
  if [ -z "$missing" ] && [ -n "$signed_in" ]; then
    ok "the session opens the tree, and it lists what WebDAV uploaded"
  else
    bad "the session opens the tree" "missing:${missing:- nothing}, $(head -c 120 <<<"$body")"
  fi

  # Opening a file hands over the bytes, as an attachment: this origin serves
  # the UI, and somebody's upload is not the UI's to render inside it.
  headers="$(curl -s -D - -o /dev/null -b "$jar" "http://$davhost/files/notes.txt")"
  downloaded="$(curl -s -b "$jar" "http://$davhost/files/notes.txt")"
  case "$headers" in
    *[Cc]ontent-[Dd]isposition*attachment*)
      if [ "$downloaded" = "smoke" ]; then
        ok "opening a file downloads the stored bytes"
      else
        bad "opening a file downloads the stored bytes" "got '$downloaded'"
      fi ;;
    *) bad "opening a file downloads it as an attachment" "$(grep -i '^content-' <<<"$headers" | tr -d '\r' | tr '\n' ' ')" ;;
  esac

  # The picture the Subsonic case put in the tree, this time through the
  # browser's own door: the listing offers an image for it, and asking for that
  # image makes one. Same generator, a second consumer.
  listing="$(curl -fsS -b "$jar" "http://$davhost/files/" 2>/dev/null || true)"
  thumbfile="$(mktmp)/thumb.jpg"
  thumbcode="$(curl -s -o "$thumbfile" -w '%{http_code} %{content_type}' -b "$jar" \
    "http://$davhost/thumb/cover.jpg?size=96")"
  thumbsize="$(stat -c '%s' "$thumbfile" 2>/dev/null || echo 0)"
  case "$listing:$thumbcode" in
    *'/thumb/cover.jpg'*':200 image/jpeg'*)
      if [ "$thumbsize" -gt 100 ] && [ "$thumbsize" -lt 2102 ]; then
        ok "the browser listing offers a thumbnail and the server makes it ($thumbsize bytes)"
      else
        bad "the browser listing offers a thumbnail and the server makes it" "$thumbsize bytes"
      fi ;;
    *) bad "the browser listing offers a thumbnail and the server makes it" \
         "the page did not link one, or the request answered '$thumbcode'" ;;
  esac

  # And a file that cannot have one is not offered one, which is what keeps a
  # listing from being a wall of broken images.
  case "$listing" in
    *'/thumb/upload.txt'*) bad "a text file is offered no thumbnail" "the listing links one" ;;
    *)                     ok "a text file is offered no thumbnail" ;;
  esac

  # The two formats Go cannot decode, through the shipped image and the ffmpeg
  # in it: a photograph from a phone, and a frame out of a video. This is the
  # only place either binary is real -- the unit tests skip where there is none.
  #
  # The video is also what the indexer reads out of its own container over
  # ranges (#48), which the status assertions further down are about.
  curl -fsS -u "$davuser:$davpass" -X PUT --data-binary "@scripts/testdata/photo.heic" \
    "http://$davhost/dav/photo.heic" >/dev/null 2>&1
  curl -fsS -u "$davuser:$davpass" -X PUT --data-binary "@scripts/testdata/clip.mp4" \
    "http://$davhost/dav/clip.mp4" >/dev/null 2>&1

  # And a Matroska, which is read out of its own elements the way the MP4 is
  # read out of its boxes (#145). Its thumbnail is ffmpeg's either way.
  curl -fsS -u "$davuser:$davpass" -X PUT --data-binary "@scripts/testdata/film.mkv" \
    "http://$davhost/dav/film.mkv" >/dev/null 2>&1

  for subject in photo.heic clip.mp4 film.mkv; do
    file="$(mktmp)/thumb.jpg"
    code="$(curl -s -o "$file" -w '%{http_code} %{content_type}' -b "$jar" \
      "http://$davhost/thumb/$subject?size=96")"
    bytes="$(stat -c '%s' "$file" 2>/dev/null || echo 0)"
    # A JPEG, and one with a picture in it: a header alone is under a hundred
    # bytes and a 96-pixel thumbnail is a few thousand.
    case "$code" in
      "200 image/jpeg"*)
        if [ "$bytes" -gt 200 ] && [ "$bytes" -lt 100000 ]; then
          ok "ffmpeg makes a thumbnail of $subject ($bytes bytes)"
        else
          bad "ffmpeg makes a thumbnail of $subject" "$bytes bytes"
        fi ;;
      *) bad "ffmpeg makes a thumbnail of $subject" "the request answered '$code'" ;;
    esac
  done

  # And the listing offers them, which is the half of this a browser sees: the
  # answer is a property of the build, so wiring the path changed every row of
  # every folder at once.
  listing="$(curl -fsS -b "$jar" "http://$davhost/files/" 2>/dev/null || true)"
  case "$listing" in
    *'/thumb/photo.heic'*'/thumb/clip.mp4'*|*'/thumb/clip.mp4'*'/thumb/photo.heic'*)
      ok "the listing offers a picture for both" ;;
    *) bad "the listing offers a picture for both" "$(grep -o '/thumb/[a-z.]*' <<<"$listing" | tr '\n' ' ')" ;;
  esac

  # The listing is one page of a folder and a cursor to the next, which is what
  # keeps a hundred thousand photographs out of one document. The tree here is
  # far smaller than a page, so what is asserted is the cursor itself: the row
  # it names is behind us and the rest of the folder is still ahead.
  paged="$(curl -fsS -b "$jar" "http://$davhost/files/?after=f/cover.jpg" 2>/dev/null || true)"
  case "$paged" in
    *'>cover.jpg<'*) bad "a listing resumes after the cursor" "the row the cursor names came back again" ;;
    *'>notes.txt<'*) ok "a listing resumes after the cursor" ;;
    *)               bad "a listing resumes after the cursor" "$(head -c 120 <<<"$paged")" ;;
  esac
  code="$(curl -s -o /dev/null -w '%{http_code}' -b "$jar" "http://$davhost/files/?after=nonsense")"
  if [ "$code" = "400" ]; then
    ok "a cursor that is not one is refused"
  else
    bad "a cursor that is not one is refused" "got $code"
  fi

  # htmx extending that listing gets the rows and nothing else: the same URL,
  # one request header apart, and no second endpoint answering in JSON.
  fragment="$(curl -fsS -b "$jar" -H 'HX-Request: true' "http://$davhost/files/" 2>/dev/null || true)"
  case "$fragment" in
    *'<html'*|*navbar*) bad "htmx is answered with rows rather than the page" "the whole document came back" ;;
    *'>notes.txt<'*)    ok "htmx is answered with rows rather than the page" ;;
    *)                  bad "htmx is answered with rows rather than the page" "$(head -c 120 <<<"$fragment")" ;;
  esac

  # Uploaded through the browser form, read back through WebDAV: two doors into
  # one tree, which is most of the architecture in a single assertion.
  updir="$(mktmp)"
  printf 'from the browser' > "$updir/upload.txt"
  code="$(curl -s -o /dev/null -w '%{http_code}' -b "$jar" \
    -F "file=@$updir/upload.txt" "http://$davhost/files/")"
  back="$(curl -fsS -u "$davuser:$davpass" "http://$davhost/dav/upload.txt" 2>/dev/null || true)"
  if [ "$code" = "303" ] && [ "$back" = "from the browser" ]; then
    ok "a file uploaded in the browser is there over WebDAV"
  else
    bad "a file uploaded in the browser is there over WebDAV" "upload answered $code, WebDAV gave '$back'"
  fi

  # A folder made in the browser is a collection over WebDAV, and the only way
  # to show that is to put something in it through the other door.
  code="$(curl -s -o /dev/null -w '%{http_code}' -b "$jar" \
    --data-urlencode "name=made here" "http://$davhost/folders/")"
  put="$(curl -s -o /dev/null -w '%{http_code}' -u "$davuser:$davpass" \
    -X PUT --data-binary 'inside' "http://$davhost/dav/made%20here/inside.txt")"
  if [ "$code" = "303" ] && [ "$put" = "201" ]; then
    ok "a folder made in the browser is a collection over WebDAV"
  else
    bad "a folder made in the browser is a collection over WebDAV" \
      "the form answered $code, the PUT into it answered $put"
  fi

  # The same file, renamed and then deleted in the browser, checked through the
  # other door each time: one thing's whole life, seen from both sides.
  code="$(curl -s -o /dev/null -w '%{http_code}' -b "$jar" \
    --data-urlencode "name=renamed.txt" "http://$davhost/rename/upload.txt")"
  old="$(curl -s -o /dev/null -w '%{http_code}' -u "$davuser:$davpass" "http://$davhost/dav/upload.txt")"
  new="$(curl -fsS -u "$davuser:$davpass" "http://$davhost/dav/renamed.txt" 2>/dev/null || true)"
  if [ "$code" = "303" ] && [ "$old" = "404" ] && [ "$new" = "from the browser" ]; then
    ok "a file renamed in the browser has the new name over WebDAV"
  else
    bad "a file renamed in the browser has the new name over WebDAV" \
      "the form answered $code, the old name $old, the new one '$new'"
  fi

  # And a folder with something in it, which was refused on every surface until
  # #101: the rename is one form post and every path underneath moves with it.
  curl -s -o /dev/null -b "$jar" --data-urlencode "name=holiday" "http://$davhost/folders/"
  curl -s -o /dev/null -u "$davuser:$davpass" -X PUT --data-binary 'inside' \
    "http://$davhost/dav/holiday/inside.txt"
  code="$(curl -s -o /dev/null -w '%{http_code}' -b "$jar" \
    --data-urlencode "name=archive" "http://$davhost/rename/holiday")"
  moved="$(curl -s -o /dev/null -w '%{http_code}' -u "$davuser:$davpass" "http://$davhost/dav/archive/inside.txt")"
  left="$(curl -s -o /dev/null -w '%{http_code}' -u "$davuser:$davpass" "http://$davhost/dav/holiday/inside.txt")"
  if [ "$code" = "303" ] && [ "$moved" = "200" ] && [ "$left" = "404" ]; then
    ok "a folder renamed in the browser takes its contents with it"
  else
    bad "a folder renamed in the browser takes its contents with it" \
      "the form answered $code, the moved file $moved, the old path $left"
  fi

  code="$(curl -s -o /dev/null -w '%{http_code}' -b "$jar" -X POST "http://$davhost/delete/renamed.txt")"
  gone="$(curl -s -o /dev/null -w '%{http_code}' -u "$davuser:$davpass" "http://$davhost/dav/renamed.txt")"
  if [ "$code" = "303" ] && [ "$gone" = "404" ]; then
    ok "a file deleted in the browser is gone over WebDAV"
  else
    bad "a file deleted in the browser is gone over WebDAV" \
      "the form answered $code, WebDAV answered $gone"
  fi

  # A source file with the extension of a transport stream, which is what makes
  # the failure count below mean something: what decides that this is not a
  # video is reading it (#146).
  printf 'import { Stratus } from "./stratus"\nexport const backup = 1\n' |
    curl -fsS -u "$davuser:$davpass" -X PUT --data-binary @- \
      "http://$davhost/dav/backup.ts" >/dev/null 2>&1

  # The library reports on itself, which is the only way to see a first pass
  # over an adopted bucket getting anywhere. Everything this script uploaded has
  # been through the indexer by now -- and, with the safety net ten minutes
  # away, only because each write handed its file over.
  finished=""
  for _ in $(seq 1 50); do
    status="$(curl -fsS -b "$jar" "http://$davhost/status" 2>/dev/null || true)"
    case "$status" in *'>100%<'*) finished=yes; break ;; esac
    sleep 0.2
  done
  if [ -n "$finished" ]; then
    ok "the status page reports the library indexed"
  else
    bad "the status page reports the library indexed" "$(grep -o '[0-9]*%' <<<"$status" | head -3 | tr '\n' ' ')"
  fi

  # Indexed is not the same as read: an extraction that failed still counts as
  # done, so this is the number that says whether anything went wrong. Exactly
  # one thing did, and on purpose -- the track this script uploaded is a text
  # file with an .mp3 name, which is how the Subsonic case proves that browsing
  # reads the row rather than the bytes.
  #
  # The TypeScript file above is the other half of that number: by its name it
  # is a transport stream, and before #146 it would have been copied to disk and
  # handed to ffprobe, which fails. Two failures here rather than one means the
  # name won an argument it should not have been in.
  case "$status" in
    *'data-count="failed">1<'*) ok "the only file that could not be read is the one planted here" ;;
    *) bad "the only file that could not be read is the one planted here" \
         "$(grep -o 'data-count="[a-z]*">[0-9]*' <<<"$status" | tr '\n' ' ')" ;;
  esac

  # And the video is not it. Its row is marked in the listing when it is waiting
  # or unreadable, and it is neither: the metadata came out of the container
  # over ranges, with no local copy and no ffprobe.
  clip="$(curl -fsS -b "$jar" "http://$davhost/files/" 2>/dev/null | sed -n '/>clip.mp4</,/<\/tr>/p')"
  case "$clip" in
    "")                   bad "the video is in the listing" "no row for clip.mp4" ;;
    *unreadable*|*waiting*) bad "the video was read out of its container" "$(tr -d '\n' <<<"$clip" | head -c 120)" ;;
    *)                    ok "the video was read out of its container" ;;
  esac
  # The CSRF defence, from outside: a form on somebody else's page carries the
  # cookie and must still be refused.
  code="$(curl -s -o /dev/null -w '%{http_code}' -b "$jar" \
    -H 'Sec-Fetch-Site: cross-site' -X POST "http://$davhost/logout")"
  if [ "$code" = "403" ]; then
    ok "a cross-site form post is refused"
  else
    bad "a cross-site form post is refused" "got $code"
  fi

  curl -s -o /dev/null -b "$jar" -c "$jar" -X POST "http://$davhost/logout"
  code="$(curl -s -o /dev/null -w '%{http_code}' -b "$jar" "http://$davhost/")"
  if [ "$code" = "303" ]; then
    ok "signing out ends the session"
  else
    bad "signing out ends the session" "got $code"
  fi

  # Vendored into the binary, never a CDN: a self-hosted cloud has to work with
  # no outbound network at all.
  code="$(curl -s -o /dev/null -w '%{http_code} %{content_type} %{size_download}' \
    "http://$davhost/static/bootstrap-5.3.8/bootstrap.min.css")"
  case "$code" in
    "200 text/css"*" 232111") ok "Bootstrap is served out of the binary" ;;
    *) bad "Bootstrap is served out of the binary" "got '$code'" ;;
  esac

  # Vendored the same way and for the same reason. The size is the assertion
  # that it is the published file and not something a build step produced.
  code="$(curl -s -o /dev/null -w '%{http_code} %{content_type} %{size_download}' \
    "http://$davhost/static/htmx-2.0.10/htmx.min.js")"
  case "$code" in
    "200 text/javascript"*" 51238") ok "htmx is served out of the binary" ;;
    *) bad "htmx is served out of the binary" "got '$code'" ;;
  esac

  csp="$(curl -s -D - -o /dev/null "http://$davhost/login" | grep -i '^content-security-policy:' | tr -d '\r')"
  case "$csp" in
    *"default-src 'none'"*) ok "the pages carry a content security policy" ;;
    *)                      bad "the pages carry a content security policy" "got '$csp'" ;;
  esac
  # Under default-src 'none' a browser refuses the listing's next page before
  # htmx sees it, and nothing in the server would notice: one directive, and
  # the only place its absence shows is a browser.
  case "$csp" in
    *"connect-src 'self'"*) ok "the policy lets the listing ask for its next page" ;;
    *)                      bad "the policy lets the listing ask for its next page" "got '$csp'" ;;
  esac

  # One line per request, which is the only way to see a 401 or a 409 after the
  # fact. The healthcheck is deliberately not in there.
  #
  # Waited for rather than checked once: docker collects a container's stdout
  # asynchronously, so a line written a moment ago is not necessarily one that
  # `docker logs` will show yet.
  logged=""
  for _ in $(seq 1 25); do
    if docker logs "$davname" 2>&1 | grep -q '"msg":"request".*"path":"/dav/notes.txt"'; then
      logged=yes
      break
    fi
    sleep 0.2
  done
  if [ -n "$logged" ]; then
    ok "every request leaves a log line"
  else
    bad "every request leaves a log line" "$(docker logs "$davname" 2>&1 | tail -3)"
  fi
  if docker logs "$davname" 2>&1 | grep -q '"path":"/healthz"'; then
    bad "the healthcheck stays out of the log" "it logs every thirty seconds"
  else
    ok "the healthcheck stays out of the log"
  fi

  # Nothing here was found by a query, which is the claim of #158. The status
  # page above already says the library is indexed; this says how it got that
  # way. "indexed media" is logged by a pass over the pending query and by
  # nothing else, so its absence means every file was read because its own
  # write handed it over.
  #
  # The interval of this container is ten minutes and the run is far shorter, so
  # a line here would mean the query ran anyway -- either on a timer that should
  # not have fired, or because a notice was dropped with a handful of uploads.
  if docker logs "$davname" 2>&1 | grep -q '"msg":"indexed media"'; then
    bad "every file was indexed by its own write, with no query" \
      "$(docker logs "$davname" 2>&1 | grep '"msg":"indexed media"' | head -2)"
  else
    ok "every file was indexed by its own write, with no query"
  fi
else
  bad "the container with credentials starts" "$(docker logs "$davname" 2>&1 | tail -3)"
fi
stop_container "$davname"

# ---------------------------------------------------------------------------
section "Configuration failure matrix"
# ---------------------------------------------------------------------------
# Same argument as the data directory: a configuration the server can never
# honour has to stop it at startup, not surface on the first request.

refuses() {
  local name="$1"; shift
  if docker run --rm ${cover_args[@]+"${cover_args[@]}"} "$@" "$RUN_REF" >/dev/null 2>&1; then
    bad "$name" "the server started"
  else
    ok "$name"
  fi
}

refuses "a malformed storage DSN refuses to start" -e STRATUS_STORAGE_DSN=nonsense
refuses "an unsupported storage scheme refuses to start" -e STRATUS_STORAGE_DSN=ftp://example.com/blobs
refuses "a password with no username refuses to start" -e STRATUS_PASSWORD=an-example-password
refuses "a username with no password refuses to start" -e STRATUS_USERNAME=edu
refuses "a malformed database DSN refuses to start" -e STRATUS_DB_DSN=nonsense
refuses "an unsupported database scheme refuses to start" -e STRATUS_DB_DSN=oracle://user:pass@db/stratus
refuses "an unreachable database refuses to start" -e STRATUS_DB_DSN=postgres://u:p@127.0.0.1:1/stratus?sslmode=disable
# The one that reads as harmless and is not: a level nobody can parse used to
# start the server at info, and whoever set it debugged blind.
refuses "an unparseable log level refuses to start" -e STRATUS_LOG_LEVEL=debgu

# ---------------------------------------------------------------------------
section "Compose healthcheck"
# ---------------------------------------------------------------------------
# The exec-form healthcheck in compose.yaml is what actually runs in production.
# `up --wait` returns non-zero unless every service reports healthy, so this one
# command proves the healthcheck works end to end.
composedir="$(mktmp)"
if STRATUS_DATA_PATH="$composedir" STRATUS_PORT=18099 \
     docker compose -p stratus-smoke up -d --build --wait --quiet-pull >/dev/null 2>&1; then
  ok "compose reports the service healthy (exec-form healthcheck works)"
else
  bad "compose reports the service healthy" \
      "$(STRATUS_DATA_PATH="$composedir" STRATUS_PORT=18099 docker compose -p stratus-smoke logs 2>&1 | tail -3)"
fi
STRATUS_DATA_PATH="$composedir" STRATUS_PORT=18099 \
  docker compose -p stratus-smoke down >/dev/null 2>&1 || true

# ---------------------------------------------------------------------------
printf '\n\033[1mSummary\033[0m\n'
printf '  %s passed, %s failed\n\n' "$(green "$pass")" "$([ "$fail" -eq 0 ] && printf '%s' "$fail" || red "$fail")"
[ "$fail" -eq 0 ]
