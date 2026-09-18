# stratus-backend

The Stratus server: one Go binary, one container, no sidecars.

Stratus is a self-hosted personal cloud for photos, calendar, music and video.
Instead of shipping its own API and a client app per platform, it speaks
protocols your existing apps already understand.

> **Work in progress.** Files over WebDAV and music over OpenSubsonic work
> today, the web UI browses, uploads, downloads and tidies them, and the
> container is real. CalDAV and sharing are not written yet.
> The tables below say what answers and what does not, rather than what is
> intended — if a row says **works**, it works.

## Protocols

| Protocol | Use | Works with | Status |
|---|---|---|---|
| WebDAV | files, photo backup, sync | rclone, Finder, Nautilus, FolderSync | **works** |
| tus | resumable upload of large files | tus-js-client, TUSKit, tus-android-client | **works** ‡ |
| HTTP range | audio/video streaming | browsers, VLC, mpv | **works** |
| CalDAV | calendar | DAVx5, Thunderbird, iOS/macOS | next |
| OpenSubsonic | music | Symfonium, Substreamer, DSub, Feishin | **works** † |
| Web UI | sign in, browse, upload, download, rename, delete, library status | any browser | **partly** |
| CardDAV | contacts | DAVx5, Thunderbird | planned |
| DLNA / UPnP-AV | TVs, set-top players | | planned |

‡ Same caveat as below, one row down: the protocol works and the container
suite cuts an upload in half and resumes it, but no tus client library has been
pointed at this server yet.

† The protocol works and is asserted end to end, up to and including streaming
a track out of the shipped container. **No real client has been pointed at it
yet**, so read that row as the server holding up its end rather than as a
promise about any particular app.

Nothing here is a private API: every feature is reachable from a client that
already exists, which is why there is no Stratus app to install.

## WebDAV

Mounted at `/dav/`, behind HTTP Basic, and only when `STRATUS_USERNAME` and
`STRATUS_PASSWORD` are both set — an install nobody has configured is not a file
server.

```sh
rclone mount :webdav: /mnt/stratus --webdav-url http://localhost:8080/dav/ \
  --webdav-user edu --webdav-pass "$(rclone obscure "$STRATUS_PASSWORD")"
```

Automatic camera-roll backup is the thinnest part of this, and it is a client
problem rather than a server one. On Android, FolderSync schedules the camera
folder to a WebDAV target, though the version that does it comfortably is paid.
DAVx5 is free and very good at calendars and contacts, but it exposes WebDAV as
a storage provider for file managers rather than uploading a camera roll, so it
is listed against CalDAV above and not here. On iOS there is nothing free worth
recommending. The server side is plain WebDAV and works with any of them --
just do not read the table above as a promise that a phone backs itself up for
nothing.

A file is a row plus a blob, and `internal/files` is the only place that pair is
written: bytes go to the blob store under a name nothing derives from the path,
the tree lives in the database, and the ETag is a SHA-256 of what was actually
stored. Ranges, conditional requests and video seeking come from
`http.ServeContent`, which the reader is shaped for.

Locking is **advertised and not enforced**. macOS Finder refuses to mount a
share read-write unless the server claims class 2, so `LOCK` answers with a
well-formed token that nothing records and `UNLOCK` always succeeds.

That is a lie to the client, and the cost of it is worth stating: two clients
writing the same file at the same time are not protected — and they were not
protected before either, because there was no locking at all. It removes no
guarantee. The real defence against a lost update here is the strong ETag and
`If-Match`, which every write already honours.

### Logs

JSON on stdout, one line per request: method, path, status, bytes, duration and
the caller's address. No headers and no query string — one carries the
credentials and the other is where a token would end up if a protocol ever put
one there. `/healthz` and `/readyz` log at debug, because the container asks
every thirty seconds and three thousand lines a day of nothing is not a log — a
failed readiness check logs its own reason at error level instead.

### Media metadata

Every file that arrives is read once for what it can say about itself: when a
photo was taken, its dimensions and orientation, where it was taken, the camera;
the duration, artist, album and track of a recording; the codec and dimensions
of a video. Without it a library is a pile of files — there is no gallery by
date and no music browsing.

