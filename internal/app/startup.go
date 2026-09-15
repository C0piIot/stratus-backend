// Everything that runs once, before the server listens.
//
// A composition root's job is to fail here rather than later: a blob store that
// cannot be written to, a database that cannot be migrated or a data directory
// somebody else owns are all better as an exit code than as the first upload
// somebody loses.
package app

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/C0piIot/stratus-backend/internal/auth"
	"github.com/C0piIot/stratus-backend/internal/config"
	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/db/mysql"
	"github.com/C0piIot/stratus-backend/internal/db/postgres"
	"github.com/C0piIot/stratus-backend/internal/db/sqlite"
	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/media"
	"github.com/C0piIot/stratus-backend/internal/storage"
	"github.com/C0piIot/stratus-backend/internal/storage/disk"
	"github.com/C0piIot/stratus-backend/internal/storage/s3"
)

// probeFile is written and removed at startup to prove the data dir is writable.
const probeFile = ".stratus-write-probe"

// probeKey is the same idea one layer up, in the blob store. It is a valid key
// on every backend, and it never survives startup.
const probeKey = "stratus-write-probe"

// startupTimeout bounds opening and probing the blob store. An S3 endpoint that
// accepts a connection and then says nothing would otherwise hang the process
// before it ever listens, which looks identical to a hung container.
const startupTimeout = 30 * time.Second

// open runs every startup check and returns the backends they proved usable.
//
// The named return is what makes the deferred close correct: a failure halfway
// through has already opened something, and returning early without this would
// leak a file handle or a connection pool on every refused start.
func (a *App) open(ctx context.Context) (deps Deps, err error) {
	defer func() {
		if err != nil {
			_ = deps.Close()
		}
	}()

	if err = checkCredentials(a.cfg); err != nil {
		return deps, err
	}
	// Fail fast and loudly: a data dir the process cannot write to is the most
	// likely misconfiguration, and finding out on the first upload is too late.
	if err = EnsureDataDir(a.cfg.DataDir); err != nil {
		return deps, err
	}

	startupCtx, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()

	if deps.Storage, err = openStorage(startupCtx, a.cfg.Storage); err != nil {
		return deps, err
	}
	if err = probeStorage(startupCtx, deps.Storage); err != nil {
		return deps, err
	}
	slog.Info("storage ready", "scheme", a.cfg.Storage.Scheme, "dsn", a.cfg.Storage)

	if deps.Database, err = openDatabase(startupCtx, a.cfg.Database); err != nil {
		return deps, err
	}
	// Migrating is also the write probe: a database user that cannot create a
	// table fails here rather than on the first upload.
	if err = deps.Database.Migrate(startupCtx); err != nil {
		return deps, fmt.Errorf("migrate the database: %w", err)
	}
	slog.Info("database ready", "scheme", a.cfg.Database.Scheme, "dsn", a.cfg.Database)

	if a.cfg.IndexInterval > 0 {
		// ffprobe is a hard requirement rather than an optional extra: without
		// it a track has no duration and a video no dimensions, and half a
		// media library is worse than an honest refusal to start.
		ffprobe, ferr := media.LookupFFprobe()
		if ferr != nil {
			return deps, ferr
		}
		tmp, terr := media.TempDir(a.cfg.DataDir)
		if terr != nil {
			return deps, terr
		}
		deps.Indexer = media.NewIndexer(files.New(deps.Storage, deps.Database), deps.Database, tmp, ffprobe)
	}

	if credentials(a.cfg).Configured() {
		slog.Info("webdav ready", "prefix", davPrefix, "user", a.cfg.Username)
	} else {
		slog.Warn("webdav disabled", "reason", "STRATUS_USERNAME and STRATUS_PASSWORD are not set")
	}

	return deps, nil
}

// credentials is the single user, as configured. It is built here rather than
// stored on App because nothing serves a login yet: the protocol surfaces take
// it when they arrive.
func credentials(cfg config.Config) auth.Credentials {
	return auth.Credentials{Username: cfg.Username, Password: cfg.Password.Reveal()}
}

