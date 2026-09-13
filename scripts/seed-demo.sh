#!/usr/bin/env bash
#
# Puts the demo media into an instance, over the same WebDAV any client speaks.
#
# The test instance keeps nothing -- no volume, and a deploy replaces its disk
# along with the image -- so a fresh machine has an empty tree, an indexer with
# nothing to read and a music library with no artists in it. This fills it in
# again: five photographs with their EXIF, two tagged tracks with a cover, and
# fifteen seconds of video.
#
# The media is a release asset rather than a directory in this repository: the
# whole clone is 6.6 MB and the bundle is 4.4, which is not a weight to carry in
# every clone forever. scripts/demo/SHA256SUMS pins the bytes; the attribution
# is in scripts/demo/CREDITS.md and travels inside the tarball as well.
#
#   STRATUS_USERNAME=demo STRATUS_PASSWORD=... scripts/seed-demo.sh https://host
#   make demo BASE=http://localhost:8080
#
# It ends by asking the server what it just did: the listing, the artist, the
# cover and a range request. That makes it the one check this project runs
# against something deployed rather than against an image on the build machine.

set -euo pipefail

BASE="${1:-${BASE:-http://localhost:8080}}"
BASE="${BASE%/}"
USER="${STRATUS_USERNAME:?set STRATUS_USERNAME}"
PASS="${STRATUS_PASSWORD:?set STRATUS_PASSWORD}"

TAG="demo-media-v1"
BUNDLE="$TAG.tar.gz"
URL="https://github.com/C0piIot/stratus-backend/releases/download/$TAG/$BUNDLE"
HERE="$(cd "$(dirname "$0")" && pwd)"

green() { printf '\033[32m%s\033[0m' "$1"; }
red()   { printf '\033[31m%s\033[0m' "$1"; }
ok()    { printf '  %s %s\n' "$(green ✓)" "$1"; }
die()   { printf '  %s %s\n' "$(red ✗)" "$1" >&2; exit 1; }

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# Percent-encoding by hand, over bytes: a name with a space in it is a URL a
# client has to be able to follow, and "Kevin MacLeod/Stratus demo" is in there
# precisely because that is the interesting case.
encode() {
	local LC_ALL=C s="$1" out="" i c
	for ((i = 0; i < ${#s}; i++)); do
		c="${s:i:1}"
		case "$c" in
		[a-zA-Z0-9._~/-]) out+="$c" ;;
		*) printf -v c '%%%02X' "'$c"; out+="$c" ;;
		esac
	done
	printf '%s' "$out"
}

content_type() {
	case "${1##*.}" in
	jpg | jpeg) printf 'image/jpeg' ;;
	png) printf 'image/png' ;;
	mp3) printf 'audio/mpeg' ;;
	mp4) printf 'video/mp4' ;;
	md) printf 'text/markdown; charset=utf-8' ;;
	*) printf 'application/octet-stream' ;;
	esac
}

dav() { curl -fsS -u "$USER:$PASS" "$@"; }

