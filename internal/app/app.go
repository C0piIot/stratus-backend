// Package app is the composition root: it wires configuration into an HTTP
// server and owns the process lifecycle. Nothing else imports it.
package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/C0piIot/stratus-backend/internal/auth"
	"github.com/C0piIot/stratus-backend/internal/config"
	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/db/postgres"
	"github.com/C0piIot/stratus-backend/internal/db/sqlite"
	"github.com/C0piIot/stratus-backend/internal/storage"
	"github.com/C0piIot/stratus-backend/internal/storage/disk"
	"github.com/C0piIot/stratus-backend/internal/storage/s3"
)

// probeTimeout bounds the self-probe used by the container healthcheck. A
// healthcheck that can hang forever is worse than none: Docker would never mark
// the container unhealthy.
const probeTimeout = 3 * time.Second

// shutdownTimeout bounds how long in-flight requests get to finish.
const shutdownTimeout = 15 * time.Second

// probeFile is written and removed at startup to prove the data dir is writable.
const probeFile = ".stratus-write-probe"

// probeKey is the same idea one layer up, in the blob store. It is a valid key
// on every backend, and it never survives startup.
const probeKey = "stratus-write-probe"

// startupTimeout bounds opening and probing the blob store. An S3 endpoint that
// accepts a connection and then says nothing would otherwise hang the process
// before it ever listens, which looks identical to a hung container.
const startupTimeout = 30 * time.Second

// App holds the wired application. Construction is pure: no I/O happens until
// Run, so Handler can be exercised from tests without touching the filesystem.
type App struct {
	cfg     config.Config
	version string
}

// Deps are the backends the protocol surfaces are built on.
//
// They are opened by Run and passed down rather than stored on App, which keeps
// New free of I/O and makes what each surface actually needs visible at its
// signature. /healthz needs nothing, so the tests for it pass a zero Deps and
// say so out loud.
type Deps struct {
	Storage  storage.Storage
	Database db.Store
}

// Close releases whatever is open, and tolerates a partly built Deps because
// that is exactly what a failed startup leaves behind.
func (d Deps) Close() error {
	var errs []error
	// Not every blob backend holds a handle; the disk one holds its root.
	if closer, ok := d.Storage.(io.Closer); ok {
		errs = append(errs, closer.Close())
	}
	if d.Database != nil {
		errs = append(errs, d.Database.Close())
	}
	return errors.Join(errs...)
}

// New wires an App. It performs no I/O.
func New(cfg config.Config, version string) *App {
	return &App{cfg: cfg, version: version}
}

// Handler builds the HTTP routes. Separate from Run so every protocol surface
// can be tested through httptest without binding a port.
//
// deps is unused while /healthz is the only route. It is threaded through now
// because WebDAV, CalDAV and the web UI all need it, and because a signature is
// a better place to state that than a comment.
func (a *App) Handler(deps Deps) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		// Nothing useful to do if the client hung up mid-write.
		_, _ = io.WriteString(w, "ok\n")
	})
	return mux
}

// Server applies the timeout policy. Separate from Run so the policy itself can
// be asserted -- see TestServerTimeouts.
func (a *App) Server(deps Deps) *http.Server {
	return &http.Server{
		Addr:              a.cfg.Addr,
		Handler:           a.Handler(deps),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// WriteTimeout is deliberately left at zero: media streaming responses
		// are long-lived and a blanket write deadline would cut them off
		// mid-file. Slowloris is covered by ReadHeaderTimeout instead.
	}
}

// Run serves until ctx is cancelled, then drains in-flight requests. Signal
// handling belongs to the caller, which keeps Run testable with a plain
// cancellable context.
func (a *App) Run(ctx context.Context) error {
	deps, err := a.open(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if err := deps.Close(); err != nil {
			slog.Error("closing backends", "err", err)
		}
	}()

	srv := a.Server(deps)
	errc := make(chan error, 1)
	go func() {
		// uid/gid are logged so the container smoke tests can assert the
		// *runtime* user: distroless has no shell to run `id` in.
		slog.Info("stratus listening",
			"version", a.version, "addr", srv.Addr, "data_dir", a.cfg.DataDir,
			"uid", os.Getuid(), "gid", os.Getgid())
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		slog.Info("shutting down")
		// A fresh context on purpose, not an oversight: ctx is already cancelled
		// -- that is why we are here -- so deriving from it would abort every
		// in-flight request immediately and defeat the graceful drain.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		return srv.Shutdown(shutdownCtx) //nolint:contextcheck // deliberately not the cancelled parent
	}
}

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

	return deps, nil
}

// checkCredentials refuses a configuration that could never authenticate
// anybody, rather than letting it surface as a login failure much later.
// Nothing authenticates yet, so unset credentials are fine; half-set ones are
// not.
func checkCredentials(cfg config.Config) error {
	switch {
	case cfg.Username == "" && cfg.PasswordHash == "":
		return nil
	case cfg.Username == "":
		return errors.New("STRATUS_PASSWORD_HASH is set but STRATUS_USERNAME is not")
	case cfg.PasswordHash == "":
		return errors.New("STRATUS_USERNAME is set but STRATUS_PASSWORD_HASH is not")
	}
	if err := auth.ValidateHash(cfg.PasswordHash.Reveal()); err != nil {
		return fmt.Errorf("STRATUS_PASSWORD_HASH: %w (produce one with `stratus hash-password`)", err)
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

// HealthURL turns a listen address into a URL reachable from inside the same
// container. A wildcard bind address is not dialable, so it maps to loopback.
func HealthURL(listenAddr string) (string, error) {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return "", err
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/healthz", nil
}

// Probe performs the container healthcheck against a locally bound listener.
func Probe(listenAddr string) error {
	url, err := HealthURL(listenAddr)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s returned %s", url, resp.Status)
	}
	return nil
}