**What a file is comes from its first bytes**, not from its name. A name is
something somebody typed, and two cases matter: a recording copied off a phone
with no extension at all, and `.ts`, which is a transport stream from a set-top
box and a TypeScript source file in equal measure. Both are answered by reading
a few hundred bytes — the same reading that fills in a content type the client
did not give, and files a blob under `video/` rather than under `other/`. What
the bytes cannot say, the name still answers.

It runs in the background, in this process, and `STRATUS_INDEX_INTERVAL=0` turns
it off. **A file is read as it arrives**: an upload tells the indexer rather
than waiting to be found, so the interval above is the idle poll and the safety
net — for a version bump, for rows an import inserted, for anything that landed
while the server was not running. **An upgrade that improves the extractor
re-reads everything**: the queue is a query for files whose metadata is older
than the current extractor, so raising its version puts the whole library back
in it. That is deliberate — it is how a better extractor reaches what it
already looked at, with no migration and no script — but on a large library the
first pass after an upgrade is not free.

**Replacing a file re-reads it too.** The queue compares the validator the
metadata was extracted from against the file's, so overwriting a video with a
different one does not leave the old duration behind. `/status` in the web UI
reports how much of the library has been read, how much is waiting and what
could not be read at all.

**No large file is ever downloaded to be read.** That is the rule, and two
things follow from it.

The first is that the formats which say what they are near the beginning are
read where they lie: an MP4 or QuickTime video through its boxes, a Matroska or
WebM through its elements. Duration, dimensions, codec, rotation and date all
live in a header, and the blob store reads ranges, so a four-gigabyte recording
costs a few hundred kilobytes to index — on a bucket as much as on a disk.

The second is that everything else is only copied while it is small. AVI, WMV
and MPEG-TS state no duration at all: ffprobe works one out from what it can
reach, which means a partial file gives a plausible and wrong answer — measured,
on a thirty-second file, as 7.5 and 6.9 seconds. So they are copied and probed
if they are under 64 MB, and above that they keep their kind and nothing else,
with `/status` saying why. A film with no duration is better than a film with
the wrong one, and much better than four gigabytes of egress to find out.

The queue is a query rather than a table: a file with no metadata row is a file
to look at, so nothing is lost in a restart and a newly uploaded file is picked
up on its own. A file that cannot be parsed gets a row saying why, or it would
be read again on every pass forever.

**ffprobe is required**, and the image ships a statically linked one we build —
no package manager, no shell, one more layer. Photos are read in pure Go and
straight off the blob, so a photo in a bucket costs a few kilobytes rather than
the whole file. Audio and video go through ffprobe, which needs a local file, so
those are spooled to the data directory and removed afterwards.

### What the blob store looks like

An object is stored under `<kind>/<year>/<month>/<day>/<id>.<ext>` — for
instance `image/2026/09/15/K3XR…Q7.jpg`. The kind is one of `image`, `video`,
`audio`, `document` or `other`, and both it and the extension are inherited from
the name the file was uploaded with; the date is the day it was uploaded.

**The database is still what names your files**, and the layout does not change
that: nothing reads a key back, and a file whose name lied about its type stays
filed under the wrong kind. It is best effort, for one situation — you have lost
the database and are looking at the data directory. Without it you would be
looking at a hundred thousand files called nothing.

## Resumable uploads

A `PUT` is all or nothing. A four-gigabyte video uploaded from a phone on a
mobile connection restarts from zero on every drop, forever, and no amount of
retry logic in the client changes that — HTTP has no answer, since RFC 9110 says
a server should *reject* `Content-Range` on a `PUT`.

