# stratus-backend

The Stratus server: one Go binary, one container, no sidecars.

Stratus is a self-hosted personal cloud for photos, calendar, music and video.
Instead of shipping its own API and a client app per platform, it speaks
protocols your existing apps already understand.

## Protocols

| Protocol | Use | Works with |
|---|---|---|
| WebDAV | files, photo backup, sync | rclone, Finder, Nautilus, DAVx5, FolderSync |
| CalDAV | calendar | DAVx5, Thunderbird, iOS/macOS |
| OpenSubsonic | music | Symfonium, Substreamer, DSub, Feishin |
| HTTP range | audio/video streaming | browsers, VLC, mpv |
| CardDAV | contacts | *planned* |
| DLNA / UPnP-AV | TVs, set-top players | *planned* |

## Pluggable backends

Two seams, and only two:

- **Blob storage** — `disk` and `s3` at launch; ftp and others later.
- **Metadata database** — `sqlite` at launch (pure Go, CGO-free); postgres and
  mysql later. No driver-specific SQL leaves its driver package.

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

## Configuration

Every setting has a default and an env var. Copy `.env.example` to `.env`, or
pass overrides on the command line:

```sh
make up STRATUS_PORT=9000 STRATUS_DATA_PATH=/srv/stratus
```

| Variable | Default | Meaning |
|---|---|---|
| `STRATUS_PORT` | `8080` | host port the backend is published on |
| `STRATUS_DATA_PATH` | `./data` | host dir for photos, music, calendars, SQLite |
| `STRATUS_UID` / `STRATUS_GID` | invoking user | user the container runs as |
| `STRATUS_LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |

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

Multi-stage build, `distroless/static:nonroot` runtime, ~16 MB image. The
container runs non-root with a read-only root filesystem, all capabilities
dropped and `no-new-privileges`. The healthcheck is the binary probing itself
(`stratus -healthcheck`) since distroless ships no shell or curl.

## Status

Greenfield — the skeleton above is all there is so far. Work is tracked on the
[Stratus project board](https://github.com/users/C0piIot/projects/2).

## License

MIT — see [LICENSE](LICENSE).
