// Package app is the composition root: it wires configuration into an HTTP
// server and owns the process lifecycle. Nothing else imports it.
package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/C0piIot/stratus-backend/internal/auth"
	"github.com/C0piIot/stratus-backend/internal/config"
	"github.com/C0piIot/stratus-backend/internal/dav"
	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/media"
	"github.com/C0piIot/stratus-backend/internal/storage"
	"github.com/C0piIot/stratus-backend/internal/subsonic"
	"github.com/C0piIot/stratus-backend/internal/tus"
	"github.com/C0piIot/stratus-backend/internal/web"
)

// shutdownTimeout bounds how long in-flight requests get to finish.
const shutdownTimeout = 15 * time.Second

// davPrefix is where the file surface lives. Not "/" so that the web UI and the
// other protocol surfaces have somewhere to go later.
const davPrefix = "/dav/"

// davRealm is what a client shows when it asks for a password.
const davRealm = "Stratus"

// tusPrefix is where resumable uploads live. A prefix of its own rather than a
// corner of davPrefix: every upload in progress has a URL there, and those are
// ids rather than paths in the tree.
const tusPrefix = "/tus/"

// subsonicPrefix is where the music surface lives. Unlike davPrefix this is not
// ours to choose: every Subsonic client appends /rest/<method> to whatever base
// URL it is given.
const subsonicPrefix = "/rest/"

// App holds the wired application. Construction is pure: no I/O happens until
// Run, so Handler can be exercised from tests without touching the filesystem.
type App struct {
	cfg       config.Config
	version   string
	buildDate string
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
	// Files is the one file layer this process has, shared by every surface,
	// the sweep and the indexer. One rather than one each because it carries
	// the watcher that tells the indexer a file has just landed: a second
	// instance would be a second half of the system whose writes nobody hears.
	Files *files.Service
	// Thumbs makes the pictures every surface offers, and is built here rather
	// than per surface because it holds the path to ffmpeg and the directory a
	// blob is put down in for it.
	Thumbs *media.Thumbs
	// Indexer is nil when there is nothing to index into, which today means no
	// credentials and therefore no files.
	Indexer *media.Indexer
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
func New(cfg config.Config, version, buildDate string) *App {
	return &App{cfg: cfg, version: version, buildDate: buildDate}
}

// Handler builds the HTTP routes. Separate from Run so every protocol surface
// can be tested through httptest without binding a port.
func (a *App) Handler(deps Deps) http.Handler {
	mux := http.NewServeMux()

	// Liveness. It touches nothing on purpose -- see readiness in health.go for
	// the endpoint that does, and why they are two.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		// Nothing useful to do if the client hung up mid-write.
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("GET /readyz", readiness(deps))

	// No credentials, no file surface. Refusing to mount it is clearer than
	// mounting something that answers 401 to everyone, and it means an install
	// that has not been configured yet cannot be a WebDAV server by accident.
	if creds := credentials(a.cfg); creds.Configured() && deps.Files != nil {
		service := deps.Files
		// One throttle for the whole surface, built here so that its counters
		// are shared rather than reset per request.
		verifier := auth.NewThrottle(creds, auth.DefaultThrottle)
		// A share link is the other way in, and only for a plain read: the app
		// needs a URL a Chromecast can fetch, and this is the surface it
		// already speaks. See internal/dav/signed.go.
		shares := auth.NewShares(creds)
		mux.Handle(davPrefix, dav.SignedLinks(davPrefix, shares,
			auth.Basic(davRealm, verifier, dav.Handler(davPrefix, service))))
		// The same realm and the same throttle: it is the same credentials, and
		// a second budget of guesses would be a second way in.
		mux.Handle(tusPrefix, auth.Basic(davRealm, verifier, tus.Handler(tusPrefix, service)))
		// The same verifier, deliberately. Subsonic authenticates per request
		// from the query string rather than through auth.Basic, and a second
		// NewThrottle here would give an attacker a second budget of guesses at
		// the one password this server has.
		// Thumbnails are made from the blob store and kept in it, so the
		// generator takes the store directly: a derived object has no database
		// row and never will.
		thumbs := deps.Thumbs
		mux.Handle(subsonicPrefix,
			subsonic.Handler(subsonicPrefix, a.version, verifier, deps.Database, service, thumbs))

		// The browser surface, at the root, so everything the prefixes above did
		// not claim is a page rather than a bare 404. Same verifier again, and
		// a session signed with the configured password: see auth.Sessions for
		// what that buys and what it costs.
		mux.Handle("/", web.Handler(a.version, a.buildDate, verifier,
			auth.NewSessions(creds, auth.DefaultSessionTTL), shares, service, thumbs,
			web.Indexing{Index: deps.Database, Interval: a.cfg.IndexInterval}))
	}
	// The log is outside the compression so that the bytes it counts are the
	// bytes that went out rather than the ones the handler wrote.
	return logRequests(compress(mux))
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

	// Background work is a goroutine in this process, not a queue somewhere
	// else. It stops with the context and is waited for before Run returns, so
	// a shutdown never leaves a half-finished sweep behind.
	var background sync.WaitGroup
	if a.cfg.GCInterval > 0 {
		background.Add(1)
		go func() {
			defer background.Done()
			a.collectPeriodically(ctx, deps)
		}()
	} else {
		slog.Warn("orphan blob collection disabled", "reason", "STRATUS_GC_INTERVAL is zero")
	}

	if a.cfg.IndexInterval > 0 && deps.Indexer != nil {
		background.Add(1)
		go func() {
			defer background.Done()
			a.indexPeriodically(ctx, deps)
		}()
	} else {
		slog.Warn("media indexing disabled", "reason", "STRATUS_INDEX_INTERVAL is zero")
	}
	defer background.Wait()

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
