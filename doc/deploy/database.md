# The metadata database, if not SQLite

SQLite is the default, it lives in `/data` with the blobs, and for one user it
is not a compromise: the server is one process, so there is no second writer to
contend with. Reasons to move anyway: the host charges for a disk and gives you
a database for less, you want backups and point-in-time recovery somebody else
operates, or you want the library's index somewhere a dead machine cannot take
with it.

```
STRATUS_DB_DSN=postgres://user:pass@host:5432/stratus?sslmode=require
STRATUS_DB_DSN=mysql://user:pass@host:3306/stratus?tls=true
```

Parameters are passed through to the driver, so anything
[pgx](https://pkg.go.dev/github.com/jackc/pgx/v5) or
[go-sql-driver/mysql](https://github.com/go-sql-driver/mysql#parameters) takes
works here. Migrations run at startup, so a wrong DSN is a server that does not
come up rather than a 500 later.

**Pick before the library is full.** There is no migration between engines, and
no exporter: changing your mind later means starting empty.

## The one your host already sells

| Host | Its database |
|---|---|
| Render | [Render Postgres](https://render.com/docs/postgresql). A forked `render.yaml` can create it and wire it in one step — a `databases:` entry, then `STRATUS_DB_DSN` with `fromDatabase: { name: stratus-db, property: connectionString }`. The free instance expires after 30 days; the disk you no longer need pays for a small paid one |
| Fly | [Managed Postgres](https://fly.io/docs/mpg/): `fly mpg create`, then put the connection string in `STRATUS_DB_DSN` as a secret. The older unmanaged `fly pg` app is deprecated |
| DigitalOcean | [Managed Databases](https://www.digitalocean.com/products/managed-databases), PostgreSQL or MySQL |
| Scaleway | [Managed Database](https://www.scaleway.com/en/database/) |
| AWS | [RDS](https://aws.amazon.com/rds/) |

## The rest

| Provider | Worth knowing |
|---|---|
| [Neon](https://neon.tech/) | Serverless Postgres with a free tier that stays free; it scales to zero, so the first request after a quiet night waits for it to wake |
| [Supabase](https://supabase.com/database) | Postgres with a free tier. Use the direct connection string, not the transaction pooler — see below |
| [Aiven](https://aiven.io/) | PostgreSQL and MySQL, free plan on both |
| [Crunchy Bridge](https://www.crunchydata.com/products/crunchy-bridge) | Postgres, operated by people who work on Postgres |
| [Railway](https://railway.com/) | Postgres and MySQL, usage-billed |
| [PlanetScale](https://planetscale.com/) | MySQL on Vitess, and Postgres. Vitess is not plain MySQL: check foreign keys and `FULLTEXT` before committing, because the search index is one |

Or your own `postgres:` container next to this one. The database is one of the
two seams this server is pluggable at, and an external one is the ordinary case
rather than the exotic one.

## What trips people

- **A transaction-mode pooler** (Supabase's 6543, PgBouncer, Neon's pooled
  endpoint) re-uses server connections between statements, and pgx prepares
  statements by name. Prefer the direct or session connection string; if you
  cannot, `default_query_exec_mode=exec` in the DSN is the knob.
- **MySQL means MySQL 8.0.19 or newer**, for the upsert syntax the driver uses.
  MariaDB is a different database wearing the same name and is not what this is
  tested against.
- **TLS is not on by default in either DSN.** Postgres wants `sslmode=require`,
  MySQL wants `tls=true`, and a managed provider will refuse the connection
  without them — which is the good outcome.
- **The disk does not go away.** `/data` still holds the blobs unless you also
  move those ([s3.md](s3.md)), and the indexer's spool whatever you do.
- **An old database is refused, not repaired.** Until the first tagged release
  the schema is rewritten rather than migrated, so a database made by an
  earlier `:main` image stops the server at startup with the version it found.
