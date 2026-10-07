# Render

[![Deploy to Render](https://render.com/images/deploy-to-render-button.svg)](https://render.com/deploy?repo=https://github.com/C0piIot/stratus-backend)

The button reads [`render.yaml`](../../render.yaml) and creates one web service
on `ghcr.io/c0piiot/stratus-backend:main` with a 10 GB disk at `/data`. Nothing
else: the blobs and the SQLite database both live on that disk.

The form asks for `STRATUS_USERNAME` and `STRATUS_PASSWORD` and for nothing
further. The password is held as configured rather than hashed, so there is
nothing to compute on a laptop before the first boot. Render terminates TLS and
forwards the scheme, so the session cookie comes back `Secure` with no setting
to find.

## Cost

$7 a month for the smallest instance that can take a disk, plus $0.25 per GB:
around $9.50 for the 10 GB in that file. The workspace itself is free. No free
instance type takes a disk, and one without would lose every photograph on the
next deploy.

## Changing the options

`render.yaml` names only `PORT`, `STRATUS_USERNAME` and `STRATUS_PASSWORD`.
Everything else keeps the image's defaults, which is what makes the dashboard
safe: a Blueprint sync only re-imposes the keys the file names, so a variable
you add there is never overwritten.

| What | Where |
|---|---|
| S3 instead of the disk for blobs | `STRATUS_STORAGE_DSN` in Environment — [who sells one](s3.md), Render's own included |
| PostgreSQL instead of SQLite | `STRATUS_DB_DSN` in Environment — [who sells one](database.md); Render Postgres can be declared in a forked `render.yaml` and wired with `fromDatabase` |
| Anything else in the config table | the same place |
| Disk size | Disks in the dashboard, or `sizeGB`; it can be raised, never lowered |
| Region | fork, put `region:` in `render.yaml`, use the button on your copy — a service cannot be moved afterwards |

Both DSNs are documented in the [README](../../README.md#blob-storage), and
the pages above list who sells the other end of them. Moving
the blobs or the database elsewhere does not make the disk optional: `/data` is
also the spool the indexer and the thumbnail generator work in.

Neither DSN is in the form on purpose. A Blueprint `envVars` entry is either
prompted and **required** (`sync: false`) or fixed in the file; Render has no
optional field, no editable default and no help text, so the two of them in
the form would be two required boxes most people should leave as they are, and
a mistyped DSN is a server that refuses to start.

## Upgrades

The Blueprint pins `:main` and sets `autoDeployTrigger: off`, so a merge in this
repository never touches a server somebody else deployed. **Deploy latest
reference** in the dashboard pulls the tag again.

Deploys are not zero-downtime once a disk is attached: Render stops the old
instance before it starts the new one, a few seconds either way.

## What a real deploy answered

October 2026, on the plan in that file (#190): the disk comes up writable for
uid 65532, `/readyz` reports `database: ok` and `storage: ok`, the web sign-in
sets a `Secure` cookie, a WebDAV `PUT` lands, and a manual redeploy leaves the
file where it was.