// checkCredentials refuses a configuration that could never authenticate
// anybody, rather than letting it surface as a login failure much later. The
// rule itself lives in internal/auth; this only names the variables it came
// from, which is what the operator can actually act on.
func checkCredentials(cfg config.Config) error {
	if err := credentials(cfg).Validate(); err != nil {
		return fmt.Errorf("STRATUS_USERNAME / STRATUS_PASSWORD: %w", err)
	}
	return nil
}

// openStorage builds the backend the DSN selects. Knowing that both schemes
// exist is the composition root's job and nobody else's.
func openStorage(ctx context.Context, dsn config.StorageDSN) (storage.Storage, error) {
	switch dsn.Scheme {
	case config.SchemeFile:
		store, err := disk.New(dsn.Dir)
		if err != nil {
			return nil, err
		}
		return store, nil
	case config.SchemeS3:
		store, err := s3.New(ctx, s3.Config{
			Endpoint:  dsn.Endpoint,
			Bucket:    dsn.Bucket,
			AccessKey: dsn.AccessKey,
			SecretKey: dsn.SecretKey.Reveal(),
			Region:    dsn.Region,
			UseTLS:    dsn.UseTLS,
		})
		if err != nil {
			return nil, err
		}
		return store, nil
	default:
		return nil, fmt.Errorf("unsupported storage scheme %q", dsn.Scheme)
	}
}

// openDatabase builds the metadata backend the DSN selects. Like openStorage,
// knowing that both schemes exist is the composition root's job.
func openDatabase(ctx context.Context, dsn config.DatabaseDSN) (db.Store, error) {
	switch dsn.Scheme {
	case config.SchemeSQLite:
		store, err := sqlite.New(ctx, dsn.Path)
		if err != nil {
			return nil, err
		}
		return store, nil
	case config.SchemePostgres:
		store, err := postgres.New(ctx, dsn.ConnString.Reveal())
		if err != nil {
			return nil, err
		}
		return store, nil
	case config.SchemeMySQL:
		store, err := mysql.New(ctx, dsn.ConnString.Reveal())
		if err != nil {
			return nil, err
		}
		return store, nil
	default:
		return nil, fmt.Errorf("unsupported database scheme %q", dsn.Scheme)
	}
}

// probeStorage writes, reads back and removes one object before the server
// accepts anything. Same argument as EnsureDataDir, one layer up -- and for S3
// it is the only proof that the credentials can actually write, since listing a
// bucket and writing to it are different permissions.
func probeStorage(ctx context.Context, store storage.Storage) error {
	body := []byte("stratus")

	if _, err := store.Put(ctx, probeKey, bytes.NewReader(body), int64(len(body))); err != nil {
		return fmt.Errorf("blob storage is not writable: %w", err)
	}
	r, _, err := store.Get(ctx, probeKey, storage.All())
	if err != nil {
		return fmt.Errorf("blob storage is not readable: %w", err)
	}
	got, err := io.ReadAll(r)
	if cerr := r.Close(); cerr != nil && err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("blob storage is not readable: %w", err)
	}
	if !bytes.Equal(got, body) {
		return fmt.Errorf("blob storage returned %d bytes, not the %d written", len(got), len(body))
	}
	if err := store.Delete(ctx, probeKey); err != nil {
		return fmt.Errorf("blob storage cannot delete: %w", err)
	}
	return nil
}

// EnsureDataDir creates the data directory if needed and verifies the process
// can actually write to it.
func EnsureDataDir(dir string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create data dir %s: %w", dir, err)
	}
	probe := filepath.Join(dir, probeFile)
	// The path is built from operator configuration, not from request input.
	// internal/storage, which will serve request-derived paths, must never
	// suppress G304 -- that is where it earns its keep.
	//nolint:gosec // operator-supplied config path, not user input
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("data dir %s is not writable as uid %d/gid %d: %w",
			dir, os.Getuid(), os.Getgid(), err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Remove(probe)
}
