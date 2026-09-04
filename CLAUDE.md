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
  use Basic and token auth. Cookies mean CSRF protection on every mutating form.

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
internal/calendar/        collections, objects, recurrence
internal/music/           library model, browse, search
internal/media/           EXIF/tag extraction, thumbnails, ffprobe
internal/auth/            credential verification, per-protocol adapters

internal/dav/             inbound adapter: WebDAV + CalDAV
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
- **Features** (`files`, `calendar`, `music`) own the invariants that must look
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
- **`photos`.** Photo backup is files plus EXIF indexing; the photo-ness lives in
  `media` and in date queries.
- **Any job framework.** The indexer is a goroutine started by `app`.

## Tech decisions

- Go, `net/http` from stdlib, **no web framework**.
- `github.com/emersion/go-webdav` for DAV/CalDAV primitives.
- `minio-go` for S3 (much lighter than `aws-sdk-go-v2`).
- Media processing: **ffmpeg is a requirement, not an optional extra.** Without
  it a track has no duration and a video no dimensions, and half a media library
  is worse than an honest refusal to start. The image carries a statically linked
  `ffprobe` copied into the same distroless base rather than switching to one
  with a package manager; the encoder arrives with thumbnails.
- Config over convention: sane defaults, everything overridable by env var.
- Web UI: `html/template`, Bootstrap and htmx vendored and `//go:embed`ed. No
  JavaScript toolchain, no custom CSS.

