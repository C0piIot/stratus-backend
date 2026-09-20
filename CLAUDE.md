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
- **One script of our own, and this is the conversation about it.** The share
  page hands back a URL to send somebody, and copying it by hand out of a text
  field is the kind of small misery that makes a feature feel unfinished. There
  is no way to avoid script: the policy below is `script-src 'self'` with no
  `'unsafe-inline'`, so not even an `onclick` attribute would run, which is why
  `internal/web/static/stratus/copy.js` is a file rather than three characters
  in the markup.

  Twenty lines, no build, no library. **It degrades by construction**: the
  button ships with `d-none` and the script is what removes it, so a browser
  with no JavaScript shows the field alone, which is what the page was before.
  Its URL carries the build version as a query, since it has no version of its
  own and the `immutable` header has to stay true.

  This is not permission for more. The next feature that wants script gets the
  same paragraph written about it, or it gets a form.
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
- **A share link is the ordinary URL with a signature on it, and that is the
  whole of the design** (#169). `internal/auth/share.go` is the fourth
  credential adapter beside the other three, derived from the same password with
  a context string of its own, so one password change revokes every link and
  every session together and neither value can be read as the other.

  It is not a surface. `/files/<path>` already streams a file's bytes through
  `http.ServeContent` -- ranges included, which is what casting turns on -- and
  renders a folder as HTML for the most universal client there is, so a link is
  that URL with `?k=` on the end and a browser is told nothing new. **WebDAV was
  the obvious home and it is the wrong one**: a signed `/dav/` URL serves the
  same bytes, but its listing is `PROPFIND`, which no browser issues and no
  receiver speaks, and no WebDAV client will put a query string on every request
  -- so a folder link there would open onto nothing.

  **Read-only is which gate the route goes through, not a check somebody
  remembers.** `readable` wraps the routes that read and accepts either a
  session or a link; `signedIn` wraps the routes that write and accepts only a
  session. A new writing route is read-only-safe by default, because the wrong
  gate is the one that has to be chosen on purpose.

  Three details that are easy to get wrong and are all tested: the signature is
  checked against **the path the request asked for** rather than the one the
  token names, and a subtree match has to be on `path + "/"` or a link to
  `album` opens `album2`; every link a shared page emits carries the token, the
  thumbnails included, or the listing is a grid of broken images; and a link
  that was offered and refused is `403` rather than a redirect, because somebody
  sent a dead link needs to be told and a receiver needs a status code.

  **A link works on `/dav/` too, for a plain read.** The app speaks WebDAV and
  nothing else, and what it needs is a URL a Chromecast can fetch -- a receiver
  gets the media itself and cannot send an `Authorization` header. Making it
  derive the browser surface's URL instead would have worked and is the worse
  trade: `/files/` is the web UI, the thing most likely to change shape, while
  `/dav/` is a mount that will not move, and a client should depend on the
  protocol surface.

  It widens no authority. The signature already authorises reading that path,
  and a `GET` there is the same bytes through the same `ServeContent` with the
  same ranges; what changes is the address it can be presented at. `GET` and
  `HEAD` only -- not `PROPFIND`, so a folder link opens nothing there and stays
  what it is, something for a person on the surface that renders HTML. The gate
  is `internal/dav/signed.go`, and `auth.Basic` now passes through a request
  that already carries a user, which is safe for the reason the context key is
  unexported: nothing outside `internal/auth` can claim to be somebody.

  The owner authorises the read and is not shown for it: a shared page carries
  no name, no Sign out and no way back to the share's own root -- the
  breadcrumbs start at the share, because a trail that climbed higher would
  offer a door the link does not open and one that stopped at the current
  folder would strand a visitor two levels down.

  **A token is a pure function of the credentials, and that is deliberate.**
  Anything holding the username and the password can compute the same key and
  mint the same link, `stratus-app` included -- which is how the app gets a URL
  a Chromecast can fetch without asking this server for one, and therefore
  without the private JSON endpoint principle 2 forbids. There is no call to
  make: the answer was already derivable from what the client has.

  What that costs is a format shared across two repositories, so it is written
  down rather than left to be read out of the code:

      key     = HMAC-SHA256(key: password, msg: "stratus share link v1\x00" + username)
      payload = "k1" "." b64url(owner) "." b64url(path) "." ("f" | "d") "." unix-expiry-or-0
      token   = payload "." b64url(HMAC-SHA256(key: key, msg: payload))

  where `f` is one file and `d` is a path and everything under it, and the
  expiry is `0` for a link with none. The `k1` is what makes changing the shape
  safe: a client minting an older one is refused rather than misread, which is
  the whole reason the version is inside the signature.

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

  **The same trap caught us a second time, on `style-src`.** No `'unsafe-inline'`
  beside it means a browser drops every `style` attribute, silently and with
  nothing on the server to see -- and two templates were setting sizes that way.
  Bootstrap's progress bar takes its width from one, so a fully indexed library
  rendered as "100%" painted in a sliver a few characters wide, which is how it
  was finally noticed. The answer is not to widen the policy: a size belongs in
  a `width`/`height` attribute, and the bar is now the native `<progress>`
  element, which takes its value in one too. `TestNoTemplateWritesAnInlineStyle`
  is the guard, and it lives in this package because what makes it true is the
  directive fifty lines above it.

  This is the shape of every bug this policy will ever cause: the markup is
  right, the server is happy, and the browser quietly does less than was asked.
  Anything new that a directive could forbid gets an assertion the same day.
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
- **The sqlite DSN takes no parameters.** WAL, `foreign_keys`, `busy_timeout`
  and `_txlock=immediate` are correctness requirements for a server, not
  operator preferences, so the adapter sets them.

  That last one is not obvious and cost us a bug. Without it the driver opens a
  transaction with a plain `BEGIN`, which is deferred: one that reads before it
  writes -- every write in `internal/files`, since each checks its parent first
  -- takes a read snapshot and then has to upgrade, and SQLite refuses that
  upgrade with `SQLITE_BUSY` **immediately**. `busy_timeout` does not apply,
  because waiting cannot help a snapshot that is already stale. Measured: 22 of
  60 concurrent writes failed, which is a phone with more than one upload in
  flight getting 500s. Taking the write lock at `BEGIN` leaves nothing to
  upgrade, and the second writer waits like the timeout always promised. The postgres one passes its parameters through: that set is
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

**A directory copies too, and that is the ordering's second use.** `COPY` of a
collection was a 501 because of what a half-finished one leaves behind, not
because of the recursion (#43). Blob first and row second is what answers it:
a tree copy writes every blob, commits every row in one transaction, and a
failure anywhere leaves a tree that was never touched and orphans the sweep
takes. The bytes move outside the transaction, so a copy of a thousand
photographs does not hold a write transaction open for as long as it runs.
`Write` was split for it -- `storeBlob` returns the row without committing it --
so a copy cannot grow a second opinion about what a blob key is or how an ETag
is computed.

The blob is **not** shared between the two rows, tempting as one insert would
be: `Remove` deletes blobs as soon as its transaction commits, so removing
either copy would destroy the other. That wants reference counting, or a delete
that leaves its blobs to the sweep, and both are larger than the feature.

And because a copy is the first thing here that can predictably fill a disk,
the port grew `FreeSpace`, which answers a number or `storage.Unlimited` -- a
number and not a second return value, so the only thing a caller does with it
needs no branch for the backend that cannot say. The disk backend asks
`Statfs` for the blocks available to a non-root process, since that is what this
container can actually write; S3 says `Unlimited`, which is a report and not an
evasion, because a bucket has no size the API will admit to. Not enough room is
a refusal before the first byte, and `507` on the wire.

`/status` shows the same number, where `Unlimited` renders as the word and not
as nine exabytes -- which is the thing #154 warned a UI would do with an
invented figure, and the reason the constant is named for what it means. The
one place left that could say it is WebDAV, through RFC 4331's
`quota-available-bytes`, and go-webdav has no quota support and no way for a
backend to add a property: that is the same wall #136 is waiting at, and now the
second reason to take that decision.

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
- **Two WebDAV libraries, split by method.** `github.com/emersion/go-webdav`
  answers everything except `PROPFIND`, which is
  `golang.org/x/net/webdav`'s. The rule in one line: **the one that can express
  a property answers `PROPFIND`.**

  That is not a taste. emersion builds every response from a fixed set --
  `webdav.FileInfo` has six fields and `propFindFile` has no hook -- and it has
  cost us four things: no class 2, so `LOCK` and `UNLOCK` are intercepted in
  front of it (#3); a multistatus built whole in memory, which is why
  `Depth: infinity` is refused (#160); no way to report free space over RFC
  4331 (#154); and no way to say which files have a preview (#136). Four is a
  pattern, and all four are the same wall.

  x/net has the door: `DeadPropsHolder` lets a `File` contribute properties,
  and its writer emits each response as it is produced. What made it look like
  the wrong trade is its `FileSystem` -- a `PUT` is `OpenFile` plus
  `io.Copy`, which fits a blob store badly and would want a pipe between the
  library and `files.Write`, and it does not honour `If-Match` (a `TODO` in its
  own source). **Both objections are about writing, and `PROPFIND` does not
  write**: it touches `Stat`, `OpenFile` with `O_RDONLY` and `Readdir`, which is
  why the split is by method and not by surface. The write path is untouched,
  `If-Match` still works, and CalDAV stays emersion's.

  `internal/dav/propfind.go` is the adapter, and it is **built per request on
  purpose**: x/net walks a directory and then reopens every resource to read
  its properties, throwing away the `os.FileInfo` it already had. Measured, a
  folder of fifty children costs 204 lookups without somewhere to keep what the
  listing returned and 4 with it -- the N+1 shape #160 removed from this very
  surface, and a test fails if it comes back.

  One thing changed that a client can see: **a collection's href ends in a
  slash now**, which is what RFC 4918's own examples do.

  **Two of x/net's optional interfaces are not optional here**, and both cost
  a bug before they were found. `ETager`, or the library computes a validator
  from the modification time and the size and a listing disagrees with a `GET`
  about the same bytes. And `ContentTyper`, or it falls back to
  `mime.TypeByExtension` and then to **reading the first 512 bytes** -- which
  this read-only filesystem refuses, so a `Depth: 1` listing of a folder died
  with a 500 after a partial document. Go's built-in table has no `.heic`,
  `.mp4`, `.mkv` or `.mp3`, and a distroless image has no `/etc/mime.types`,
  so the folder that broke it was a camera roll.

  Both were missed the same way: every listing this repository tested held a
  `.txt`, whose type Go knows without opening it, and the smoke assertion
  looked for the collection's own entry -- which is written before the walk
  begins, so a walk that died after one line still matched. **An assertion
  that only checks the first thing written cannot tell a listing from a
  failure.**

  And the property needs somewhere to point: `/thumb/` takes HTTP Basic as well
  as a session, because the client that reads `has-preview` authenticates over
  WebDAV and could not reach it otherwise. **That is the extension principle 2
  warns about, taken knowingly**, and what keeps it on the right side of the
  line is that nothing depends on it -- there is no standard way to ask a
  WebDAV server for a preview, so a client that does not know this URL renders
  none and works, which is what the app does against any other server today. It
  goes through the same verifier as every other surface, so a guess counts
  against the same rate limit rather than opening an oracle beside it.

  **A `PROPFIND` for the whole tree is refused before the library sees it**
  (#160), with the `403` and the `DAV:propfind-finite-depth` precondition
  RFC 4918 9.1 provides for exactly this. The library builds a multistatus as
  one value and marshals it whole -- `ServeMultiStatus` carries a
  `// TODO: streaming` -- and its `FileSystem` hands back a slice, so there is
  no shape here in which the answer goes out as it is found. Measured: a
  hundred thousand files in a thousand folders came to 44 MB of XML inside
  310 MB of heap, and it is linear. Saying yes properly means streaming, which
  means this library changing or being replaced, and that is a conversation to
  have when a real client needs it rather than a handler to write on
  speculation.

  The `Depth` header is read in `internal/dav/depth.go` and not taken from the
  library, which only passes the backend a bool by which time the answer is
  being built. An absent header is infinity, because the RFC says so, so it is
  refused too: one level would be a wrong answer a client could not tell from a
  right one. With that gone, `files.Walk` had no caller and is deleted -- the
  three hundred megabytes are not gated, they are unreachable.

  **And a directory listing uses its index now**, which is the other half of
  the same measurement and the part that was a plain bug. `ListFiles` ordered
  by `path` alone, which matched the unique index on `(owner_id, path)`, so
  SQLite took that one, used only `owner_id` from it and filtered
  `parent_path` over every row this owner has: one folder of a hundred cost
  58 ms on a library of a hundred thousand, against 0.3 ms through
  `files_owner_parent`. Ordering by `is_dir DESC, path` -- which that index is
  built in, and which `ListFilesPage` already answered in -- is the whole fix,
  and it was never only about deep listings: every `Depth: 1` PROPFIND, both
  Subsonic browse calls and the folder-cover lookup were paying it.
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

  **Nothing large is downloaded to be read** (#145). A file that states what it
  is near its head is read there, over ranges; anything else is copied only
  while it is under 64 MB, and above that it keeps its kind and nothing else,
  with the reason on the row so `/status` can say it. The same bound is what
  `media.CanThumbnail` checks before a listing offers a picture, because a grid
  of broken images is what #135 promised would not happen.

  **A thumbnail is the exception, and it is one because it can be** (#149).
  What a decoder needs is the headers and the first frames, so `window` fetches
  exactly those into a local file that claims the original's length and is
  almost entirely a hole -- measured at 2.1 MB on disk for a file claiming 6.2,
  with the frame coming out of it. It works for the containers whose headers can
  be found without reading the film: Matroska and WebM from their beginning, an
  MP4 or QuickTime from its beginning plus the `moov` atom wherever it lives.
  There is no third option: a film with no window is a film with no picture,
  never a download.

  **And the frame is chosen, not taken.** A recording that opens on black -- a
  fade, a phone starting before the sensor settles -- gives a black first frame
  and a black frame a second in, both measured at zero average brightness. The
  `thumbnail` filter weighs a hundred of them against their own average and
  returns the least ordinary, which on the same file is 125 out of 255. That
  filter is in the build for this and nothing else.

  What that costs is a duration AVI, WMV and MPEG-TS will not have. It is the
  right trade and it was measured: those three state no duration at all, so
  ffprobe derives one from what it can reach and a partial file answers 7.5
  seconds for a thirty-second film. A wrong number is worse than none, and this
  is why the readers below exist for the two formats where a header is enough.

  **What it is not required for is an MP4.** ffprobe has to seek inside a file,
  so probing one meant copying the blob to disk first -- on S3, downloading a
  four-gigabyte recording to learn that it is four minutes long (#48). Duration,
  dimensions, codec, rotation and date are in a box at one end of the container,
  and the storage port reads ranges, so `internal/media/mp4.go` walks to it in
  three ranged reads and never touches the frames. It is the argument the
  embedded-cover and EXIF readers already make, applied where the file is
  measured in gigabytes.

  Nor for a Matroska: `internal/media/mkv.go` is its twin over EBML, where Info
  and Tracks sit in the first few hundred bytes and the clusters that hold the
  film are stepped over by arithmetic.

  Two rules keep those from becoming a second, worse ffprobe. **They answer only
  for the containers they can read whole-heartedly** -- `.mp4`, `.m4v`, `.mov`,
  `.mkv` and `.webm`; AVI, WMV and MPEG-TS are copied and probed while they are
  small enough, and `.m4a` is the same box structure as an MP4 but has nothing
  to win. And **it never guesses**: a codec whose fourcc is not in its table, a
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
- **A write says which file; the query is what finds the rest.** What is
  pending is a `LEFT JOIN` over the file rows -- nothing is enqueued, nothing is
  dequeued, a restart loses nothing and an import that inserts rows is picked up
  without knowing this exists. What it costs is a scan of every file row, and
  measured on a hundred thousand of them it is 82 ms whether the answer is a
  file or nothing, because the ordering puts the newest last and the `LIMIT`
  cannot stop early (#158). Running that every minute to learn something we
  already knew is the thing this stopped doing.

  So `internal/files` takes a watcher, the composition root points it at the
  indexer, and **the notice carries the file**. That is not a second extractor:
  `IndexFile` and `IndexBatch` reach the same one, and what differs is how the
  file was found. The watcher takes no context, because the work outlives the
  request that caused it.

  **What makes the query rare is that a dropped notice says so.** The channel
  holds a few hundred files and a write must never block on it, so a burst
  larger than that would silently cost the overflow a wait -- for an hour now,
  not a minute. Instead the drop raises a second signal, and what is on the
  other side of it is the query brought forward: one signal covers any number
  of dropped files, since the query looks at all of them. That is what lets the
  buffer be a small number rather than one to tune.

  The interval is then the safety net and nothing else -- rows an import
  inserted, a `media.Version` bump, anything that landed while the process was
  not running, a file deferred until its retry time -- which is why it is an
  hour and why it is a `Ticker` rather than a timer armed after each pass: a
  steady trickle of uploads would restart a timer forever and the four things
  above would never be looked at. **It is also the resolution of the retry
  clock**: an hour of `retryAfter` under an hour of interval means a deferred
  file waits between one and two.

  One failure the direct path introduced and the query could not have: a notice
  naming a row that has since been deleted. All three drivers now map a foreign
  key violation to `ErrNotFound`, and the indexer shrugs -- there is nothing to
  record about a file that does not exist.

  **A media row records the validator it was extracted from.** Replacing a file
  keeps its row and its id, so without that the metadata of the bytes that are
  gone would describe the bytes that are there until somebody raised the
  extractor version -- which is what it did until #48. What this still cannot
  see is a rename that changes an extension, where the bytes, and therefore the
  validator, are the same while the kind is not.

  **A failure to understand some bytes is a verdict; a failure to reach them is
  not** (#157). Both used to be written down the same way -- the reason on the
  row, and the row counts as done -- so thirty seconds of a bucket not answering
  marked a whole batch as unreadable for good, and only a version bump would
  have looked at it again. Now a failure on the way to the bytes leaves a
  `retry_at` on the row and the queue's last clause brings the file back when it
  arrives. An hour, flat: an outage lasting a day costs a file twenty-four
  attempts, and a counter with an escalating delay would be arithmetic nobody
  has needed.

  The row is written either way, and that is the part worth keeping: a deferred
  file with no row at all would sit at the head of a queue ordered by id and
  limited to a batch, and nothing behind it would ever be read. `/status` counts
  it as pending rather than failed, because it is.

  Two things make the distinction possible. The extractors turn a failed read
  into their own "not my format" -- `mkv.go` does it in one line -- so the error
  that comes back cannot be trusted to say where the failure was; a `storeReader`
  wrapped around the one body every extractor reads from remembers what the
  store actually did. And what is local rather than about the file is marked
  where it happens: a spool with nowhere to write, and an ffprobe that ended on
  a signal rather than an exit code, which is how the out-of-memory killer and
  the timeout both arrive.

  **The one answer from the store that is about the file is that the object is
  not there.** A row pointing at a blob nobody has is corruption, and saying so
  is worth more than an hourly retry forever -- unless it is the answer for
  every file in the batch, which is a database pointed at an empty bucket rather
  than a library that rotted. That batch is deferred whole, which is the
  judgement `files.Collect` already makes before it deletes anything.
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

  **Both halves turn a picture the right way up** (#148). ffmpeg does it for
  what it decodes; for what Go decodes, `reduceTo` reads the EXIF tag off the
  head of the file -- the same ranged read the indexer and the cover reader
  make -- and turns the thumbnail after it has been reduced, which is a
  hundredth of the work of turning the original. The row keeps the value the
  file gave: a client that rotated what we serve would rotate it twice.

  **The generator has a version now, and it is `files.DerivedGeneration`**
  (#161). Raising it changes the key -- the number is on the front of the leaf,
  so `parentOf` still finds the parent by cutting at the last slash -- and that
  alone would only stop the old picture being served, leaving it on the disk for
  as long as its parent lived, which is the objection that kept the key a pure
  function of its parent's. So the sweep gained a second rule beside the first:
  **a derived object is garbage when its parent is gone, or when its leaf does
  not name the current generation.** A leaf naming none is stale too, which is
  what collects every picture made before this existed -- the sideways ones from
  before #148 and the black frames from before #149, on the first sweep after
  the upgrade.

  It is `media.Version` for the other half of this package, and the asymmetry it
  removes is exactly the one that paragraph names: one half could reach what it
  had already looked at and the other could not. It lives in `internal/files`
  because that is where the sweep reads it back, the same reason the rest of the
  shape does, and `TestTheGeneratorOutputHasNotMoved` is the mirror that makes
  somebody notice -- a digest of what comes out of `reduceTo`, which fails on
  the day the pixels move rather than a release later.

  What that costs, and it is the same cost as a version bump on the other side:
  the library is regenerated. Lazily, so nobody waits for all of it, but paced
  by whoever is browsing rather than by one worker -- a grid of a hundred
  photographs after an upgrade remakes a hundred thumbnails, four at a time.
  The day the reason to raise it is "the scaler is five per cent better" is the
  day to not raise it.

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
- **litmus is the canary, and it is the only test here that did not come from
  our own understanding** (#173). Everything else in this repository was
  written from the same reading of WebDAV as the code, so it agrees with the
  code by construction. litmus is a 2011 C suite by the people who wrote the
  reference implementations, and it has no opinion about what we meant: its
  first run found a `SQLITE_BUSY` under two concurrent writers that every test
  here had missed (#183).

  Built rather than installed, in `build/litmus`, because there is no image, no
  release on any forge and no package worth pinning -- the last release is a
  tarball on a web page, so the source is pinned by digest the way ffmpeg's is.
  Debian and not Alpine, unlike the other two: it carries its own `getopt` for
  systems without one, and on musl that collides. It never ships.

  **Each suite is held to the number it passes today**, and that number is the
  point. `basic`, `copymove` and `http` pass whole. `props` passes 10 of 14 --
  `PROPPATCH` is refused, because there are no dead properties here. `locks`
  passes 29 of 33, and it went from 17 the day locking became real (#174):
  what is left is `PROPPATCH` twice, a shared lock and a `LOCK` on a path with
  nothing at it, each refused on purpose. A suite that passes fewer is a
  regression and fails the build; one that passes more is a number to raise in
  a commit, with the reason. That is the same
  discipline as `deps.allow` and the coverage floors, and it is what keeps
  "known gap" from becoming "silently excluded".
- **Locking is real, and what made it possible was already linked** (#174).
  `LOCK` used to answer with a token nothing recorded, because Finder will not
  mount a share read-write against a class 1 server (#3) and the state looked
  like a table. It is not a table: `golang.org/x/net/webdav` arrived for
  `PROPFIND` (#136) and brought a `LockSystem` with it.

  **The lock system is an interface the library brought, not a third seam this
  project invented.** `xnet.LockSystem` is four methods; the composition names
  `NewMemLS()`, and a table-backed one the day a lock has to outlive a restart
  is a type satisfying the same four and a line in `dav.Handler`. There is no
  configuration variable for it today, deliberately: a setting that accepts one
  value promises a choice that does not exist, which is what principle 3 calls
  "just in case".

  **In memory is right rather than merely cheap.** A lock is a claim with a
  timeout measured in minutes and a restart forgetting one costs a client a
  retry -- the same trade the signed session makes. It is also consistent with
  a server that assumes a single instance in five other places, none of which
  said so out loud: SQLite is a local file, the indexer would have two
  instances take the same batch, the sweep would have both compute the same
  garbage, the tus spool for S3 is local so a resumed upload must come back to
  the same process, and the disk backend empties its reserved directory at
  startup on the argument that what is in it belongs to a dead process. A lock
  table would have been the only cluster-ready thing in it. That inventory is
  an issue of its own, as what #28 has to answer before stateless deployment
  means more than one of anything.

  **The `If` header parser is vendored, and that is the expensive part.** RFC
  4918 10.4 is the gnarliest grammar in the specification and x/net keeps its
  parser unexported: 173 lines with a lexer of its own.
  `internal/dav/ifheader.go` is that file copied byte for byte, package clause
  aside, with
  its 322 lines of upstream tests beside it -- because a lock is the one place
  a parser bug is a silent authorisation failure, and a fresh parser would be
  our own bugs there. What it costs is stated in its header: it is Go source
  rather than an asset, so `deps.allow` cannot see it, no upstream fix arrives
  on its own, and it is ours from now on. `.golangci.yml` excludes both files,
  because linting a copy is how it stops being one.

  **Enforcement is ours, and it is not x/net's** -- that library enforces locks
  inside handlers which serve nothing here. `internal/dav/locks.go` is the
  third thing in this package that reads a request before the library does,
  after `depth.go` and `signed.go`, and it differs from x/net in three places
  that were each found by litmus or by reading its source:

  - **A tagged list has to be about this request.** x/net confirms the
    conditions against the resource named in the tag and then lets the request
    through, so a client holding a lock of its own anywhere could name it and
    walk past somebody else's. Here the tag has to name what is being written
    or a collection above it -- which is also how a client submits the token of
    a folder it locked while writing a file inside, the shape litmus's
    `lock_collection` uses.
  - **A destination that nobody has locked is not an error.** x/net asks for
    every named resource to be covered by a claimed lock, which makes a `MOVE`
    onto a free path impossible for the client holding the source. What the
    other end has to be is not somebody else's, and a brief lock for the length
    of the request is how that is asked -- the same trick as the no-`If` case.
  - **`Not` and `ETag` conditions are evaluated.** The lock system ignores both
    -- a `TODO` in its own source -- and says only whether some token can claim
    a lock here. So `If: (<token> ["etag"])`, a client saying "only if the bytes
    are still these", would be a precondition nobody checked: the same quiet lie
    as a lock nothing records, one layer along. `fail_complex_cond_put` is what
    catches it.

  Two smaller things are deliberate. A token goes out as
  `opaquelocktoken:<n>` because RFC 4918 6.5 says a lock token is a URI and
  `memLS` hands back a bare integer; it is **not** made unguessable, and that
  is not an oversight -- anybody who can take a lock is shown the shape of
  them, and anybody who cannot take one cannot write either. And a shared lock
  is refused with `501` rather than granted as an exclusive one, which is what
  `supportedlock` has been advertising all along.

- **Text answers are compressed, and it is the composition root that does it**
  (#178). `internal/app/compress.go` wraps the whole mux, inside the request log
  so the bytes it counts are the bytes that went out.

  The measurement is the argument: a `PROPFIND` of two hundred files is 138 KB
  of XML and gzip takes it to 3.2 KB, because a multistatus is the same forty
  tags repeated and that is the best case deflate has. The issue behind it was
  filed about the namespace declaration repeated on every element -- a fifth of
  the document when it was measured against emersion, and 2% of it once
  PROPFIND moved to x/net (#136), which is a good reminder that a measurement
  has a date on it. Compressed, that 2% is worth nothing at all, and the
  thousand lines of multistatus writer it would have taken to recover stay
  unwritten.

  **There is no reverse proxy to do it for us**, which is principle 1 read from
  the other side: the thing that would normally compress -- nginx, Caddy, a CDN
  -- is exactly what "one binary, one container" says we do not deploy.

  Three rules in it are correctness rather than preference, and each has a test:
  a partial response is never compressed, because `Content-Range` counts bytes
  of the original representation and video seeking is made of these; a response
  that already carries a `Content-Encoding` is left alone; and a strong `ETag`
  on a compressed answer is weakened, since it names a representation and this
  is a different one. `Vary: Accept-Encoding` goes on every answer to a client
  that asked, including the ones we decline to compress, or a cache in front
  hands the gzipped body to somebody who cannot read it.

  **The gate is the content type, not the route.** A photograph, a video and a
  track are already compressed, so trying is pure cost -- and doing it by type
  means a surface added later is covered without anybody remembering to.

  BREACH was considered and does not apply here, which is worth writing down
  before somebody makes it apply: it needs a secret in the response body beside
  attacker-influenced content, and there is none -- the session is a cookie and
  CSRF is `SameSite` plus `CrossOriginProtection`, so no page carries a token.
  The day a form grows one is the day this paragraph is a problem.

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
- **One deploy manifest, and it is Render's** (#13). `render.yaml` is the only
  hosted-platform file here that is a recommendation: a web service on the
  published image and a disk at `/data`. The three platforms a Go binary is
  usually pushed to are excluded on purpose -- Vercel, Netlify and Cloud Run are
  stateless and scale to zero, which kills the indexer goroutine and forces S3
  plus a hosted database before anything works -- and Fly has no button to
  offer, which is why `fly.toml` in this repository is the throwaway test
  instance and must not be advertised as a way to run Stratus.

  What made a button possible at all is the password being held as configured:
  no platform can compute a bcrypt hash in its own form, so a hashed one meant
  a laptop and a terminal before the first boot.

  **The thing to check on any platform added here is who owns the mount.** The
  image runs as 65532, and a platform that hands it a root-owned volume -- which
  is exactly what Docker does with a fresh named one -- produces a server that
  refuses to start with a message about `/data` and no obvious cause. Fly reads
  the image's user and mounts accordingly; that is a property of the platform,
  not of the manifest, so it is answered by deploying rather than by reading.
