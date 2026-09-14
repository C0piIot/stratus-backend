#!/usr/bin/env bash
#
# Per-package coverage floors.
#
# A single total would be noise here. internal/storage/s3 measures 15% or 90%
# depending on whether Silo is running, cmd/stratus is main(), and
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
# One consequence worth knowing: if Silo is not running, the S3 conformance
# suite skips and internal/storage/s3 drops to ~15%, so this script turns a
# silent skip into a failed build.
#
# Usage: scripts/coverage.sh [coverage.out]   (or: make cover)

set -euo pipefail

profile="${1:-coverage.out}"

FLOORS="
internal/app:94
internal/auth:100
internal/config:100
internal/dav:89
internal/files:91
internal/media:88
internal/db:61
internal/db/postgres:94
internal/db/sqlite:94
internal/db/sqlutil:95
internal/storage:98
internal/storage/disk:90
internal/storage/s3:88
internal/subsonic:100
internal/web:98
"

# Not gated, and why:
#   cmd/stratus                   flags and exit codes; what it does is asserted
#                                 by scripts/smoke.sh, which unit coverage
#                                 cannot see. `make smoke-cover` now measures it
#                                 -- 100% of statements, in a profile of its own
#                                 that these floors deliberately do not read.
#   internal/storage/storagetest  the conformance suite itself. It runs from the
#   internal/db/dbtest            disk, s3, sqlite and postgres tests, and Go
#                                 attributes that coverage to them, not to it.
#
# internal/app went 92 -> 94 with /readyz (#34), which is reachable in every
# branch it has: a closed store answers, and what it answers is not "not found".
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
# The drivers went 93 -> 94 and 92 -> 94, and internal/db 60 -> 61, with the
# search queries (#85). Four new methods each, all of them Collect plus a scan,
# and the branch each has that a working database will not take on request is
# reached by closing the store under the query. internal/db rose because the
# folding those queries match against is a pure function of the port, testable
# where it lives.
#
# internal/web started at 97 and went to 98 with renaming and deleting, which
# arrived with the fault injection the rest of this file talks about: a database
# that refuses a delete, and one that cannot say what is inside a folder.
#
# What is left uncovered there is two branches, both unreachable on purpose. One
# is a template that fails halfway, which is why rendering goes through a buffer
# rather than straight to the ResponseWriter -- the templates are parsed at
# startup with template.Must and executed over a struct of strings, so nothing
# is left that can fail. The other is opening a directory as a file, which the
# page that serves bytes checks for and the page that routes to it already
# decided. Deleting either to reach 100 would delete the reason it is there.
#
# internal/subsonic starts at 100, which is high but is what the package is: it
# does no I/O of its own beyond writing a response, so every branch is reachable
# from httptest -- the two that report a client hanging up mid-response included,
# through a ResponseWriter that fails on demand.
#
# internal/dav went 80 -> 89 and internal/storage/disk 83 -> 90 with #53, and
# how says more than the number. That issue's premise was that the floors were
# low because nothing here can be made to fail on demand, and it proposed a
# fault injector over the two ports. Measured, the two lowest floors in the
# project needed no such thing: dav's gaps were conditional-request combinations
# and an error table, both reachable from an ordinary request or from a unit
# test of the two pure functions involved; disk's were the operating system
# refusing, which a test provokes with an awkward path and a chmod -- and a
# wrapper around storage.Storage cannot help the package that *is*
# storage.Storage anyway.
#
# What genuinely needed the injector was narrower: failures that happen *after*
# something else succeeded. internal/files went 87 -> 91 on those, and the case
# that mattered was the half of "blob first, row second" where the row does not
# land -- which turned out to need *both* seams failing, because Write already
# removes the blob by hand and only a cleanup that fails too leaves the orphan
# the sweep is for. The injectors are internal/storage/storagetest.FailOn and
# internal/db/dbtest.FailOn, and every case they enable was checked by turning
# them off: a test that still passes with no failure injected was testing
# nothing.
#
# internal/media went 84 -> 88 with the embedded-cover parsers (#86): a tag is
# a byte slice, so every bound and every malformed length is reachable from a
# hand-built fixture without a backend that fails on demand. What is left
# uncovered there is the seek and store failures, which need the injector #53 is
# about.
#
# internal/files went 86 -> 87 and internal/media 81 -> 84 with thumbnails
# (#45): the sweep's new rule and the resize arithmetic are both pure functions
# of their input, so the branches that matter are reachable without a backend
# that fails on demand. internal/media also gained its own tests for a path
# that a protocol adapter exercises -- coverage of it counts against the
# package the test lives in, not the one the code does.
#
# internal/media is lower than the rest because running ffprobe cannot be tested
# where there is no ffprobe. Interpreting its output is tested against captured
# reports, and executing it is asserted by scripts/smoke.sh inside the image
# that has it.
#
# internal/storage/s3 likewise: the multipart sweep is exercised against Silo,
# but the two branches that report a failure from the listing or the abort need
# a server that fails on demand.
#
# internal/storage/disk went down a point when the .tmp sweep landed: its two
# error branches need a filesystem that fails a ReadDir or a Remove.
#
# The two SQL drivers went down a point when BlobKeys landed, because a scan
# failure and a rows.Err mid-iteration do not happen without a fault-injecting
# driver. That is no longer where those branches live: #30 moved them to
# internal/db/sqlutil, which holds no SQL and can register such a driver, and
# the drivers went back up as a result.
#
# The drivers went 92 -> 93 and 91 -> 92 with the music queries: the browse
# methods are all Collect plus a scan, and the one branch each that a working
# database will not take on request is reached by closing the store under the
# query -- the same trick TestBlobKeysOnAClosedStore already used.
#
# internal/dav sits lower than the rest on purpose: most of what is left
# uncovered there is one error branch per protocol edge, and the ones worth
# pinning -- the status codes RFC 4918 is specific about -- are asserted.
#
# internal/db has a low floor for the same reason: db.Migrate is exercised by
# both driver packages, and the import cycle that keeps drivers out of the port
# means it cannot open a database of its own to test against. It went 58 -> 60
# with Media.Normalize (#59), which is pure enough to test where it lives.

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
