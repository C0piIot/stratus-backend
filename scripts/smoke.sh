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
BIN_SIZE_FAIL=$((20 * 1024 * 1024))

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

cleanup() {
  for c in ${containers[@]+"${containers[@]}"}; do docker rm -f "$c" >/dev/null 2>&1 || true; done
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
  docker run -d --name "$name" -p "127.0.0.1::8080" "$@" "$REF" >/dev/null
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
VERSION="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
if docker build --build-arg "VERSION=$VERSION" -t "$REF" . >/dev/null 2>&1; then
  ok "image builds ($REF)"
else
  bad "image builds" "docker build failed; rerun without -q to see why"
  echo; echo "aborting: nothing to test"; exit 1
fi

# ---------------------------------------------------------------------------
section "Image properties"
# ---------------------------------------------------------------------------
# The image must add exactly one layer to the base: the binary and nothing else.
# A second layer means a stray COPY or a leaked source tree.
docker pull -q "$BASE_IMAGE" >/dev/null 2>&1 || true
base_layers="$(docker image inspect -f '{{len .RootFS.Layers}}' "$BASE_IMAGE" 2>/dev/null || echo 0)"
img_layers="$(docker image inspect -f '{{len .RootFS.Layers}}' "$REF")"
# Two layers now: the binary and ffprobe. The number matters less than the fact
# that it is counted -- a base swapped for something with a package manager in
# it would show up here first.
if [ "$base_layers" -gt 0 ] && [ "$((img_layers - base_layers))" -eq 2 ]; then
  ok "adds exactly two layers over the base ($base_layers -> $img_layers)"
else
  bad "adds exactly two layers over the base" "base=$base_layers image=$img_layers"
fi

# ffprobe is a requirement, so its absence has to fail here rather than at the
# first video somebody uploads.
if docker run --rm --entrypoint /usr/local/bin/ffprobe "$REF" -version >/dev/null 2>&1; then
  ok "ffprobe runs inside the image"
else
  bad "ffprobe runs inside the image" "it is missing or not executable"
fi

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
out="$(docker run --rm "$REF" -version 2>&1 || true)"
if [ "$out" = "stratus $VERSION" ]; then
  ok "-version reports the injected version"
else
  bad "-version reports the injected version" "got '$out', want 'stratus $VERSION'"
fi

# The healthcheck must fail when nothing is listening, otherwise it can never
# mark a wedged container unhealthy.
if docker run --rm "$REF" -healthcheck >/dev/null 2>&1; then
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
  -not -path "$datadir/.index" \
  -not -name 'stratus.db*' | tr '\n' ' ')"
if [ -z "$leftovers" ]; then
  ok "neither write probe is left behind"
else
  bad "neither write probe is left behind" "found: $leftovers"
fi
docker rm -f "$name" >/dev/null 2>&1

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
docker rm -f "$name" >/dev/null 2>&1

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
  out="$(timeout "$STARTUP_DEADLINE" docker run --rm --name "$name" "$@" "$REF" 2>&1)"
  rc=$?
  set -e
  elapsed=$((SECONDS - start))
  docker rm -f "$name" >/dev/null 2>&1

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
docker rm -f "$name" >/dev/null 2>&1

# ---------------------------------------------------------------------------
section "WebDAV"
# ---------------------------------------------------------------------------
# The first surface that does something. Unit tests drive the handler; this
# drives the shipped image with a real client, over a real port.

davdir="$(mktmp)"
davname="stratus-smoke-dav"
davuser="edu"
davpass="an example password for the smoke tests"
run_detached "$davname" -u "$(id -u):$(id -g)" -v "$davdir:/data" \
  -e STRATUS_USERNAME="$davuser" -e STRATUS_PASSWORD="$davpass" \
  -e STRATUS_INDEX_INTERVAL=200ms

if wait_serving "$davname"; then
  davhost="$(docker port "$davname" 8080/tcp | head -1)"

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

  # The indexer picks up what was just uploaded, which is the whole loop: the
  # pending query, an extractor, and a row written back.
  indexed=""
  for _ in $(seq 1 50); do
    if docker logs "$davname" 2>&1 | grep -q '"msg":"indexed media"'; then
      indexed=yes
      break
    fi
    sleep 0.2
  done
  if [ -n "$indexed" ]; then
    ok "the indexer picks up an uploaded file"
  else
    bad "the indexer picks up an uploaded file" "$(docker logs "$davname" 2>&1 | tail -3)"
  fi
else
  bad "the WebDAV container starts" "$(docker logs "$davname" 2>&1 | tail -3)"
fi
docker rm -f "$davname" >/dev/null 2>&1

# ---------------------------------------------------------------------------
section "Configuration failure matrix"
# ---------------------------------------------------------------------------
# Same argument as the data directory: a configuration the server can never
# honour has to stop it at startup, not surface on the first request.

refuses() {
  local name="$1"; shift
  if docker run --rm "$@" "$REF" >/dev/null 2>&1; then
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
refuses "an unsupported database scheme refuses to start" -e STRATUS_DB_DSN=mysql://user:pass@db/stratus
refuses "an unreachable database refuses to start" -e STRATUS_DB_DSN=postgres://u:p@127.0.0.1:1/stratus?sslmode=disable

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
