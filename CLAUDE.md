# Stratus backend

The server: one Go binary, one container, no sidecars.

The principles this obeys, the protocol surface it has to answer and the working
agreements it is held to are one level up, in the workspace repo's `CLAUDE.md`.
Claude Code reads both. What is here is the backend's own half, in the repo whose
code it describes.

## Web UI

A small server-rendered UI, built up feature by feature: log in, browse and
download files, view the calendar. It is a convenience for the cases where
reaching for rclone or DAVx5 is overkill, not the primary way to use Stratus.

Hard constraints, in the same spirit as the rest of the project:

- **No JavaScript build step.** No npm, no bundler, no transpiler, no
  `node_modules`. If a feature needs a build to work, the feature waits.
- **`html/template` from the stdlib**, served from `//go:embed`.
- **Bootstrap for CSS, vendored and embedded** -- never from a CDN. A self-hosted
  cloud must work with no outbound network, and an embedded asset also keeps the
  CSP tight.
- **No hand-written stylesheet.** Bootstrap's utility classes cover the layout we
  need. If something genuinely cannot be expressed with them, that is a
  conversation, not a new `.css` file that grows forever.
- **htmx only where it is genuinely required**, vendored and embedded like
  Bootstrap. Default to a plain form and a full page render. One page needs it
  so far: the file listing, which is paged by a cursor and extends itself as
  somebody scrolls (#138). What makes that acceptable rather than the thin end
  of a wedge is that the page works without it -- the row at the end of a page
  is a link to the next one, and htmx only turns that link into a swap -- and
  that the fragment it asks for is the same URL answering with a piece of the
  same HTML. A second endpoint, or one answering in JSON, would be the private
  API principle 2 forbids.
- The UI authenticates with its own session cookie, since the protocol surfaces
  use Basic and token auth. **The session is signed, not stored**: the value
  carries who it is for and when it expires, under an HMAC keyed by a derivation
  of the configured username and password. It lives in `internal/auth` beside
  the other two credential adapters, so the password does not leave that
  package.

  What that buys is no state and no migration, and a password change that
  revokes every session already issued. What it costs is the revocation in
  between: signing out clears the browser's cookie and there is nothing else to
  delete, so a copied cookie works until it expires -- hence a **seven-day
  ceiling that is not renewed on use**, which is the only bound the design has.
  A map in memory was the alternative: real revocation, at the price of a
  restart signing everybody out.
- CSRF is that cookie's `SameSite=Lax` plus `http.CrossOriginProtection` from
  the standard library, and it is wired **inside `internal/web`** rather than by
  the composition root. It is meaningless on the other surfaces -- a WebDAV or
  Subsonic client is not a browser and sends no cookie -- and a caller that
  forgot it would lose the defence with nothing to show for it. No synchroniser
  token, so no new form can forget to carry one.
- Every page is served under `default-src 'none'`, which is what embedding the
  assets rather than linking a CDN is worth: `style-src 'self'`,
  `script-src 'self'` and `connect-src 'self'` are the whole policy, and the UI
  works on a network with no route out.

  That third directive is the entire price of htmx, and it is not the one that
  was expected: the listing's next page is an `hx-get`, which is an
  `XMLHttpRequest`, and under `default-src 'none'` the browser refuses it before
  htmx sees anything -- a failure with no server-side symptom at all, which is
  why `scripts/smoke.sh` asserts the directive rather than trusting it. What was
  expected, `'unsafe-eval'`, is not needed: every place htmx evaluates a string
  goes through its `maybeEval`, and the `htmx-config` meta element in the layout
  turns that off. A meta element and not a line of script, since the policy
  forbids that too.
- **One URL per directory and the same one per file**, under `/files/`. A
  directory renders a page and a file hands over its bytes, because to somebody
  typing a URL they are the same thing.

  **A directory renders a hundred rows of itself and a cursor to the rest.**
  Not `LIMIT` with an `OFFSET`, which makes the database count past everything
  it skips and repeats or drops a row when somebody uploads into the folder
  mid-scroll: the cursor is the last row of the page before, and the query seeks
  straight to it. It is in the URL in the clear, because it is a path the
  browser already has -- with a letter in front of it saying which of the two
  groups the ordering had reached, since directories sort before files and a
  page boundary can fall between them.

  A file is served as an **attachment, never inline**. This origin serves the
  UI, and a file somebody uploaded is not the UI's to render inside it -- the
  content security policy above would already stop a script in an uploaded
  HTML page, and the disposition is what stops the question from arising. The
  day a preview is worth having is the day to decide where it renders, and the
  answer will not be "the same origin as the session cookie".

  The bytes go out through `http.ServeContent` over the seeker `internal/files`
  returns, so ranges, conditional requests and a resumed download are the
  standard library's, not this package's.
- **An upload streams from the socket into the blob store**, part by part, using
  `r.MultipartReader` rather than `ParseMultipartForm` -- which would spool every
  file to a temporary one first. This is where somebody puts a 4 GB video, so the
  bytes are never on this machine twice, and the size a part does not declare is
  the `-1` `files.Write` already takes.

  It **replaces** what was at that name, because a `PUT` over WebDAV does, and
  one door behaving differently from the other is worse than the surprise. The
  filename is reduced to its last element before it is used -- a directory
  upload sends a relative path and an old browser a whole Windows one -- and
  what is left is refused by `files.Write` if it is still not a path.
- **A page at `/status` reports on the indexer**, because a library is read in
  the background and a first pass over one somebody has just pointed the server
  at takes hours. It counts what is indexed, what is waiting and what could not
  be read; the numbers are a fragment htmx refreshes, and the listing marks the
  rows nothing has looked at yet. It reports and does not drive: there is no
  button here that starts, stops or hurries the indexer, because a surface that
  could would be a surface that has to be protected from being pressed twice.
- **Renaming and deleting are pages, not buttons in the row.** Each is a GET
  that asks and a POST that does: a rename needs a name typed into something,
  and a delete cannot be undone -- there is no trash bin, so the page in between
  is the only chance to have not meant it. It also keeps the listing from
  carrying two forms per row.

  A rename is a rename, not a move: the field is reduced to one path element, so
  a typed path cannot quietly carry a file across the tree. A folder with
  anything in it cannot be renamed at all -- the metadata port refuses to move a
  directory that still has descendants, and the page says that rather than the
  generic "something is in the way", which would send somebody looking for a
  thing that is not there.
- **A new folder posts to `/folders/<parent>`**, not to the listing's own URL
  with a different body. Two forms on one page mean two endpoints: telling them
  apart by what happens to be in the body -- a file part or a text field --
  would be a piece of cleverness that is wrong once and then permanent.

## Configuration

Everything through environment variables, readable from a `.env` file. No config
file format, no flags beyond `-version` and `-healthcheck`.

Each pluggable seam is **one DSN**, where the scheme selects the driver. This is
principle 3 expressed as configuration:

```
STRATUS_DB_DSN       sqlite:///data/stratus.db
                     postgres://user:pass@host:5432/stratus?sslmode=require

STRATUS_STORAGE_DSN  file:///data/blobs
                     s3://KEY:SECRET@s3.eu-west-1.amazonaws.com/bucket?region=eu-west-1
```

Credentials:

```
STRATUS_USERNAME
STRATUS_PASSWORD        held as configured, not hashed
```

Rules that follow from this:

- **A DSN may carry secrets, so it is never logged verbatim.** Redact userinfo and
  secret query parameters before any log line or error message touches one.
- **The sqlite DSN takes no parameters.** WAL, `foreign_keys` and `busy_timeout`
  are correctness requirements for a server, not operator preferences, so the
  adapter sets them. The postgres one passes its parameters through: that set is
  large, documented and legitimate, and pgx rejects what it does not know at
  connect time, which happens at startup anyway.
- **The password is held as configured, in the clear, and this was chosen rather
  than settled for.** OpenSubsonic token auth is `md5(password + salt)`, which a
  server holding only a bcrypt hash cannot compute; supporting both forms of
  configuration would have meant one protocol behaving differently depending on
  how the operator set the password up. The exposure it accepts is the process
  environment, which already carries the S3 secret key and the database password,
  and it is never written anywhere.

## Abstractions

### Blob storage — `internal/storage`

Narrow interface: `Put`, `Get` (range-capable), `Delete`, `Stat`, `List`.
Backends: **disk** and **s3**, both implemented; ftp and others later.

**A second half for writes that arrive over several calls** -- `StartUpload`,
`AppendUpload`, `UploadOffset`, `CompleteUpload`, `AbortUpload` -- because a
`PUT` is all or nothing and a phone on mobile data restarts a four-gigabyte
video from zero on every drop (#122). Required rather than an optional
interface: both backends honour it truthfully, a file growing and a multipart
upload gaining parts, and the conformance suite makes them prove it.

Two rules make that implementable on both. The offset is **a precondition, not
a seek** -- an append says where it believes the store is and is refused
otherwise -- so a retried chunk is a conflict rather than duplicated bytes, and
S3, which cannot fill a hole after the fact, is not asked to. And **the chunk
size is the backend's problem**: the s3 one spools the tail that is not yet a
whole part, rather than making a phone learn that S3 refuses a part under 5 MB.
What that spool holds is counted as accepted, so the offset a client is told is
one a restart cannot take back.

Nothing an upload has accepted is visible to `Get`, `Stat` or `List` until it
completes, which is what keeps it out of reach of the sweep in `internal/files`.
The disk backend keeps them in a second reserved directory for that reason: the
first one is emptied when the store opens, on the argument that what is in it
belongs to a dead process, and an upload somebody will resume tomorrow is the
opposite of that.

**A blob key is an opaque string the database owns**, never derived from the
path and never from a content hash. That is what would let a Nextcloud bucket be
adopted in place: its objects are already named `urn:oid:<fileid>` with the tree
in its own database, so importing one is a batch of row inserts rather than a
server-side copy of every byte. Content-addressing would spend that migration
and buy nothing back.

Opaque to the code, not to a person. `newBlobKey` files an object under
`<kind>/<year>/<month>/<day>/<id>[.<ext>]`, where the kind and the extension are
inherited from the name the file arrived with and the date is the upload's
(#123). That is best effort for exactly one reader -- somebody looking at a data
directory with no database left, who would otherwise find a hundred thousand
indistinguishable files. **Nothing parses it back.** The day something reads a
kind out of a key, the layout is a schema and changing it is a migration; and
since nothing does, a store can hold every generation of the shape at once,
including keys this project never wrote.

**No key Stratus writes is a prefix of another.** S3 holds `a` and `a/b` at
once; a filesystem cannot, so the disk backend fails the second `Put` and the
two backends would diverge on a key only one of them can take. `ValidateKey`
cannot catch it -- it sees one key, and the collision is a property of the set --
and it must not try: a rule rejecting anything not of our shape would reject an
adopted key too. The guarantee lives in the two constructors instead.
`newBlobKey` puts a random leaf at a fixed depth, and `DerivedKey` takes a
single segment and panics on anything else, which is also what keeps it the
inverse of the `parentOf` the sweep reads keys back with.

**Nor do two keys differ only in case**, and for the same reason: a
case-insensitive filesystem -- APFS, exFAT, a Windows share, all of them
plausible under `STRATUS_DATA_PATH` -- would collapse such a pair into one file
while S3 held two objects, and the second `Put` would destroy the first. The id is
RFC 4648 base32, which has no lowercase in it, and everything a caller can
influence -- the kind, the extension -- is lowercased on the way in, so the pair
is unrepresentable rather than rejected. Both properties are the same rule:
**the constructors own the shape of a key, the validator only owns what a single
key may contain.**

`internal/tus` is the protocol over that: created with `POST`, resumed from what
`HEAD` reports, appended to with `PATCH`. Hand-written rather than taken from
tusd, which arrives with a storage abstraction of its own that would sit
absurdly beside this one. Deferred length is not implemented and not advertised
-- every client this is for knows how big the file is, and accepting an upload
of unknown length means inventing a rule for when it ended.

**A directory moves with everything under it**, in one statement per driver
rather than a row at a time: a rewrite that stopped halfway would leave the rest
of the tree pointing at a parent that no longer exists. `db.ValidateMove` is
where the one rename that cannot be expressed is refused -- a directory into
itself, where the statement doing the rewriting would read rows it had already
written -- and it lives in the port so all three drivers share it.

An **upload in progress is a row**, in `internal/db`, not something held in
memory: the point of a resumable upload is that a phone can come back to it
after a tunnel or a restart, and a server that forgot where it was would make
the client start again. The row carries the offset the store has accepted and
the running SHA-256, marshalled between requests -- `crypto/sha256` can, which
is the only reason completing a four-gigabyte upload does not mean reading it
back to hash it. When a store keeps less than it read, the hash is ahead of the
object and cannot be wound back, so it is dropped and that read-back happens
after all: the expensive path, taken rarely and on purpose.

Nothing else will ever collect an abandoned upload -- the sweep cannot see one
-- so `files.CollectUploads` runs beside it on the same tick, and the deadline
it enforces is the one the client was told.

### Metadata database — `internal/db`

Repository-style interface, hand-written SQL per driver, **no ORM**.
Drivers: **sqlite** via `modernc.org/sqlite`, **postgres** via `pgx` and
**mysql** via `go-sql-driver`, all three pure Go and all three passing the same
conformance suite.

MySQL was left out until somebody wanted it (#27), and what it cost when it
arrived was the estimate: a third set of hand-written queries, and three
corrections nothing else had needed. It cannot index a `TEXT` path, so
uniqueness rides on a `path_hash` column that exists in no other driver -- the
schema shape is the driver's own business, which is why nothing compares the
three. It has no `RETURNING`, so a write that needs its row back is two
statements. And a subquery may not name the table an `UPDATE` or `DELETE` is
writing, so the guard that refuses to empty a directory goes through a derived
table with a `LIMIT` in it, which is what stops the optimiser merging it back.

Its collation default is accent- and case-insensitive, which would have made
`Photo.jpg` and `phóto.jpg` one row under a unique index; the schema pins
`utf8mb4_0900_bin`. That is the same class of silent collapse as #16, one layer
down.

Hard rule: no driver-specific SQL or types leak outside the driver package.

**The tree invariant is half SQL and half Go, and that asymmetry is a decision.**
That a directory with anything in it cannot be deleted or moved is a `NOT EXISTS`
inside each driver's statement, so it is one round trip and cannot race a
concurrent insert. That a row's parent exists is `files.requireParent` instead.
A foreign key from `(owner_id, parent_path)` to `(owner_id, path)` would be the
obvious way to move the second half down beside the first, and it is refused on
price rather than on principle:

- **It enforces half of the check.** The parent must exist *and* be a directory,
  and no constraint can see `is_dir`, so `notes.txt/inner.txt` would satisfy it.
  The Go check stays either way, which makes the constraint a second and weaker
  statement of the same rule rather than a replacement for it.
- **It would not simplify the drivers.** Telling "no such row" from "not empty"
  is already shared in `sqlutil.CheckAffected`, so the `NOT EXISTS` costs two
  lines per statement, while `ON DELETE RESTRICT` would cost a foreign-key branch
  in each driver's `mapErr` and keep `CheckAffected` regardless.
- **The root has no row.** `parent_path` is `''` at the top level and no row can
  satisfy that, so it also needs a nullable column or a self-referencing row per
  owner, and either one leaks into every query that lists the root.

What it buys against all that is a backstop in the database for a bug in the one
package that writes rows. Worth revisiting if a second writer appears.

One cost that does not count today and will: SQLite has no `ALTER TABLE ADD
CONSTRAINT`, so adding the constraint after a release means rebuilding the table,
and the `PRAGMA foreign_keys=OFF` that needs is silently ignored inside a
transaction -- which is how `db.Migrate` applies every migration. Until the first
real deployment it would simply go into `0001_schema.sql`.

## Architecture

Ports and adapters at **two** boundaries, and nowhere else. This is principle 3
stated as structure: the metadata database and blob storage are ports with
swappable adapters; everything else stays concrete.

Deliberately **not** full hexagonal architecture. The domain here is thin --
files, events and tracks are nearly flat records -- while the adapters are fat:
PROPFIND multistatus, iCalendar recurrence expansion, the Subsonic double
envelope. A ceremonial domain layer in the middle would be mostly mapping code.
Two more reasons it would not pay:

- The hot path moves `io.Reader`s from storage into an HTTP response. Mapping
  through layers either copies bytes or passes the reader through anyway, which
  means the port leaks an I/O primitive regardless. Better to be honest: the
  storage port is deliberately I/O-shaped.
- Inbound ports would be fiction. Each protocol has exactly one implementation,
  and their shapes differ so much that a shared inbound interface would end up
  anemic or become a lowest common denominator -- which is precisely the private
  API principle 2 forbids.

```
cmd/stratus/              main: flags and exit codes, nothing else
internal/app/             composition root: wiring, router, lifecycle

internal/config/          env and DSN parsing, secret redaction

internal/storage/         PORT: Storage iface, sentinels, ValidateKey
internal/storage/disk/    adapter
internal/storage/s3/      adapter
internal/storage/storagetest/   conformance suite every adapter must pass

internal/db/              PORT: Store iface, sentinels, entities
internal/db/sqlite/       adapter + migrations/
internal/db/postgres/     adapter + migrations/
internal/db/sqlutil/      plumbing both SQL adapters share, and not one line of SQL
internal/db/dbtest/       conformance suite every adapter must pass

internal/files/           cross-protocol file invariants
internal/calendar/        collections, objects, recurrence            -- not yet
internal/media/           EXIF/tag extraction, thumbnails, ffprobe
internal/auth/            credential verification, per-protocol adapters

internal/dav/             inbound adapter: WebDAV (CalDAV not yet)
internal/subsonic/        inbound adapter: OpenSubsonic
internal/web/             inbound adapter: server-rendered UI
```

### The four layers

- **Composition root** (`app`) wires everything and owns the lifecycle. Nothing
  imports it.
- **Ports** (`storage`, `db`) declare the interfaces, the sentinel errors and the
  shared validation. **Entities live here, next to the port.** A feature
  depending on the db *port* is dependency inversion working as intended, not a
  leak; the rule that matters is that no driver escapes its adapter package. A
  separate package of anemic types plus mappers would only separate two types
  that are the same thing in this project.
- **Features** (`files`, `calendar`) own the invariants that must look
  identical from every protocol. `files` exists for a concrete reason: a file is
  a database row *plus* a blob, and if `dav` and `web` each wired storage and db
  themselves they would diverge on ETag computation and on what happens when the
  blob write succeeds and the row insert fails. That pair of writes is not atomic
  and cannot be, with two independent seams -- so ordering is the mitigation:
  **blob first, row second**, which leaves a collectable orphan blob instead of a
  row pointing at nothing.
- **Inbound adapters** (`dav`, `tus`, `subsonic`, `web`) translate protocol bytes into
  feature calls and back. They are the only packages that know about HTTP status
  codes, XML namespaces or template rendering.

### Rules, enforced rather than documented

An architecture that lives only in a markdown file erodes in three months. These
are `depguard` rules in `.golangci.yml`, so the first violation fails the build:

- Inbound adapters do not import each other.
- Ports, features, `media` and `auth` do not import inbound adapters, nor `app`.
- No driver-specific import outside its own adapter package.

### Not created until something actually needs it

Restraint here is principle 3, not laziness:

- **`httpx`** for range serving and error mapping. `http.ServeContent` already
  implements RFC 7233, and each protocol's error shape differs (207 multistatus
  vs Subsonic error codes vs an HTML page). The only shared part is the
  classification, which is already the sentinel errors. Create it when two
  handlers genuinely duplicate something.
- **`music`.** There was a package pencilled in here for the library model, and
  writing the OpenSubsonic adapter showed there is nothing for it to hold. An
  album is not a row -- it is a `GROUP BY` over tags -- so the model is the
  `db.Music` port and the queries behind it, and browsing is calling them. The
  only logic above that is the id encoding and the envelope, which are the
  protocol's and belong in `internal/subsonic`. A feature package here would be
  a pass-through, and the day it stops being one (derived tables, a play count,
  a playlist) is the day to create it.
- **`photos`.** Photo backup is files plus EXIF indexing; the photo-ness lives in
  `media` and in date queries.
- **Any job framework.** The indexer is a goroutine started by `app`.

## Tech decisions

- Go, `net/http` from stdlib, **no web framework**.
- `github.com/emersion/go-webdav` for DAV/CalDAV primitives.
- `minio-go` for S3 (much lighter than `aws-sdk-go-v2`). The client, not the
  server: MinIO the server was archived in April 2026, and the conformance
  suite runs against Silo, a maintained fork of it (#116). minio-go is a
  separate project, Apache-2.0 and still released.
- Media processing: **FFmpeg is a requirement, not an optional extra.** Without
  ffprobe a track has no duration and a video no dimensions; without ffmpeg a
  photograph from a phone has no picture of itself. Half a media library is
  worse than an honest refusal to start, and both binaries are looked up at
  startup for that reason -- which is also what keeps `media.CanThumbnail` a
  function of the name, rather than something a page has to ask a generator
  about. The image carries two statically
  linked tools copied into the same distroless base rather than switching to one
  with a package manager.

  **What it is not required for is an MP4.** ffprobe has to seek inside a file,
  so probing one meant copying the blob to disk first -- on S3, downloading a
  four-gigabyte recording to learn that it is four minutes long (#48). Duration,
  dimensions, codec, rotation and date are in a box at one end of the container,
  and the storage port reads ranges, so `internal/media/mp4.go` walks to it in
  three ranged reads and never touches the frames. It is the argument the
  embedded-cover and EXIF readers already make, applied where the file is
  measured in gigabytes.

  Two rules keep that from becoming a second, worse ffprobe. **It answers only
  for `.mp4`, `.m4v` and `.mov`** -- Matroska, AVI, WMV and MPEG-TS are copied
  and probed as before, and `.m4a` is the same box structure but has nothing to
  win. And **it never guesses**: a codec whose fourcc is not in its table, a
  duration the header does not state, a box that does not parse, and the file
  goes to ffprobe. The two paths write the same column, so a test compares their
  answers against real files whenever there is an ffprobe to compare with.

  **We build both** (`build/ffprobe/Dockerfile` and `build/ffmpeg/Dockerfile`,
  published by `.github/workflows/media-tools.yml`) rather than copying
  general-purpose ones, which are 128 MB each and carry everything FFmpeg ships.
  Two recipes and not one configure run producing both: ffprobe would inherit
  decoders it has no use for and stop being 1.7 MB.

  - **`ffprobe`, 1.7 MB.** Probing is demuxer work and no frame is ever decoded,
    so the demuxer list mirrors `byExtension` in `internal/media` and a test
    holds the two together — an extension added without its demuxer fails at
    probe time in production rather than at build time.
  - **`ffmpeg`, 3.9 MB.** Only for what Go cannot decode: HEIC, which needs
    libheif and therefore cgo, and a frame out of a video. It decodes and
    scales; the JPEG is written in Go, so it emits a rawvideo frame already
    reduced rather than a full-size one. AV1 and camera raw are deliberately
    out, and the audio encoders arrive with transcoding. The transpose, hflip
    and vflip filters are in it for nothing this project writes: they are what
    ffmpeg itself reaches for when it straightens a frame the container says was
    recorded rotated.

  Each recipe asserts what it was asked for while it builds, and ffmpeg's also
  decodes a committed HEIC and checks the byte count of the scaled pixels. That
  is the assertion that matters: a codec list can be complete while the binary
  still cannot read the format somebody uploads, because HEIF is read through
  the mov demuxer and no flag name says so.
- **The queue is a query, and a write is a tap on the shoulder.** What is
  pending is a `LEFT JOIN` over the file rows -- nothing is enqueued, nothing is
  dequeued, a restart loses nothing and an import that inserts rows is picked up
  without knowing this exists. What that cost was a minute of waiting after
  every upload, so `internal/files` now takes a watcher and the composition root
  points it at the indexer.

  The notice carries no work and no promise. It wakes the loop, which runs the
  same batch over the same query, so there is one path that indexes anything;
  it holds one, so five hundred photographs arriving together are one pass; and
  it may be dropped -- by a full channel, by a process that dies -- because the
  row is already committed and the query is still the truth. The watcher takes
  no context for the same reason: the work outlives the request that caused it.

  **A media row records the validator it was extracted from.** Replacing a file
  keeps its row and its id, so without that the metadata of the bytes that are
  gone would describe the bytes that are there until somebody raised the
  extractor version -- which is what it did until #48. What this still cannot
  see is a rename that changes an extension, where the bytes, and therefore the
  validator, are the same while the kind is not.
- **What a file is, is read rather than inferred** (#146). `internal/sniff` is a
  leaf package over a 512-byte window: it answers a MIME type and a `db.Kind`,
  or it answers nothing, and nothing is the answer that sends the question back
  to the name. Three callers had been guessing separately -- the type stored on
  a row, the kind an extractor works from, and the prefix a blob key is filed
  under -- so the table lives below all three rather than in any of them.

  Two things it has to know that the standard library does not. **ISOBMFF is one
  container for a photograph, a film and a track**, and only the brand after
  `ftyp` says which, so HEIC, AVIF, MP4, QuickTime and M4A are told apart by
  four bytes at offset 8. And **MPEG-TS has no header at all** -- a sync byte at
  the start of every 188-byte packet is the whole signature, which is why the
  window is 512 bytes and why `.ts` can be a TypeScript file without being
  mistaken for a recording.

  It refuses rather than guesses: an unknown ISOBMFF brand, an ASF file that
  could be audio or video, a buffer of noise the standard library would call
  text. A wrong answer from the bytes is worse than no answer, because the name
  at least says what somebody meant.

  What this does **not** change is the listing's offer of a thumbnail, which is
  still `media.CanThumbnail` over the name alone -- a page cannot read five
  hundred files to decide what to draw. So `.mts` and `.m2ts` are in that list
  by name and `.ts` is not.
- **Thumbnails are lazy, and they are blobs.** Generated on first request rather
  than on upload, because a phone backing up five hundred photos would otherwise
  pay a decode and a resize per PUT with the client waiting -- and because lazily
  is the only path that also covers files which arrived some other way, such as a
  bucket adopted in place.

  They are kept in the blob store under `derived/<blobkey>/<size>.jpg` and not in
  a cache of their own. A cache port with disk, database and Redis backends was
  the first design and it is wrong twice: a third pluggable seam is what
  principle 3 forbids, and Redis is named in principle 1 as a thing this project
  does not have. The store already satisfies every requirement -- a container
  with no volume loses nothing, deleting one regenerates it, and the key is a
  pure function of the original's.

  **That last property is what makes them collectable.** A derived object has no
  database row and never will, so the sweep in `internal/files` would delete
  every thumbnail an hour after it was made and the lazy path would generate it
  again, forever -- a treadmill with no symptom beyond a CPU graph. The key
  carries its parent's, so one rule covers both: a derived object is garbage
  exactly when the blob it was made from is. A `derived` table with a foreign key
  was the alternative, and it buys a guarantee for a migration and a query per
  thumbnail served.

  Sizes come from a fixed ladder, because the size is part of the key and an
  arbitrary one means an unbounded set of objects nothing asks for twice.

  **What Go cannot decode goes to ffmpeg, and that binary decodes and scales and
  nothing else** (#141). It hands back a rawvideo frame already reduced to the
  size asked for -- about 360 KB against the 48 MB a full 12 MP frame would push
  through a pipe -- and the JPEG is written by the same encoder every other
  thumbnail goes through. rawvideo carries no header, so the width is the one
  the scale filter was given and the height is arithmetic that has to divide
  exactly; a frame that does not is refused rather than guessed at.

  Three things follow that are easy to get wrong later. **Rotation is ffmpeg's
  job**: it reads the matrix in the container and inserts a transpose, which is
  the only reason that filter is in the build -- without it a portrait
  recording is not a sideways thumbnail, it is a failed one. **A video costs a
  copy of the whole file**, which is what #48 removed for probing an MP4 and
  cannot remove here, because a binary opens files and seeks in them; it is paid
  once per file and size, and then the derived blob answers. And **the list of
  extensions in `decodableByFFmpeg` mirrors the decoders in the recipe**, the
  same way `byExtension` mirrors the demuxers and with the same failure mode: an
  extension added on one side only is a broken thumbnail in production, not a
  broken build.

  A JPEG's own EXIF rotation is still ignored, because that file is decoded in
  Go and never reaches ffmpeg. That is a gap of its own rather than part of
  this one.

  **Two surfaces ask for them now**, and the second one changed the shape: a
  listing in the browser offers a picture for every row this build can decode,
  which is the photo grid `open` warned about when it said generating one twice
  costs nothing but generating five hundred at once is somebody's problem. It is
  answered from both ends -- the browser loads them lazily, and a semaphore
  bounds how many are decoded at a time, because a twelve-megapixel JPEG costs
  about fifty megabytes while it is being read. Whether a file can have one is
  `media.CanThumbnail`, computed rather than stored: it is a property of the
  build, and the day the ffmpeg path lands every HEIC changes its answer without
  a byte moving.

  **A picture inside a track is read in Go rather than by ffmpeg**, and that is
  the same argument as EXIF: the storage port reads ranges, so a parser that
  seeks takes the first few kilobytes of a FLAC and stops, while ffmpeg needs a
  local file and would spool a 50 MB track out of a bucket to lift a 200 KB
  picture out of its head. It also keeps that binary honest as the thing for
  pixels Go cannot produce. FLAC's PICTURE block, ID3v2's APIC frame and MP4's
  covr atom cover a real library; Vorbis and Opus keep theirs base64-encoded in
  a comment and are not read yet.
- `golang.org/x/image` for the scaler, which the standard library has no
  equivalent of. JPEG and PNG are decoded and encoded by the stdlib; HEIC and
  video frames need the ffmpeg above, so a format is either read in-process or
  refused honestly, never read badly.
- **Principle 5 is a gate, not an intention.** `deps.allow` lists every module
  linked into the binary and `scripts/smoke.sh` checks it against the shipped
  one, so a transitive arrival is a line in a diff. `depguard` is the other half
  and answers a different question: it forbids a handful of imports by name in
  *our* code, while this counts what actually ends up in the artifact --
  including the parsers a client library drags in and the `github.com/pkg/errors`
  that `depguard` forbids us and `imagemeta` ships anyway.

  Module names without versions: Dependabot bumps one every week, and a gate
  that failed on each would be turned off by the second month. A bump that drags
  something new in still shows, because the list changes.
- Config over convention: sane defaults, everything overridable by env var.
- Web UI: `html/template`, Bootstrap and htmx vendored and `//go:embed`ed. No
  JavaScript toolchain, no custom CSS. Bootstrap 5.3.8 is in, CSS and its
  prebuilt bundle both, and htmx 2.0.10 beside it, byte for byte as published
  and with the checksums recorded beside the `//go:embed`. Each carries its own
  version in its asset path, which is what lets the cache header say
  `immutable`.

  It costs 4 MB of binary -- three of them `html/template`, a third of one the
  vendored assets and 50 KB of that htmx -- which is the whole reason
  `scripts/smoke.sh` carries a size budget: the number moved because a decision
  moved it.

