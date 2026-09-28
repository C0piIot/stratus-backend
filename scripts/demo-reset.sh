#!/usr/bin/env bash
#
# Empties the demo instance and puts the demo media back: what every deploy and
# the hourly reset do, in one place so the two cannot drift.
#
#   FLY_API_TOKEN=... scripts/demo-reset.sh <app> [image]
#
# **The wipe is destroying the volume**, not the machine and not a deploy. The
# data lives on a Fly volume (#238), which outlives the machine it is mounted
# on -- that is what a volume is for, and what the demo needed after a cold
# start threw away the machine's own disk in the middle of an hour. So both
# go, and the deploy makes a new machine on a new, empty volume.
#
# Then it checks the tree really is empty before seeding. A reset that kept
# anything would seed on top of it and look fine, which is how a schema change
# once landed on a database written by the build before it.
#
# Without an image it deploys what fly.toml names, which is what CI last
# published.

set -euo pipefail

APP="${1:?usage: demo-reset.sh <app> [image]}"
IMAGE="${2:-}"
BASE="https://$APP.fly.dev"
HERE="$(cd "$(dirname "$0")" && pwd)"

for id in $(flyctl machine list --app "$APP" --json | jq -r '.[].id'); do
	flyctl machine destroy "$id" --app "$APP" --force
done

# A volume is still attached for a moment after its machine is destroyed, and
# refuses to go until it is not.
for id in $(flyctl volumes list --app "$APP" --json | jq -r '.[].id'); do
	for attempt in 1 2 3 4 5 6; do
		flyctl volumes destroy "$id" --app "$APP" --yes && break
		[ "$attempt" -lt 6 ] || exit 1
		sleep 5
	done
done

# A deploy will not make a volume for a new machine, only mount one that is
# there: the name, region and size are fly.toml's. No snapshots, which are
# billed, of a volume that is thrown away within the hour.
toml="$HERE/../fly.toml"
setting() { sed -n "s/^ *$1 *= *\"\(.*\)\"/\1/p" "$toml" | head -1; }
size="$(setting initial_size)"
flyctl volumes create "$(setting source)" --app "$APP" --region "$(setting primary_region)" \
	--size "${size%gb}" --scheduled-snapshots=false --yes

flyctl deploy --app "$APP" ${IMAGE:+--image "$IMAGE"} --ha=false --yes --wait-timeout 5m

# The version is the password, and every OpenSubsonic answer carries it,
# including the refusal for asking with no credentials.
version="$(curl -fsS "$BASE/rest/ping.view" | sed -n 's/.*serverVersion="\([^"]*\)".*/\1/p')"
[ -n "$version" ] || { echo "the instance did not say what it is running" >&2; exit 1; }
user="$(setting STRATUS_USERNAME)"

listing="$(curl -fsS -u "$user:$version" -X PROPFIND -H "Depth: 1" "$BASE/dav/")"
if [ "$(grep -o '<D:href>' <<<"$listing" | wc -l)" -ne 1 ]; then
	echo "the tree is not empty after the reset; not seeding on top of it:" >&2
	grep -o '<D:href>[^<]*' <<<"$listing" >&2
	exit 1
fi

echo "reseeding $APP: $version"
STRATUS_USERNAME="$user" STRATUS_PASSWORD="$version" "$HERE/seed-demo.sh" "$BASE"