[tus](https://tus.io) is the answer this server implements, at `/tus/`, behind
the same credentials and the same failed-login limit as WebDAV and mounted only
when they are set. `POST` creates an upload, `HEAD` says how much of it arrived,
`PATCH` appends from there, `DELETE` abandons it. The file appears in the tree
when the last chunk lands, and it is the same file a `PUT` would have made,
ETag included.

Two things are worth knowing before pointing something at it:

- **The destination is the `filename` in `Upload-Metadata`**, and it is a path
  rather than a name: `holiday/clip.mp4` lands in `holiday`, which has to exist.
- **An upload you abandon is collected after twelve hours**, and the deadline is
  in `Upload-Expires` on every response so a client never has to guess. That is
  shorter than the day after which S3 abandons a multipart upload of its own
  accord, so the two cannot disagree about what is still there.

Deferred length is not supported: `Upload-Length` is required when the upload is
created. Every client this is for knows how big the file is, and a server that
accepts an upload of unknown length has to invent a rule for when it ended.

### Orphaned blobs

A write puts the bytes down before the row, and takes a fresh blob key every
time, so that a failed overwrite cannot destroy the content it was replacing.
The price is that every overwrite leaves the previous blob behind. A sweep runs
in the background — in the same process, as everything here does — and deletes
blobs no row points at.

Two rules make it safe rather than dangerous:

- **A grace period.** A blob with no row may be a write still in flight, so
  nothing younger than `STRATUS_GC_GRACE` is touched.
- **It refuses an empty index.** A database that references no blobs at all,
  next to a store with objects in it, is far more likely to be a database
  pointed somewhere new than a library somebody emptied. It logs and does
  nothing.

**Generated files live under a `derived/` prefix in the same store**, and the
sweep understands them: a thumbnail has no row of its own, so it is garbage
exactly when the file it was made from is. One rule collects both, including
after an overwrite, which leaves the old blob orphaned *and* its thumbnails
filed under a key nothing will look for again.

That prefix is worth knowing about beyond tidiness. Deleting everything under it
is safe — the pictures are regenerated the next time something asks — and on S3
it is where a lifecycle rule or a backup policy would treat derived data
differently from originals.

It runs daily and leaves anything written in the last hour alone. Both are
`STRATUS_GC_INTERVAL` and `STRATUS_GC_GRACE` if you ever need them: `0` for the
interval turns the sweep off, and the grace refuses to be `0` at all, since a
sweep with no grace can take an upload whose row has not landed yet. The
defaults are the answer for a normal install, which is why they are down here
and not in the table above.

The two backends also clean up after themselves when they open: the disk one
empties its reserved directory of interrupted uploads, and the S3 one aborts
multipart uploads abandoned more than a day ago, which are invisible to a
listing and billed until something ends them.

Both keep a second place for uploads that arrive over several requests and are
meant to be resumed, which is the opposite case and is deliberately left alone
by those sweeps: a directory beside the first one for the disk backend, and a
spool under the data directory for the S3 one, where the tail of an upload waits
until it is a whole part. Nothing uses them yet — the protocol that will is
[#122](https://github.com/C0piIot/stratus-backend/issues/122) — and an empty
directory is all you will find there today.

## OpenSubsonic

Mounted at `/rest/`, with the same credentials as WebDAV and, like it, only when
they are set. The path is not a choice: every Subsonic client appends
`/rest/<method>` to the base URL it is given, so an operator types
`http://localhost:8080` and nothing else.

**Browsing works both ways**, which is not optional: half the clients browse by
tag and half by folder, and a server that answers only one of them is broken for
the other half. By tag it is `getArtists`, `getArtist`, `getAlbum` and
`getSong`, grouped by album artist so a compilation stays one album. By folder
it is `getIndexes` and `getMusicDirectory` over the **real file tree** -- the
directories are the ones you uploaded into, not folders invented from tags,
which is the part other servers have had to fix.

**Search and the home screen work too**: `search3` and `search2`, `getAlbumList2`
and `getAlbumList`, `getGenres` with `getSongsByGenre` behind it, and
`getRandomSongs`. An empty search returns the library a page at a time, which is
how a client downloads one to browse with no network. Matching is
case-insensitive **past ASCII** -- searching for `BJÖRK` finds `Björk` -- and it
is done against text the server folded rather than by asking the database,
because SQLite and PostgreSQL do not agree on what `lower()` means for a letter
with an accent on it.

`stream` and `download` serve the file that was stored, unchanged. Ranges,
conditional requests and seeking come from the same code that serves a video
over WebDAV.

What is not there yet, and it is better to know before installing a client:

- **Cover art comes from two places, and Ogg is the one gap.** A `cover.jpg`,
  `folder.jpg` or `front.jpg` beside the tracks is preferred, because finding it
  is a listing the request already did; failing that, the picture inside the
  first track is read — FLAC, MP3 and MP4 tags. Vorbis and Opus keep theirs
  base64-encoded inside a comment and are not read yet. Albums always advertise
  a `coverArt` id, and asking for one that is not there is answered as "there is
  none", which is what a client draws a placeholder for.
- **No favourites, ratings, play counts or playlists.** Those are user state,
  which means tables that do not exist. `scrobble` is not implemented, so
  nothing counts a play either.
- **No transcoding.** `maxBitRate` and `format` are ignored and the original is
  served, which is what `format=raw` asks for explicitly.
- **Four of the ten album lists are empty**, and on purpose: "most played",
  "top rated", "recently played" and "starred" are ordered by something only a
  play count or a rating could provide, and nothing records either. The shelf
  is blank rather than filled with something that is not what it says.
- **A file is in the library once the indexer has read it**, which is also how
  long it takes to appear in a folder listing. That is the same rule for both
  views, so they cannot disagree.

Both authentication schemes are accepted: `p` carrying the password, and `t`
carrying `md5(password + salt)`, which is what the [credentials](#credentials)
section is about. Failed logins share one limit with WebDAV rather than having
their own: it is the same single password, so alternating surfaces must not
double an attacker's budget of guesses.

Two properties of the protocol are worth knowing before reading a log. A
response is **XML** unless `f=json` asks otherwise, and an **error arrives
inside an HTTP 200** with a code in the body. A 404 from `/rest/` therefore
means something else entirely: the surface is not mounted, because there are no
credentials.

## Web UI

At the root, with the same credentials as everything else and, like the other
surfaces, only when they are set: with none configured `/login` is a 404 rather
than a form for a user who does not exist.

What it does today is sign you in, walk the tree, hand you a file, take one
back, make a folder to put it in, and rename or delete what is there. The
calendar waits for CalDAV to exist. It is a convenience for when reaching for
rclone or DAVx5 is overkill, and it consumes the same internals the protocol
handlers do — it will never grow a private JSON API of its own.

**One URL per directory, and the same one per file**: `/files/photos/2026` is a
page, `/files/photos/2026/img.jpg` is the picture. Opening a file downloads it
rather than rendering it in the page — this origin serves the UI, and a file
somebody uploaded is not the UI's to display inside it. Downloads go through
`http.ServeContent`, so ranges, conditional requests and resuming a half-finished
download behave exactly as they do on the streaming surface.

**Uploading replaces**, exactly as a `PUT` over WebDAV does: a file whose name is
already in that folder is overwritten, and the blob it leaves behind is swept up
later like any other. The files stream from the browser straight into storage
rather than being spooled to a temporary file first, so the size limit is your
disk.

**A photograph in a listing shows a picture of itself**, made the first time
somebody looks at the folder and kept as a derived blob the sweep collects. The
URL carries the file's ETag, so a browser caches it forever and a new upload
gets a new one. **HEIC and video included**: JPEG and PNG are decoded in Go, and
what Go cannot read goes to the ffmpeg in the image, which is also what turns
the first second of a video into a frame. A photograph taken on a phone held
upright comes out upright, whichever half of the pipeline read it: ffmpeg
applies the rotation in a HEIC or a video, and the JPEG path reads the EXIF tag
and turns the picture itself. Only what this build can decode is offered an
image — camera raw and AVIF are not — so a listing is never a wall of broken
pictures. They load as
you scroll, and the server decodes a few at a time, because opening a folder of
five hundred photographs should not ask for five hundred at once.

**A folder arrives a hundred rows at a time.** The listing is paged by a cursor
rather than by a page number, so opening a folder costs the same whether it
holds ten files or a hundred thousand, and nothing is repeated or skipped when
somebody uploads into it while you are reading. The rest of the folder loads as
you reach the bottom of it; with JavaScript turned off the same thing is a link
at the end of the page that goes to the next one.

**`/status` says how much of the library has been read.** Metadata is extracted
in the background, and on a first pass over a library that somebody has just
pointed the server at, the only honest answer to "is it done yet" is a number:
how many files there are, how many have been read, how many are waiting and how
many could not be read at all. The page refreshes itself, and a file the
indexer has not reached yet is marked in the listing as well — which is
something to see during that first pass and nothing the rest of the time.

**Deleting asks first and then means it.** There is no trash bin: the row goes,
and the blob behind it is swept up afterwards, so the page in between is the only
chance to have not meant it. Deleting a folder takes everything inside it.

**Renaming a folder takes everything inside it**, from the UI and from a WebDAV
`MOVE` alike. It is a rewrite of every path underneath, done in one statement
inside one transaction, so it either all moves or none of it does — and no bytes
move at all, because a path is a column and a blob has no idea what it is
called. Renaming a folder of ten thousand photos costs the same as renaming one
file.

**The session is a signed cookie rather than a row in a table.** The value says
who it is for and when it expires, signed with a key derived from the configured
username and password. Three consequences, in the order they are likely to
surprise:

- **Changing the password — or the username — signs every browser out.** That is
  the revocation a stateless session has, and it is why the key comes from the
  credentials rather than from a random secret.
- **Restarting the container does not.** A session outlives the process that
  issued it.
- **Signing out clears the browser's cookie, and that is all it can do.** There
  is no server-side record to delete, so a cookie copied elsewhere stays usable
  until it expires. Sessions therefore last **seven days and are not renewed on
  use**: that ceiling is the only bound this design has.

The cookie is `HttpOnly` and `SameSite=Lax`, and `Secure` whenever the request
arrived over HTTPS — directly, or through a proxy that says so with
`X-Forwarded-Proto`. Not `Secure` unconditionally on purpose: on a plain
`http://box.lan:8080` install the browser would store the cookie and never send
it back.

CSRF is that `SameSite=Lax` plus the standard library's
`http.CrossOriginProtection`, which refuses a state-changing request the browser
itself reports as cross-site. Bootstrap and htmx are embedded in the binary
rather than pulled from a CDN, so every page is served under `default-src
'none'` — `'self'` for styles, scripts and the one request the listing makes for
its next page — and nothing is fetched from anywhere else at all, which is also
what lets the UI work on a network with no route to the internet.

## Pluggable backends

Two seams, and only two:

- **Blob storage** — `disk` and `s3`, both implemented and both passing the same
  conformance suite.
- **Metadata database** — `sqlite`, `postgres` and `mysql`, all three pure Go.
  No driver-specific SQL leaves its driver package, and all three pass the same
  conformance suite.

## Quickstart

The image is published for amd64 and arm64, so Docker is the only thing you
need:

```sh
mkdir -p data
docker run --rm -p 8080:8080 \
  --user "$(id -u):$(id -g)" \
  -v "$PWD/data:/data" \
  -e STRATUS_USERNAME=edu -e STRATUS_PASSWORD='choose one' \
  ghcr.io/c0piiot/stratus-backend:main
```

The backend listens on <http://localhost:8080>, and that is a working WebDAV
server at `/dav/` — mount it with the `rclone` line above.

`--user` and a directory you own are not decoration: the container runs as a
non-root user and refuses to start rather than come up healthy and fail on your
first upload.

`:main` is the head of the default branch and moves with it. There are no
releases yet, so a digest is the only way to hold still.

### From a clone

For development, or to run a build of your own. Docker is still the only
prerequisite — Go is never installed on the host, the toolchain runs in a
container.

```sh
make up        # build the image and start the backend
make health    # -> healthy
make logs      # follow the JSON logs
make down      # stop, keeping your data
```

`make help` lists every target. That gets you a server with nothing to serve:
WebDAV is only mounted once there are credentials, so set `STRATUS_USERNAME` and
`STRATUS_PASSWORD` in `.env` before mounting anything.

## Configuration

Every setting has a default and an env var. Copy `.env.example` to `.env`, or
pass overrides on the command line:

```sh
make up STRATUS_PORT=9000 STRATUS_DATA_PATH=/srv/stratus
```

| Variable | Default | Meaning |
|---|---|---|
| `STRATUS_PORT` | `8080` | host port the backend is published on |
| `STRATUS_DATA_PATH` | `./data` | host dir for blobs, the database and temporary files |
| `STRATUS_UID` / `STRATUS_GID` | invoking user | user the container runs as |
| `STRATUS_LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error`; anything else refuses to start |
| `STRATUS_STORAGE_DSN` | `file://<data>/blobs` | blob backend; the scheme picks it |
| `STRATUS_DB_DSN` | `sqlite://<data>/stratus.db` | metadata backend; likewise |
| `STRATUS_USERNAME` | unset | the single user |
| `STRATUS_PASSWORD` | unset | the single user's password |

### Blob storage

One DSN, and its scheme selects the backend:

```
file:///data/blobs
s3://KEY:SECRET@s3.eu-west-1.amazonaws.com/bucket?region=eu-west-1
s3://KEY:SECRET@s3.lan:9000/stratus?tls=false
```

The server writes, reads back and removes one object at startup, so wrong
credentials or a bucket it cannot write to stop the process instead of surfacing
on your first upload. A DSN carries secrets, so it is redacted everywhere it is
printed.

### Metadata database

```
sqlite:///data/stratus.db
postgres://user:pass@db.lan:5432/stratus?sslmode=require
mysql://user:pass@db.lan:3306/stratus
```

MySQL is 8.0.19 or newer, for the upsert syntax the driver uses. MariaDB is a
different database wearing the same name and is not what it is tested against.

Migrations run at startup: a self-hosted binary should not ask you to press a
button after an upgrade. Rolling *back* to an older image is refused rather than
attempted, because a schema from the future is not something to guess at.

Until the first tagged release the schema is rewritten rather than migrated: the
history is one initial migration and stays that way while it changes. A database
made by an earlier `:main` image is therefore refused by that same guard, and the
answer is to delete it rather than upgrade it.

SQLite takes no DSN parameters. WAL, `foreign_keys` and `busy_timeout` are
correctness requirements for a server, not preferences, so they are set for you.

### Credentials

```sh
STRATUS_USERNAME=edu STRATUS_PASSWORD='...' make up
```

Setting one without the other refuses to start.

The password is held as configured rather than hashed, which is a deliberate
trade: OpenSubsonic's token authentication is `md5(password + salt)`, and a
server that only holds a hash cannot compute it. Hashing would mean one protocol
behaving differently from the rest. The exposure is the process environment,
which already carries the S3 secret key and the database password; it is never
written anywhere.

`STRATUS_DATA_PATH` is a bind mount, not a named volume: your library stays on
your own filesystem, and the container runs as the user that owns it. The server
verifies the directory is writable at startup and exits with a clear error if it
is not.

## Development

`make help` lists every target. The toolchain runs in a container by default and
switches to a native `go` automatically when one matching `go.mod` is on PATH, so
the same commands work on a laptop without Go and on a CI runner with it.

```sh
make ci          # everything CI runs
make test        # unit tests
make test-race   # under the race detector (Debian image: -race needs cgo)
make test-s3     # the storage conformance suite against a throwaway Silo
make test-db     # the metadata conformance suite against PostgreSQL and MySQL
make cover       # coverage, against a floor per package
make deps        # the modules in the binary, against deps.allow
make lint        # golangci-lint, version pinned in .golangci-version
make smoke       # build the image and assert its runtime properties
make smoke-cover # the same suite against an instrumented twin, for coverage
make demo        # put the demo media into a running instance
make env         # show the resolved toolchain
```

`make smoke` is the part worth knowing about: it asserts the image is static and
non-root with no shell, that it survives a read-only root filesystem with all
capabilities dropped, and that a data directory it cannot write to makes the
server refuse to start rather than come up healthy and fail on the first upload.

`deps.allow` lists every module linked into the binary — 33 of them today, from
six direct dependencies. The rest arrive by transit: `minio-go` brings an INI
parser for an AWS credentials file this server never reads, and a YAML parser
besides. None of that is fatal, and all of it arrived without anybody deciding.
The smoke suite compares the list against the shipped binary, so a new module is
a line in a diff rather than a surprise: when the build fails, `make deps-update`
rewrites the file and the diff says what came in. Names without versions, because
a gate that failed on every weekly version bump would be switched off by the
second month.

`make cover` holds each package to a floor listed in `scripts/coverage.sh`, set
at the number reached the day it was added so it can only go up. A single total
would say nothing useful: `internal/storage/s3` measures 15% or 90% depending on
whether Silo is running, which is also what makes the floor catch a conformance
suite that skipped instead of running.

`make smoke-cover` runs the container suite a second time against a twin of the
image built with `go build -cover`, and writes `coverage-smoke.out`. The shipped
image is never instrumented, and the assertions about the artefact itself — its
size, its layers, its lack of a shell — stay measured on the one that ships. It
answers what unit coverage structurally cannot: `cmd/stratus` reads 0% there and
100% here, because flags and exit codes are only reachable by running the
binary. The two profiles are kept apart rather than merged, since a floor an
end-to-end run can satisfy has stopped being a statement about unit tests.

CI publishes `ghcr.io/c0piiot/stratus-backend:main` on every merge, multi-arch,
and — where a repository sets a `FLY_APP` variable — deploys that exact digest to
a test instance described by [`fly.toml`](fly.toml). It is one job at the end of
a pipeline that has already run the linters, the race detector, both conformance
suites, the coverage floors and the container suite, so nothing reaches it that
has not been through all of them. Anywhere `FLY_APP` is unset, including every
fork, the job does not run.

That instance is deliberately disposable, and deliberately open: the smallest
machine Fly sells, **no volume**, and **the password is the version string in
the page footer** — the short sha of the commit it is running, which anybody can
read off this repository. It is the version alone: the build timestamp beside it
is not part of it. Three things follow, and none of them is an accident:

- **Anyone can sign in and upload to it.** It is a demo, it holds nothing but
  the demo, and it is not a place to put anything of yours.
- **Every deploy changes the password**, so every client and every browser
  session is logged out by the next merge to `main`. Reconfiguring a Subsonic
  client after a merge is the cost of not having a secret to manage.
- **It sleeps when nobody is looking**, and is suspended rather than stopped so
  that it comes back holding what it held. This is the setting that decides
  whether somebody arriving at a quiet moment finds the demo or finds nothing.
- **It wipes itself every hour**, and on every deploy. That is what makes an open
  instance not worth abusing: whatever anybody leaves there — including you — is
  gone within the hour, and the demo media is put back.
  `.github/workflows/demo-reset.yml` destroys the machine and lets a deploy make
  a new one, without changing a line of what it runs.

  Destroying it is the point rather than an implementation detail. A deploy
  replaces the image and leaves the disk under it, and so does stopping the
  machine and starting it again: both were believed to wipe and neither does,
  which the demo demonstrated by spending two days serving a database from
  before the schema the binary expected.

Which is why the deploy ends by seeding it: `scripts/seed-demo.sh` fetches a
4.4 MB bundle of freely licensed media — five photographs with their EXIF, two
tagged tracks with a cover, fifteen seconds of video — and puts it in over
WebDAV, then asks the server what it did with it: the files read back, the
indexer found the artist, the album has a cover and the video answers a range
request. It is the only check here that runs against something deployed rather
than against an image on the build machine, and `make demo BASE=…` runs the same
thing against a local instance.

The bundle is a release asset rather than a directory in this repository,
because the whole clone is 6.6 MB and carrying the media would double it for
everybody, forever. `scripts/demo/SHA256SUMS` pins the bytes and
`scripts/demo/CREDITS.md` carries the attribution.

One thing that demo does not show, and it is worth knowing which: the video
**downloads rather than plays**, because the UI serves files as attachments on
purpose. What it does show is the file tree with thumbnails, downloads, and the
whole music library in a Subsonic client.

`.golangci.yml` uses `depguard` to enforce the architecture rules from
[`CLAUDE.md`](CLAUDE.md), so a driver type leaking out of its package is a failed
build rather than a note in a document.

## Container

Multi-stage build, `distroless/static:nonroot` runtime, about 27 MB. The Go
binary is most of it at 21 MB, beside 1.7 MB of `ffprobe`, 3.9 MB of `ffmpeg`
and a base under one megabyte. It grew 4 MB with the web UI: `html/template`
costs about three of those and the embedded Bootstrap a third of one, with htmx
a further 50 KB, which is what a page rendered by the standard library and
served from the binary costs.

**Both FFmpeg tools are built here rather than taken off the shelf**, in
`build/ffprobe/Dockerfile` and `build/ffmpeg/Dockerfile`. A general-purpose
static build is 128 MB and carries every decoder, encoder, filter and scaler
FFmpeg ships. Ours carry what Stratus uses and nothing else, which is two
different lists: `ffprobe` runs `-show_format -show_streams` and never decodes a
frame, so it needs the demuxers for the formats indexed; `ffmpeg` decodes the
formats Go cannot — HEIC, which is what a phone records, and a frame out of a
video — and scales them, leaving the JPEG to be written in Go.

Each recipe asserts itself while it builds, and `ffmpeg`'s does it by decoding a
committed HEIC and checking the pixels come out the right size. That is the
assertion worth having: every codec list can be right while the binary still
cannot read the format somebody uploads. `scripts/smoke.sh` then measures both
binaries against a budget, because a build that quietly stopped being trimmed
would otherwise show up as a mystery rather than a number.

The base stayed distroless rather than becoming alpine to get these tools — the
image still has no shell and no package manager, and the smoke suite asserts it.

The container runs non-root with a read-only root filesystem, all capabilities
dropped and `no-new-privileges`. The healthcheck is the binary probing itself
(`stratus -healthcheck`) since distroless ships no shell or curl.

### Two health endpoints, and which is which

`GET /healthz` is **liveness**: the process is up and serving. It touches no
dependency, and that is deliberate — it is what the container healthcheck asks,
and restarting the container does not fix a database on another host. A
healthcheck that failed on a dependency outage would turn one broken dependency
into a restart loop, and with `restart: unless-stopped` an endless one.

`GET /readyz` answers the question an operator actually has, and drives nothing:

```
database: ok
storage: ok
```

`503` if either does not answer, `not configured` for an install with no
credentials — which is a legitimate state, not a fault, so it is still a `200`.
The reason a check failed is logged with the dependency named and never put in
the response: a driver error can carry the host it could not reach, and a DSN is
not printed verbatim anywhere else either.

Both are unauthenticated. What they disclose is that two backends respond, never
a name, a DSN or a byte of content.

## Status

Single user, and usable from a WebDAV client today: files go in and come out
over `/dav/`, with metadata extracted in the background and orphaned blobs swept
up.

Working now:

- Both pluggable seams — disk and S3 for blobs, SQLite, PostgreSQL and MySQL
  for metadata — each with a conformance suite every one of its drivers
  passes.
- WebDAV, behind HTTP Basic with a global limit on failed logins.
- OpenSubsonic: browsing by tag and by folder, search, the album lists a home
  screen is made of, and streaming -- over both of the protocol's
  authentication schemes and sharing that same limit, with cover art from
  beside the music or out of the tags. No user state, no transcoding, and no
  client has been tried against it yet.
- EXIF, audio tags and video probing, indexed in the background and started by
  the upload itself, with a page saying how far it has got.
- A web UI: sign in, walk the tree, download a file, upload one, make a folder,
  rename and delete. A signed-cookie session and a CSP that allows nothing but
  the binary's own assets.
- A request log, migrations applied at startup, and a container asserted from
  the outside by 84 smoke checks.

Not there yet: CalDAV and sharing --
and on the music side, anything that remembers what the user did. Work
and the decisions behind it are tracked on the
[Stratus project board](https://github.com/users/C0piIot/projects/2), where
`Priority` says when and the `decision` label says what still needs a call.

## License

MIT — see [LICENSE](LICENSE).
