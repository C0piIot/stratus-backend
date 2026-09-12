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
  Bootstrap. Default to a plain form and a full page render.
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
  assets rather than linking a CDN is worth: `style-src 'self'` and
  `script-src 'self'` are the whole policy, and the UI works on a network with
  no route out.
- **One URL per directory and the same one per file**, under `/files/`. A
  directory renders a page and a file hands over its bytes, because to somebody
  typing a URL they are the same thing.

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
Blobs are content-addressed where practical; the DB holds the naming.

### Metadata database — `internal/db`

Repository-style interface, hand-written SQL per driver, **no ORM**.
Drivers: **sqlite** via `modernc.org/sqlite` and **postgres** via `pgx`, both
pure Go and both passing the same conformance suite. MySQL was considered and
left out on purpose — it is the only genuinely different dialect, which makes it
the best validator of the port and the most expensive to keep.

Hard rule: no driver-specific SQL or types leak outside the driver package.

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
- **Inbound adapters** (`dav`, `subsonic`, `web`) translate protocol bytes into
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
- `minio-go` for S3 (much lighter than `aws-sdk-go-v2`).
- Media processing: **FFmpeg is a requirement, not an optional extra.** Without
  it a track has no duration and a video no dimensions, and half a media library
  is worse than an honest refusal to start. The image carries two statically
  linked tools copied into the same distroless base rather than switching to one
  with a package manager.

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
    out, and the audio encoders arrive with transcoding.

  Each recipe asserts what it was asked for while it builds, and ffmpeg's also
  decodes a committed HEIC and checks the byte count of the scaled pixels. That
  is the assertion that matters: a codec list can be complete while the binary
  still cannot read the format somebody uploads, because HEIF is read through
  the mov demuxer and no flag name says so.
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
  prebuilt bundle both, byte for byte as published and with the checksums
  recorded beside the `//go:embed`; htmx is not, and waits for a page that needs
  it. The version is in the asset path, which is what lets the cache header say
  `immutable`.

  It costs 4 MB of binary -- three of them `html/template`, a third of one the
  vendored assets -- which is the whole reason `scripts/smoke.sh` carries a size
  budget: the number moved because a decision moved it.

