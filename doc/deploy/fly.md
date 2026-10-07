# Fly.io

No button — Fly has none. A `fly.toml`, an app, a volume, two secrets and a
deploy.

```sh
fly apps create my-stratus
fly volumes create stratus_data --app my-stratus --region cdg --size 1
fly secrets set --app my-stratus STRATUS_USERNAME=edu STRATUS_PASSWORD='choose one'
fly deploy --app my-stratus --ha=false   # reads ./fly.toml
```

## fly.toml

```toml
app = "my-stratus"
primary_region = "cdg"

# 256 MB is not enough for the first thumbnails while the indexer is still
# reading: the kernel kills the server mid-request and the proxy answers 502.
# Swap takes the spike more slowly instead.
swap_size_mb = 512

[build]
  image = "ghcr.io/c0piiot/stratus-backend:main"

[env]
  STRATUS_ADDR = ":8080"
  STRATUS_DATA_DIR = "/data"

[http_service]
  internal_port = 8080
  # Fly terminates TLS and forwards the scheme, which is what makes the
  # session cookie Secure.
  force_https = true
  # Nobody is watching it most of the time, so it sleeps and the first request
  # wakes it. Fly will not suspend a machine with swap, so: stop.
  auto_stop_machines = "stop"
  auto_start_machines = true
  min_machines_running = 0

  [[http_service.checks]]
    interval = "30s"
    timeout = "5s"
    grace_period = "15s"
    method = "GET"
    path = "/healthz"

# Name, region and size are the volume you created above. 1 GB is the
# smallest Fly sells; a volume can be extended, never shrunk, so size it for
# the library rather than for the first week.
[mounts]
  source = "stratus_data"
  destination = "/data"
  initial_size = "1gb"

[[vm]]
  size = "shared-cpu-1x"
  memory = "256mb"
```

## Things worth knowing

- **The volume is not optional, and a deploy will not make it.** A deploy
  mounts a volume that is already there, which is why `fly volumes create` is
  its own step. A machine's own disk is thrown away on a cold start, and
  `auto_stop_machines` guarantees cold starts.
- **A deploy does not empty the volume**, and neither does stopping the machine
  and starting it again. Only destroying the volume does.
- **One machine, hence `--ha=false`.** Fly's default is to spread a new app
  over two, which here means two servers on one library — the thing this one is
  written not to be — and the second has no volume to mount anyway.
- **`/healthz`, not `/readyz`** as the check: liveness touches no dependency, so
  a database blip does not get the machine restarted. The
  [README](../../README.md#two-health-endpoints-and-which-is-which) says why.
- **The mount is owned by uid 65532**, the image's user, because Fly reads it
  from the image. A platform that hands over a root-owned volume instead
  produces a server that refuses to start with `/data` in the message.
- **Upgrading is `fly deploy --ha=false` again**: `:main` is a moving tag,
  pulled when you ask for it.

## Changing the options

Secrets, so nothing with a password in it lands in the file:

```sh
fly secrets set --app my-stratus \
  STRATUS_STORAGE_DSN='s3://KEY:SECRET@s3.eu-west-1.amazonaws.com/bucket?region=eu-west-1' \
  STRATUS_DB_DSN='postgres://user:pass@db.example:5432/stratus?sslmode=require'
```

Either way `/data` stays: it is the spool the indexer and the thumbnail
generator work in. Everything else is in the README's configuration table.

---

The [`fly.toml`](../../fly.toml) in this repository is not that file: it is the
disposable instance CI keeps on the last green `main`, open to anybody and
emptied every hour. Read it as a worked example, not as a starting point.
