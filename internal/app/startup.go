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
	"runtime"
	"time"

	"github.com/C0piIot/stratus-backend/internal/auth"
	"github.com/C0piIot/stratus-backend/internal/config"
	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/db/mysql"
	"github.com/C0piIot/stratus-backend/internal/db/postgres"
	"github.com/C0piIot/stratus-backend/internal/db/sqlite"
	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/incoming"
	"github.com/C0piIot/stratus-backend/internal/lookup"
	"github.com/C0piIot/stratus-backend/internal/media"
	"github.com/C0piIot/stratus-backend/internal/media/stream"
	"github.com/C0piIot/stratus-backend/internal/media/thumb"
	"github.com/C0piIot/stratus-backend/internal/settings"
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

	// What the web UI stored, under whatever the environment fixed (#397).
	// Read before anything else is built, since what is built next reads it.
	if deps.Settings, err = settings.New(startupCtx, func(key string) string { return a.cfg.Fixed[key] }, deps.Database); err != nil {
		return deps, fmt.Errorf("read the settings: %w", err)
	}
	if a.level != nil {
		a.level.Set(deps.Settings.Values().LogLevel)
	}

	// One file layer for the whole process, and the watcher that tells the
	// indexer about a write as it happens. The variable is read by the watcher
	// and written just below, both in this goroutine and both before Run serves
	// anything or starts a background pass, which is what makes reading it from
	// the closure safe.
	var indexer *media.Indexer
	deps.Files = files.New(deps.Storage, deps.Database, files.WithWatcher(func(f db.File) {
		if indexer != nil {
			indexer.Notice(f)
		}
	}))

	// Both tools are hard requirements rather than optional extras, and for the
	// same reason: without ffprobe a track has no duration and a video no
	// dimensions, and without ffmpeg a photograph from a phone has no picture of
	// itself. Half a media library is worse than an honest refusal to start --
	// and the thumbnail half of that is also what keeps thumb.CanThumbnail a
	// function of the name, rather than something a page has to ask about.
	ffmpeg, ferr := media.LookupFFmpeg()
	if ferr != nil {
		return deps, ferr
	}
	// The spool is the generator's as much as the indexer's, so it exists
	// whether or not anything is being indexed.
	tmp, terr := media.TempDir(a.cfg.DataDir)
	if terr != nil {
		return deps, terr
	}
	deps.Thumbs = thumb.New(deps.Storage, deps.Files, deps.Database, ffmpeg, tmp)
	deps.Scratch = tmp

	// The same ffmpeg transcodes, reading each file from a listener on the
	// loopback interface rather than a copy on disk.
	loopback, lerr := stream.NewLoopback(ctx, deps.Files)
	if lerr != nil {
		return deps, lerr
	}
	deps.Loopback = loopback
	deps.Transcoder = stream.NewTranscoder(ffmpeg, loopback)
	// Built whatever the setting says, since the setting can change: whether
	// a film is offered re-encoded is asked of it per request.
	deps.Encoder = stream.NewEncoder(deps.Transcoder, deps.Storage)
	if on, why := videoEncoding(deps.Settings.Values().VideoTranscode, runtime.GOMAXPROCS(0)); on {
		slog.Info("films are also offered re-encoded to H.264", "because", why)
	} else {
		slog.Info("films are offered as they are and remuxed, never re-encoded", "because", why)
	}

	// The indexer is built with indexing off too, and waits for the setting:
	// which is why ffprobe is looked up whatever it says.
	ffprobe, perr := media.LookupFFprobe()
	if perr != nil {
		return deps, perr
	}
	indexer = media.NewIndexer(deps.Files, deps.Database, tmp, ffprobe)
	deps.Indexer = indexer

	// The import folder needs somewhere to file things under, which is the
	// single user: with none configured there is no owner and nothing to do.
	// Its path is the environment's alone; how often it is swept is a setting.
	if a.cfg.IncomingDir != "" && credentials(a.cfg).Configured() {
		deps.Incoming = incoming.New(a.cfg.IncomingDir, a.cfg.Username, deps.Files)
	}

	// The third source of an album's cover, read by every surface that serves
	// one: the rows are there whether or not the pass that fills them runs.
	// The pass is built whenever there is somebody's library to look at, and
	// asks nothing while STRATUS_MUSIC_LOOKUP is off.
	deps.Thumbs.WithLookups(deps.Database)
	if credentials(a.cfg).Configured() {
		deps.Lookup = lookup.New(deps.Database, deps.Thumbs, a.version)
	}

	if credentials(a.cfg).Configured() {
		slog.Info("webdav ready", "prefix", filesPrefix, "user", a.cfg.Username)
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

// videoEncoding decides whether films are re-encoded here, and says why.
//
// Auto is four CPUs or more. libx264 on one or two cores makes a segment of
// 1080p more slowly than it plays, so a player would stall on every one of
// them -- worse than being handed a remux its device may refuse, which at
// least fails at once. Four is where veryfast keeps ahead with a core to
// spare for everything else.
func videoEncoding(mode string, cpus int) (bool, string) {
	switch mode {
	case config.VideoTranscodeOn:
		return true, "STRATUS_VIDEO_TRANSCODE is on"
	case config.VideoTranscodeOff:
		return false, "STRATUS_VIDEO_TRANSCODE is off"
	}
	if cpus >= 4 {
		return true, fmt.Sprintf("auto, and %d CPUs keep ahead of a film", cpus)
	}
	return false, fmt.Sprintf("auto, and %d CPUs would fall behind a film", cpus)
}
