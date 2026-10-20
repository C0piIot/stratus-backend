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
	"strings"
	"sync"
	"time"

	"github.com/C0piIot/stratus-backend/internal/auth"
	"github.com/C0piIot/stratus-backend/internal/calendar"
	"github.com/C0piIot/stratus-backend/internal/config"
	"github.com/C0piIot/stratus-backend/internal/dav"
	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/incoming"
	"github.com/C0piIot/stratus-backend/internal/lookup"
	"github.com/C0piIot/stratus-backend/internal/media"
	"github.com/C0piIot/stratus-backend/internal/media/stream"
	"github.com/C0piIot/stratus-backend/internal/media/thumb"
	"github.com/C0piIot/stratus-backend/internal/music"
	"github.com/C0piIot/stratus-backend/internal/storage"
	"github.com/C0piIot/stratus-backend/internal/subsonic"
	"github.com/C0piIot/stratus-backend/internal/tus"
	"github.com/C0piIot/stratus-backend/internal/web"
)

// shutdownTimeout bounds how long in-flight requests get to finish.
const shutdownTimeout = 15 * time.Second

// filesPrefix is where the file surface lives, and it is **one prefix for two
// protocols** (#279): the same URL the web UI has always served a directory
// and a file at now answers WebDAV as well. What tells the two apart is the
// method and nothing else -- a browser never sends PROPFIND, and a WebDAV
// client never asks for a listing with GET -- so there is no negotiation here
// and no header to sniff.
const filesPrefix = "/files/"

// tusPrefix is where resumable uploads live. A prefix of its own rather than a
// corner of davPrefix: every upload in progress has a URL there, and those are
// ids rather than paths in the tree.
const tusPrefix = "/tus/"

// subsonicPrefix is where the music surface lives. Unlike davPrefix this is not
// ours to choose: every Subsonic client appends /rest/<method> to whatever base
// URL it is given.
const subsonicPrefix = "/rest/"

// photosPrefix is where photos are served by year and month, a mount of its
// own for the reason playlistsPrefix is one (#213) -- and one address for both
// protocols since #279: the pages and the collection are the same URLs, split
// by method below.
const photosPrefix = "/photos/"

// videosPrefix is where the videos are served by date, the photographs'
// sibling (#215): same tree, same split by method, a different kind in it.
const videosPrefix = "/videos/"

// musicPrefix is where the library is served by tag, a mount of its own for
// the reason playlistsPrefix is one, and one address for both protocols
// (#279): the music pages and the collection are the same URLs.
const musicPrefix = "/music/"

// playlistsPrefix is where playlists are served as .m3u8 files. A mount of its
// own and not a folder under filesPrefix: that tree is the user's, and a
// generated file there could collide with a real one (#203). Both protocols
// here too since #279: the file for a player, the page for a person.
const playlistsPrefix = "/playlists/"

// trashPrefix is what has been deleted, as collections (#323), and one address
// for both protocols like the rest: the list of deletions to a browser, a
// collection per deletion to a client that mounts the server.
const trashPrefix = "/trash/"

// calendarsPrefix is where calendars are served over CalDAV (#75). A mount of
// its own like the generated ones, with no browser half yet: a GET there is an
// .ics, which is the protocol's own answer, until the calendar view (#111).
const calendarsPrefix = "/calendars/"

// wellKnownCalDAV is where a client starts when it was given only the origin
// (RFC 6764), which is what DAVx5 is given.
const wellKnownCalDAV = "/.well-known/caldav"

// App holds the wired application. Construction is pure: no I/O happens until
// Run, so Handler can be exercised from tests without touching the filesystem.
type App struct {
	cfg       config.Config
	version   string
	buildDate string
	// lookupPause is the breath between two lookup passes that both filled
	// a batch: a field set from the constant, so a test can watch the loop
	// come back without standing there for it.
	lookupPause time.Duration
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
	Thumbs *thumb.Service
	// Transcoder turns a track into what a client asked for (#50), reading it
	// through Loopback, which is the listener on 127.0.0.1 that hands a blob to
	// ffmpeg without copying it first.
	Transcoder *stream.Transcoder
	Loopback   *stream.Loopback
	// Encoder re-encodes films to H.264, and is nil where this machine is not
	// to: see videoEncoding.
	Encoder *stream.Encoder
	// Indexer is nil when there is nothing to index into, which today means no
	// credentials and therefore no files.
	Indexer *media.Indexer
	// Incoming sweeps the import folder, and is nil when none is configured --
	// which is the default, and also what no credentials means, since an
	// import needs somebody to file things under.
	Incoming *incoming.Watcher
	// Lookup asks outside sources about the music library (#217), and is nil
	// unless STRATUS_MUSIC_LOOKUP is on: nothing leaves the machine by
	// default.
	Lookup *lookup.Service
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
	if d.Loopback != nil {
		errs = append(errs, d.Loopback.Close())
	}
	return errors.Join(errs...)
}

