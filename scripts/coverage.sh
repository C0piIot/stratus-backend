#!/usr/bin/env bash
#
# Per-package coverage floors.
#
# A single total would be noise here. internal/storage/s3 measures 15% or 90%
# depending on whether MinIO is running, cmd/stratus is main(), and
# internal/storage/storagetest is executed from the tests of the packages it
# checks -- coverage Go does not attribute back to it.
#
# The floors below are the numbers on the day they were set, rounded down, so
# they can only go up. Raising one belongs to the PR that earns it; lowering one
# is a conversation, in the PR that needs it.
#
# internal/app went 95 -> 94 when Run was split into open() and the lifecycle.
# The split added one statement that no test can reach: the log line for a
# backend that fails to close while the server is shutting down. Run builds its
# own dependencies, so there is nowhere to inject one that fails. Deleting the
# log to protect the number would be the metric wagging the code.
#
# One consequence worth knowing: if MinIO is not running, the S3 conformance
# suite skips and internal/storage/s3 drops to ~15%, so this script turns a
# silent skip into a failed build.
#
# Usage: scripts/coverage.sh [coverage.out]   (or: make cover)

set -euo pipefail

profile="${1:-coverage.out}"

FLOORS="
internal/app:92
internal/auth:100
internal/config:100
internal/dav:80
internal/files:86
internal/media:81
internal/db:58
internal/db/postgres:92
internal/db/sqlite:91
internal/db/sqlutil:95
internal/storage:98
internal/storage/disk:83
internal/storage/s3:88
"

# Not gated, and why:
#   cmd/stratus                   flags and exit codes; what it does is asserted
#                                 by scripts/smoke.sh, which unit coverage
#                                 cannot see.
#   internal/storage/storagetest  the conformance suite itself. It runs from the
#   internal/db/dbtest            disk, s3, sqlite and postgres tests, and Go
#                                 attributes that coverage to them, not to it.
#
# internal/app went 94 -> 92 with the media indexer: what is left uncovered in
# both background loops is the branch where a pass fails halfway, and injecting
# that means breaking a backend underneath a goroutine that is already running.
#
# The two drivers went 88 -> 91 and 89 -> 92 when the plumbing they had a copy
# of each moved to internal/db/sqlutil (#30). What was hard to cover in them was
# exactly that plumbing -- a RowsAffected that fails, a result set that stops
# halfway -- so removing it raised what was left. internal/db/sqlutil starts at
# 95 because a package that holds no SQL can register a fault-injecting driver
# and reach those branches on purpose, which neither adapter can.
#
# internal/media is lower than the rest because running ffprobe cannot be tested
# where there is no ffprobe. Interpreting its output is tested against captured
# reports, and executing it is asserted by scripts/smoke.sh inside the image
# that has it.
#
# internal/storage/s3 likewise: the multipart sweep is exercised against MinIO,
# but the two branches that report a failure from the listing or the abort need
# a server that fails on demand.
#
# internal/storage/disk went down a point when the .tmp sweep landed: its two
# error branches need a filesystem that fails a ReadDir or a Remove.
#
# The two SQL drivers went down a point when BlobKeys landed: what is left
# uncovered there is a scan failure and a rows.Err mid-iteration, neither of
# which happens without a fault-injecting driver -- more machinery than three
# log-and-return lines are worth.
#
# internal/dav sits lower than the rest on purpose: most of what is left
# uncovered there is one error branch per protocol edge, and the ones worth
# pinning -- the status codes RFC 4918 is specific about -- are asserted.
#
# internal/db has a low floor for the same reason: db.Migrate is exercised by
# both driver packages, and the depguard rule that keeps drivers out of the port
# means it cannot open a database of its own to test against.

[ -f "$profile" ] || { echo "no coverage profile at $profile"; exit 1; }

green() { printf '\033[32m%s\033[0m' "$1"; }
red()   { printf '\033[31m%s\033[0m' "$1"; }

# go tool cover -func reports per function, and a package percentage is a
# statement-weighted sum, so this reads the profile itself: "file:from,to stmts
# count" per block.
percentages="$(awk '
	NR > 1 {
		split($1, loc, ":")
		path = loc[1]
		sub(/\/[^\/]*$/, "", path)              # dirname
		sub(/^.*stratus-backend\//, "", path)   # drop the module prefix
		stmts[path] += $2
		if ($3 > 0) hit[path] += $2
	}
	END {
		for (p in stmts) printf "%s %.1f\n", p, 100 * hit[p] / stmts[p]
	}
' "$profile")"

fail=0
printf '\n\033[1mCoverage floors\033[0m\n'

for entry in $FLOORS; do
	pkg="${entry%%:*}"
	floor="${entry##*:}"

	got="$(awk -v p="$pkg" '$1 == p {print $2}' <<<"$percentages")"
	if [ -z "$got" ]; then
		printf '  %s %-28s no data in the profile\n' "$(red ✗)" "$pkg"
		fail=$((fail + 1))
		continue
	fi

	# Integer comparison in tenths: this has to work without bc.
	if [ "$(printf '%.0f' "$(awk -v g="$got" 'BEGIN{print g*10}')")" -ge "$((floor * 10))" ]; then
		printf '  %s %-28s %5s%% (floor %s%%)\n' "$(green ✓)" "$pkg" "$got" "$floor"
	else
		printf '  %s %-28s %5s%% is below the %s%% floor\n' "$(red ✗)" "$pkg" "$got" "$floor"
		fail=$((fail + 1))
	fi
done

if [ "$fail" -gt 0 ]; then
	printf '\n%s package(s) failed the floor check. Add tests, or raise the floor in %s and say why.\n' "$fail" "$0"
	exit 1
fi
printf '\n'