# OpenSubsonic does not use Basic: it authenticates per request from the query
# string, and every answer -- errors included -- is an HTTP 200 with the reason
# inside it. Both facts are the protocol's, and both have to be honoured here or
# this script waits out its deadline reading refusals.
SALT="seed$$"
TOKEN="$(printf '%s' "$PASS$SALT" | md5sum | cut -d' ' -f1)"
rest() {
	local method="$1"; shift
	local body
	body="$(curl -fsS "$BASE/rest/$method.view?c=seed-demo&f=xml&v=1.16.1&u=$(encode "$USER")&t=$TOKEN&s=$SALT&$*" || true)"
	case "$body" in
	*'status="failed"'*) die "$method: $(sed -n 's/.*message="\([^"]*\)".*/\1/p' <<<"$body")" ;;
	esac
	printf '%s' "$body"
}

printf '\n\033[1mFetching\033[0m\n'
curl -fsSL -o "$work/$BUNDLE" "$URL" || die "could not download $URL"
(cd "$work" && sha256sum -c "$HERE/demo/SHA256SUMS" >/dev/null 2>&1) ||
	die "$BUNDLE does not match scripts/demo/SHA256SUMS"
ok "$BUNDLE, and it is the bundle SHA256SUMS names"

mkdir -p "$work/media"
tar -xzf "$work/$BUNDLE" -C "$work/media"
files="$(cd "$work/media" && find . -type f | sed 's|^\./||' | sort)"
count="$(wc -l <<<"$files")"

printf '\n\033[1mUploading to %s\033[0m\n' "$BASE"
# Collections first, parents before children: MKCOL makes one directory, not a
# path, and a 405 means somebody already made it.
while read -r dir; do
	[ "$dir" = "." ] && continue
	code="$(curl -s -o /dev/null -w '%{http_code}' -u "$USER:$PASS" -X MKCOL "$BASE/dav/$(encode "$dir")")"
	case "$code" in
	201 | 405) ;;
	*) die "MKCOL $dir answered $code" ;;
	esac
done < <(cd "$work/media" && find . -type d | sed 's|^\./||' | sort)

while read -r f; do
	dav -T "$work/media/$f" -H "Content-Type: $(content_type "$f")" "$BASE/dav/$(encode "$f")" >/dev/null ||
		die "PUT $f failed"
done <<<"$files"
ok "$count files in"

printf '\n\033[1mWhat the server says about it\033[0m\n'
listed=0
while read -r f; do
	dav -o /dev/null "$BASE/dav/$(encode "$f")" && listed=$((listed + 1))
done <<<"$files"
[ "$listed" -eq "$count" ] || die "only $listed of $count files read back over WebDAV"
ok "every file reads back over WebDAV"

# The music surface needs the indexer to have run. Locally that is a minute at
# the default interval, so it is waited for rather than assumed.
deadline=$((SECONDS + 150))
artists=""
while [ $SECONDS -lt $deadline ]; do
	artists="$(rest getArtists)"
	case "$artists" in *"Kevin MacLeod"*) break ;; esac
	sleep 2
done
case "$artists" in
*"Kevin MacLeod"*) ok "the indexer read the tags: getArtists has the artist" ;;
*) die "getArtists never showed the artist; the indexer may be off or still working" ;;
esac

albums="$(rest getAlbumList2 "type=alphabeticalByName")"
case "$albums" in
*"Stratus demo"*) ok "getAlbumList2 has the album" ;;
*) die "getAlbumList2 does not list the album" ;;
esac

# The cover id comes out of the answer rather than being built here, which is
# what a client does with it too.
cover="$(tr '<' '\n' <<<"$albums" | grep 'name="Stratus demo"' | sed -n 's/.*coverArt="\([^"]*\)".*/\1/p' | head -1)"
[ -n "$cover" ] || die "the album carries no coverArt id"
type="$(curl -s -o /dev/null -w '%{content_type}' \
	"$BASE/rest/getCoverArt.view?c=seed-demo&u=$(encode "$USER")&t=$TOKEN&s=$SALT&id=$(encode "$cover")&size=300")"
case "$type" in
image/jpeg*) ok "getCoverArt answers a JPEG for the album" ;;
*) die "getCoverArt answered $type" ;;
esac

# Range requests are what a player uses to seek, and the video is here for them:
# the browser downloads it, since the UI serves attachments on purpose.
code="$(curl -s -o /dev/null -w '%{http_code}' -u "$USER:$PASS" -r 0-1023 \
	"$BASE/dav/$(encode "Video/sintel-trailer.mp4")")"
[ "$code" = "206" ] || die "a range request for the video answered $code"
ok "the video answers a range request"

printf '\n  %s %s\n\n' "$(green ✓)" "seeded: $BASE"