// New wires an App. It performs no I/O.
func New(cfg config.Config, version, buildDate string) *App {
	return &App{cfg: cfg, version: version, buildDate: buildDate, lookupPause: lookupPause}
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
		// The web UI's session opens every surface, so a page can use the
		// protocols rather than grow an API of its own: see auth.Session, and
		// why the cookie counts only on a request the browser calls its own.
		sessions := auth.NewSessions(creds, auth.DefaultSessionTTL)
		playlists := music.New(deps.Database)
		browser := web.Handler(web.Deps{
			Version: a.version, BuildDate: a.buildDate, Verifier: verifier, Sessions: sessions, Shares: shares,
			Files: service, Thumbs: deps.Thumbs, Index: deps.Database, Playlists: playlists,
			Indexing: web.Indexing{
				Index: deps.Database, Interval: a.cfg.IndexInterval, Ahead: a.cfg.ThumbsAhead,
				Lookups: deps.Database, Lookup: a.cfg.MusicLookup,
			},
			Imports: imports(deps), Video: films(deps), TrashRetention: a.cfg.TrashRetention,
		})

		// Every protocol surface is behind the same realm and the same
		// throttle -- it is the same credentials, and a second budget of
		// guesses would be a second way in -- and the session opens each of
		// them for the UI's own pages (#234).
		guard := func(h http.Handler) http.Handler {
			return auth.Session(sessions, auth.Basic(auth.Realm, verifier, h))
		}
		// One URL, two protocols, split by method (#279): a WebDAV method is
		// the collection's, everything else the browser's. The browser half is
		// the handler mounted at "/" below -- it already routes every prefix
		// and is handed the whole path. A signed link needs no wrapper here:
		// it authorises GET and HEAD, which are the browser's side of the
		// split, and the page that verifies it is the one that serves them.
		collection := func(h http.Handler) http.Handler { return davOrBrowser(guard(h), browser) }

		mux.Handle(filesPrefix, collection(dav.Handler(filesPrefix, service, deps.Database)))
		// tus is the one surface with no browser half.
		mux.Handle(tusPrefix, guard(tus.Handler(tusPrefix, filesPrefix, service)))
		// The same verifier, deliberately. Subsonic authenticates per request
		// from the query string rather than through auth.Basic, and a second
		// NewThrottle here would give an attacker a second budget of guesses at
		// the one password this server has.
		// Thumbnails are made from the blob store and kept in it, so the
		// generator takes the store directly: a derived object has no database
		// row and never will.
		thumbs := deps.Thumbs
		// Subsonic authenticates per request from the query string rather
		// than through auth.Basic, so it takes the session alone and the
		// verifier itself.
		mux.Handle(subsonicPrefix, auth.Session(sessions,
			subsonic.Handler(subsonicPrefix, a.version, verifier, deps.Database, service, playlists, thumbs, transcoder(deps))))
		// The generated collections, each one address and two protocols: the
		// .m3u8 and the page it describes (#203), the library by tag whose
		// browser half is the music pages (#279), what has been deleted beside
		// the collections it was deleted from (#323), and what a camera made
		// by date -- the same handler over the same tree with a different kind
		// in it (#213, #215). Every one's byte-serving half is the browser's.
		mux.Handle(playlistsPrefix, collection(dav.Playlists(playlistsPrefix, filesPrefix, playlists)))
		mux.Handle(musicPrefix, collection(dav.Music(musicPrefix, deps.Database)))
		mux.Handle(trashPrefix, collection(dav.Trash(trashPrefix, service)))
		mux.Handle(photosPrefix, collection(dav.ByDate(photosPrefix, deps.Database, db.KindImage)))
		mux.Handle(videosPrefix, collection(dav.ByDate(videosPrefix, deps.Database, db.KindVideo)))
		// Calendars have no browser half, so nothing splits them by method.
		// The redirect asks for no credentials: it says where to look and
		// nothing about what is there, and it keeps the method, which is a
		// PROPFIND when a client is discovering.
		// Both spellings, or the mux answers the bare one with a redirect that a
		// client follows as a GET rather than the PROPFIND it was.
		calendars := guard(dav.CalDAV(calendarsPrefix, calendar.New(deps.Database)))
		mux.Handle(calendarsPrefix, calendars)
		mux.Handle(strings.TrimSuffix(calendarsPrefix, "/"), calendars)
		mux.Handle(wellKnownCalDAV, http.RedirectHandler(calendarsPrefix, http.StatusPermanentRedirect))

		// The browser surface, at the root, so everything the prefixes above did
		// not claim is a page rather than a bare 404 -- and in front of it, the
		// collection the whole server is (#279): a WebDAV method at the origin
		// is answered by a synthetic listing of the mounts below it, which is
		// also what the Windows redirector probes before it will mount anything
		// at all (#281).
		mux.Handle("/", collection(dav.Root("/", dav.Searchable("/", filesPrefix, deps.Database),
			"calendars", "files", "music", "photos", "playlists", "trash", "videos")))
	}
	// The log is outside the compression so that the bytes it counts are the
	// bytes that went out rather than the ones the handler wrote, and the
	// recover outside both so that a panic in either is caught too.
	return recoverPanics(logRequests(compress(mux)))
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
	start := func(task string, loop func(context.Context, Deps)) {
		background.Add(1)
		go func() {
			defer background.Done()
			keepRunning(ctx, task, restartPause, func() { loop(ctx, deps) })
		}()
	}
	if a.cfg.GCInterval > 0 {
		start("collecting orphan blobs", a.collectPeriodically)
	} else {
		slog.Warn("orphan blob collection disabled", "reason", "STRATUS_GC_INTERVAL is zero")
	}
	if a.cfg.IndexInterval > 0 && deps.Indexer != nil {
		start("indexing media", a.indexPeriodically)
	} else {
		slog.Warn("media indexing disabled", "reason", "STRATUS_INDEX_INTERVAL is zero")
	}
	if deps.Incoming != nil {
		start("importing what arrived on disk", a.importPeriodically)
	}
	if deps.Lookup != nil {
		start("looking up music metadata", a.lookupPeriodically)
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

// transcoder is deps.Transcoder as the interface Subsonic takes, and a nil
// interface rather than a nil pointer inside one when there is none: the
// adapter checks for nil to decide whether to offer transcoding at all.
func transcoder(deps Deps) subsonic.Transcoder {
	if deps.Transcoder == nil {
		return nil
	}
	return deps.Transcoder
}

// imports is what the status page reports the import folder from, and nil when
// there is no folder. A nil interface rather than a nil pointer inside one, for
// the reason transcoder gives.
func imports(deps Deps) web.Imports {
	if deps.Incoming == nil {
		return nil
	}
	return deps.Incoming
}

// films is what the web UI plays and streams as HLS with: the media rows and
// the transcoder, or nothing at all when there is no transcoder to remux with.
func films(deps Deps) web.Video {
	if deps.Transcoder == nil || deps.Database == nil {
		return web.Video{}
	}
	v := web.Video{Media: deps.Database, Segments: deps.Transcoder}
	// A nil interface rather than a nil pointer inside one, for the reason
	// transcoder gives.
	if deps.Encoder != nil {
		v.Encoded = deps.Encoder
	}
	return v
}

// davOrBrowser sends a request to the WebDAV handler or to the browser one,
// by method.
//
// The methods a browser can produce go to the pages; everything else is
// WebDAV's. PUT and DELETE are on the WebDAV side without ambiguity because
// the UI uploads and deletes with a form POST, and OPTIONS is there so that
// what a client is told about the resource comes from the half that speaks
// the protocol.
//
// It lives here because inbound adapters do not import each other, which is
// the same reason hlsOr does, and because deciding which protocol a request
// is in is wiring rather than either adapter's business.
func davOrBrowser(dav, browser http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodPost:
			browser.ServeHTTP(w, r)
		default:
			dav.ServeHTTP(w, r)
		}
	})
}
