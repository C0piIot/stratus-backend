# Fly.io

No button — Fly has none — but [`fly.toml`](../../fly.toml) at the root of the
repository is written to be launched, so one command makes the app, the
volume, the secrets and the first deploy:

```sh
fly launch --from https://github.com/stratus-private-cloud/stratus-backend --copy-config \
  --ha=false --secret STRATUS_USERNAME=edu --secret STRATUS_PASSWORD='choose one'
```

It asks for an app name unless `--name` gives one, and the region is `cdg`
unless `--primary-region` says otherwise. `--from` clones the repository into
the current directory, so run it in an empty one; what it leaves there is a
copy of `fly.toml` with the app name written in, which is yours to keep or
throw away.

**`--copy-config` is not optional.** Without it launch ignores the file, scans
the clone, finds the Dockerfile and builds it, on a 1 GB machine with no
volume — a server that loses the library on its first cold start.

Read the file before launching: the machine size, the swap and the volume
size are in it, each with the reason it is what it is.

## Things worth knowing

- **The volume is not optional, and only launch makes it.** A later deploy
  mounts a volume that is already there; a machine's own disk is thrown away
  on a cold start, and `auto_stop_machines` guarantees cold starts. A volume
  can be extended, never shrunk, so size it for the library rather than for
  the first week.
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
- **Upgrading is `fly deploy --app my-stratus --ha=false`**, from anywhere:
  with no `fly.toml` in the directory it deploys the configuration the app
  already has, and `:main` is a moving tag, pulled when you ask for it.

## What a real deploy answered

October 2026, with flyctl 0.4.112 and the file as it is (#177): launch made one
256 MB machine and the 1 GB volume, and set both secrets before the first
boot. The volume came up writable for uid 65532, plain http redirected to
https, the web sign-in set a `Secure` cookie, a WebDAV `PUT` landed, tus
answered, and the file was still there after a stop and a start and after a
`fly deploy` run from a directory with no `fly.toml` in it.

## Changing the options

Fly sells both ends: [Tigris](https://fly.io/docs/tigris/) for the blobs and
[Managed Postgres](https://fly.io/docs/mpg/) for the index, which are the
nearest of the options in [s3.md](s3.md) and [database.md](database.md).
Either way it is a secret, so nothing with a password in it lands in the file:

```sh
fly secrets set --app my-stratus \
  STRATUS_STORAGE_DSN='s3://KEY:SECRET@s3.eu-west-1.amazonaws.com/bucket?region=eu-west-1' \
  STRATUS_DB_DSN='postgres://user:pass@db.example:5432/stratus?sslmode=require'
```

Either way `/data` stays: it is the spool the indexer and the thumbnail
generator work in. Everything else is in the README's configuration table.

---

[`deploy/demo/fly.toml`](../../deploy/demo/fly.toml) is the other Fly file in
this repository, and it is not for launching: it is the disposable instance CI
keeps on the last green `main`, open to anybody and emptied every hour. It
lives out of the root so that `fly launch --from` cannot find it.
