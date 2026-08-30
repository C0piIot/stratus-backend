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
# One consequence worth knowing: if MinIO is not running, the S3 conformance
# suite skips and internal/storage/s3 drops to ~15%, so this script turns a
# silent skip into a failed build.
#
# Usage: scripts/coverage.sh [coverage.out]   (or: make cover)

set -euo pipefail

profile="${1:-coverage.out}"

FLOORS="
internal/app:93
internal/config:100
internal/storage:98
internal/storage/disk:84
internal/storage/s3:89
"

# Not gated, and why:
#   cmd/stratus                   flags and exit codes; what it does is asserted
#                                 by scripts/smoke.sh, which unit coverage
#                                 cannot see.
#   internal/storage/storagetest  the conformance suite itself. It runs from the
#                                 disk and s3 tests, and Go attributes that
#                                 coverage to them, not to it.

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
