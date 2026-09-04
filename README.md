# stratus-backend

The Stratus server: one Go binary, one container, no sidecars.

Stratus is a self-hosted personal cloud for photos, calendar, music and video.
Instead of shipping its own API and a client app per platform, it speaks
protocols your existing apps already understand.

## Protocols

| Protocol | Use | Works with | Status |
|---|---|---|---|
| WebDAV | files, photo backup, sync | rclone, Finder, Nautilus, FolderSync | **works** |
| HTTP range | audio/video streaming | browsers, VLC, mpv | **works** |
| CalDAV | calendar | DAVx5, Thunderbird, iOS/macOS | next |
| OpenSubsonic | music | Symfonium, Substreamer, DSub, Feishin | next |
| Web UI | log in, browse, download | any browser | planned |
| CardDAV | contacts | DAVx5, Thunderbird | planned |
| DLNA / UPnP-AV | TVs, set-top players | | planned |

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
one there. `/healthz` logs at debug, because the container asks every thirty
seconds and three thousand lines a day of nothing is not a log.

### Media metadata

Every file that arrives is read once for what it can say about itself: when a
photo was taken, its dimensions and orientation, where it was taken, the camera;
the duration, artist, album and track of a recording; the codec and dimensions
of a video. Without it a library is a pile of files — there is no gallery by
date and no music browsing.

It runs in the background, in this process, and `STRATUS_INDEX_INTERVAL=0` turns
it off. The queue is a query rather than a table: a file with no metadata row is
a file to look at, so nothing is lost in a restart and a newly uploaded file is
picked up on its own. A file that cannot be parsed gets a row saying why, or it
would be read again on every pass forever.

**ffprobe is required**, and the image ships a statically linked one — no
package manager, no shell, one more layer. Photos are read in pure Go and
straight off the blob, so a photo in a bucket costs a few kilobytes rather than
the whole file. Audio and video go through ffprobe, which needs a local file, so
those are spooled to the data directory and removed afterwards.

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

## Pluggable backends

Two seams, and only two:

- **Blob storage** — `disk` and `s3`, both implemented and both passing the same
  conformance suite.
- **Metadata database** — `sqlite` and `postgres`, both pure Go. No
  driver-specific SQL leaves its driver package, and both pass the same
  conformance suite.

## Quickstart

Docker is the only prerequisite — Go is never installed on the host, the
toolchain runs in a container.

```sh
make up        # build the image and start the backend
make health    # -> healthy
make logs      # follow the JSON logs
make down      # stop, keeping your data
```

The backend listens on <http://localhost:8080>. `make help` lists every target.

That gets you a server with nothing to serve: WebDAV is only mounted once there
are credentials, so set `STRATUS_USERNAME` and `STRATUS_PASSWORD` in `.env`
before mounting anything.

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
| `STRATUS_LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |
| `STRATUS_STORAGE_DSN` | `file://<data>/blobs` | blob backend; the scheme picks it |
| `STRATUS_DB_DSN` | `sqlite://<data>/stratus.db` | metadata backend; likewise |
| `STRATUS_USERNAME` | unset | the single user |
| `STRATUS_PASSWORD` | unset | the single user's password |

### Blob storage

One DSN, and its scheme selects the backend:

```
file:///data/blobs
s3://KEY:SECRET@s3.eu-west-1.amazonaws.com/bucket?region=eu-west-1
s3://KEY:SECRET@minio.lan:9000/stratus?tls=false
```

The server writes, reads back and removes one object at startup, so wrong
credentials or a bucket it cannot write to stop the process instead of surfacing
on your first upload. A DSN carries secrets, so it is redacted everywhere it is
printed.

### Metadata database

```
sqlite:///data/stratus.db
postgres://user:pass@db.lan:5432/stratus?sslmode=require
```

Migrations run at startup: a self-hosted binary should not ask you to press a
button after an upgrade. Rolling *back* to an older image is refused rather than
attempted, because a schema from the future is not something to guess at.

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
make test-s3     # the storage conformance suite against a throwaway MinIO
make test-db     # the metadata conformance suite against a throwaway PostgreSQL
make cover       # coverage, against a floor per package
make lint        # golangci-lint, version pinned in .golangci-version
make smoke       # build the image and assert its runtime properties
make env         # show the resolved toolchain
```

`make smoke` is the part worth knowing about: it asserts the image is static and
non-root with no shell, that it survives a read-only root filesystem with all
capabilities dropped, and that a data directory it cannot write to makes the
server refuse to start rather than come up healthy and fail on the first upload.

`make cover` holds each package to a floor listed in `scripts/coverage.sh`, set
at the number reached the day it was added so it can only go up. A single total
would say nothing useful: `internal/storage/s3` measures 15% or 90% depending on
whether MinIO is running, which is also what makes the floor catch a conformance
suite that skipped instead of running.

`.golangci.yml` uses `depguard` to enforce the architecture rules from
`CLAUDE.md`, so a driver type leaking out of its package is a failed build rather
than a note in a document.

## Container

Multi-stage build, `distroless/static:nonroot` runtime. About 145 MB on amd64
and 115 MB on arm64, and nearly all of it is one file: the statically linked
`ffprobe` is 128 MB on amd64, the Stratus binary 17 MB and the base under a
megabyte. The base stayed distroless rather than becoming alpine to get one
tool — the image still has no shell and no package manager, and
`scripts/smoke.sh` asserts it.

The container runs non-root with a read-only root filesystem, all capabilities
dropped and `no-new-privileges`. The healthcheck is the binary probing itself
(`stratus -healthcheck`) since distroless ships no shell or curl.

## Status

Single user, and usable from a WebDAV client today: files go in and come out
over `/dav/`, with metadata extracted in the background and orphaned blobs swept
up.

Working now:

- Both pluggable seams — disk and S3 for blobs, SQLite and PostgreSQL for
  metadata — each with a conformance suite that both of its drivers pass.
- WebDAV, behind HTTP Basic with a global limit on failed logins.
- EXIF, audio tags and video probing, indexed in the background.
- A request log, migrations applied at startup, and a container asserted from
  the outside by 39 smoke checks.

Not there yet: CalDAV, OpenSubsonic, the web UI, thumbnails and sharing. Work
and the decisions behind it are tracked on the
[Stratus project board](https://github.com/users/C0piIot/projects/2), where
`Priority` says when and the `decision` label says what still needs a call.

## License

MIT — see [LICENSE](LICENSE).
