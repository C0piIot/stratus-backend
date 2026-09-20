#!/usr/bin/env bash
#
# litmus against the shipped image: what WebDAV actually is, rather than what
# we think it is.
#
# Our own tests are written from the same understanding as the code, so they
# agree with it by construction. This does not: it is a 2011 C program by the
# people who wrote the RFC's reference implementations, and it has no opinion
# about how we meant things. Its first run found a SQLITE_BUSY under two
# writers that every test here had missed (#183).
#
# Each suite is held to the number of tests it passes today. A suite that
# passes fewer is a regression and fails the build; one that passes more is a
# number to update, in a commit, with the reason. That is the same discipline
# as deps.allow and the coverage floors, and it is the difference between
# "known gap" and "silently excluded".
set -euo pipefail

cd "$(dirname "$0")/.."

IMAGE="${IMAGE:-stratus-backend:latest}"
LITMUS_IMAGE="${LITMUS_IMAGE:-stratus-litmus:0.13}"
DAV_USER=litmus
DAV_PASS=litmus-secret

# What each suite passes against this server, and why the short ones are short.
#
#   basic, copymove, http  -- everything.
#   props    10 of 14  PROPPATCH is refused: this server has no dead
#                      properties, so three of these cannot pass, and the
#                      fourth wants a malformed namespace declaration answered
#                      with 400 where the library answers 207.
#   locks    17 of 32  LOCK is advertised and not enforced (#174). Everything
#                      that checks a lock actually holds a resource fails, on
#                      purpose and on the record, and the README says so.
declare -A EXPECTED=( [basic]=16 [copymove]=13 [http]=4 [props]=10 [locks]=17 )
SUITES=(basic copymove http props locks)

green=$'\e[32m'; red=$'\e[31m'; bold=$'\e[1m'; off=$'\e[0m'
passed=0; failed=0
ok()  { printf '  %s✓%s %s\n' "$green" "$off" "$1"; passed=$((passed + 1)); }
bad() { printf '  %s✗%s %s\n      %s\n' "$red" "$off" "$1" "$2"; failed=$((failed + 1)); }

NET="stratus-litmus-$$"
NAME="stratus-litmus-server-$$"
DATA="$(mktemp -d)"

cleanup() {
    docker rm -f "$NAME" >/dev/null 2>&1 || true
    docker network rm "$NET" >/dev/null 2>&1 || true
    rm -rf "$DATA"
}
trap cleanup EXIT

docker build -q -t "$LITMUS_IMAGE" build/litmus >/dev/null
docker network create "$NET" >/dev/null

# The indexer stays on. It is a writer, and a suite that only passed with it
# turned off would be testing a server nobody runs -- which is exactly how the
# SQLITE_BUSY was found.
docker run -d --name "$NAME" --network "$NET" \
    -u "$(id -u):$(id -g)" -v "$DATA:/data" \
    -e STRATUS_USERNAME="$DAV_USER" -e STRATUS_PASSWORD="$DAV_PASS" \
    "$IMAGE" >/dev/null

for _ in $(seq 1 50); do
    if docker run --rm --network "$NET" "$LITMUS_IMAGE" \
        sh -c "exec 3<>/dev/tcp/$NAME/8080" >/dev/null 2>&1; then
        break
    fi
    sleep 0.2
done

printf '%slitmus %s against %s%s\n' "$bold" "${LITMUS_IMAGE#*:}" "$IMAGE" "$off"
for suite in "${SUITES[@]}"; do
    out="$(docker run --rm --network "$NET" "$LITMUS_IMAGE" \
        "$suite" "http://$NAME:8080/dav/" "$DAV_USER" "$DAV_PASS" 2>&1 || true)"
    summary="$(grep -o 'of [0-9]* tests run: [0-9]* passed' <<<"$out" | tail -1)"
    got="$(awk '{print $5}' <<<"$summary")"
    want="${EXPECTED[$suite]}"

    case "$got" in
        "$want") ok "$suite: $summary" ;;
        "")      bad "$suite" "no summary line; $(tail -3 <<<"$out")" ;;
        *)       bad "$suite: $summary, expected $want to pass" \
                     "$(grep -E 'FAIL' <<<"$out" | head -3)" ;;
    esac
done

printf '\n%sSummary%s\n  %s%d%s passed' "$bold" "$off" "$green" "$passed" "$off"
if [ "$failed" -gt 0 ]; then
    printf ', %s%d%s failed\n' "$red" "$failed" "$off"
    exit 1
fi
printf '\n'
