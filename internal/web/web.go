// Package web is the inbound adapter for the server-rendered UI: the surface a
// browser gets, for the times when reaching for rclone or DAVx5 is overkill.
//
// It is a consumer of the same internals as the protocol adapters and must
// never grow a private JSON API for its own use -- that is how principle 2
// erodes, one endpoint at a time. What it does have that they do not is a
// session, because a browser cannot be asked for a password on every request:
// hence a signed cookie from internal/auth, a login form to obtain one, and the
// CSRF defence a cookie-authenticated surface needs. What it renders on top of
// that is the same tree WebDAV serves -- one URL per directory, and the same
// one per file.
package web

import (
	"context"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/C0piIot/stratus-backend/internal/auth"
	"github.com/C0piIot/stratus-backend/internal/config"
	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/files"
)

// Each vendored library carries its own version in its path, which is what
// makes the far-future cache header below safe: an upgrade is a new path, never
// a stale copy somebody has to shift-reload.
const (
	assetPrefix = "/static/bootstrap-5.3.8"
	htmxPrefix  = "/static/htmx-2.0.10"
	hlsPrefix   = "/static/hls.js-1.7.3"
	// ownPrefix is where this project's own script lives, and it has no version
	// in its path because it has no version: the build's goes on as a query, so
	// the URL still changes when the bytes do and the immutable header stays
	// true.
	ownPrefix = "/static/stratus"
)

// contentSecurityPolicy is as narrow as it is because nothing is loaded from
// anywhere else. Bootstrap and htmx are embedded in the binary rather than
// fetched from a CDN, so 'self' is the whole story and no inline script or
// style is needed to tell it.
//
// connect-src is there for exactly one feature: the listing's next page is an
// hx-get, which is an XMLHttpRequest, and under default-src 'none' the browser
// refuses it before htmx sees anything. No 'unsafe-eval' beside it, which is
// the other half of the price this could have cost: every place htmx evaluates
// a string goes through its maybeEval, and the htmx-config meta in the layout
// turns that off.
//
// manifest-src and worker-src are the second and third directives here that
// exist for one feature each and fail in a browser alone: under default-src 'none' the manifest is refused
// before anything reads it, and a page with no manifest looks exactly like a
// page whose manifest was not wanted. scripts/smoke.sh asserts it, the way it
// asserts connect-src.
const contentSecurityPolicy = "default-src 'none'; style-src 'self'; script-src 'self'; " +
	"connect-src 'self'; img-src 'self' data:; manifest-src 'self'; worker-src 'self'; " +
	"form-action 'self'; frame-ancestors 'none'; base-uri 'none'"

// fileContentSecurityPolicy replaces the pages' policy on a file the browser
// opens, and filePolicy adds sandbox to it for what could carry a script. What
// it renders is not a template but a document the browser
// builds around the bytes -- an <img> or a <video> pointing at this same URL,
// centred by inline styles -- and the pages' policy blocks those styles and,
// with no media-src, the video itself. Scripts stay refused: nothing a file
// shows needs one.
const fileContentSecurityPolicy = "default-src 'none'; img-src 'self'; media-src 'self'; " +
	"style-src 'unsafe-inline'; frame-ancestors 'none'; base-uri 'none'"

// Index is what the gallery, the music pages and the search box read: the media
// index by date and by tag, what the user has said about the music, and the
// text search across both halves of the library.
type Index interface {
	db.Captures
	db.Finder
	Library
}

// PlaylistReader is the pages' view of the playlists: reading, and nothing
// else. What an edit means is internal/music's, and no page here makes one.
// It is that package's service and not the store, since #363, so the page
// and the mount at one .m3u8 address go through the same layer.
type PlaylistReader interface {
	Playlists(ctx context.Context, owner string) ([]db.Playlist, error)
	Playlist(ctx context.Context, owner string, id int64) (db.Playlist, []db.Track, error)
}

// Deps is what the handler is built from (#363): a struct rather than eleven
// positional parameters, so a test can say the one thing it varies.
type Deps struct {
	Version, BuildDate string
	Verifier           auth.Verifier
	Sessions           *auth.Sessions
	// Shares verifies the links somebody without an account can open.
	Shares *auth.Shares
	Files  *files.Service
	// Thumbs takes the blob store directly, which is why it is passed in
	// rather than built here: a derived object has no database row.
	Thumbs Thumbnails
	// Index is the media index by date and by tag and the text search.
	Index     Index
	Playlists PlaylistReader
	// Indexing is read, never driven: this surface reports on the indexer.
	Indexing Indexing
	// Imports is the import folder, and nil when there is none.
	Imports Imports
	// Playback is the film player, HLS and a track transcoded on request, and
	// a zero one offers none of them.
	Playback Playback
	// TrashRetention is how long a deletion is kept, for the pages that say
	// so, and zero when the trash is held and never emptied (#296).
	TrashRetention time.Duration
	// Calendars is read for the month pages at /calendars/, which write
	// nothing; nil means there are none.
	Calendars CalendarReader
	// Settings are the settings in force (#397), read per request so a
	// change is on the next page. Nil means the fields above are the whole
	// story, which is a test.
	Settings func() config.Values
	// Editor changes them, for the settings page; nil means there is no page.
	Editor SettingsEditor
	// Zones are the time zones this machine knows, which the settings page
	// offers TZ from; empty, it takes one as typed.
	Zones []string
	// Started is when the process started, and Database and Storage the
	// engines it runs on, for the status page: what somebody checks after a
	// deploy is that it was this one.
	Started           time.Time
	Database, Storage string
	// Sweep is what the collector's last pass did, and nil when nothing
	// reports one.
	Sweep func() Sweep
	// System is what the machine has left, and nil when nothing reports it.
	System func() System
	// Backup is the database's newest backup, and nil when nothing reports
	// one.
	Backup func() Backup
}

// Backup is the database's newest backup, as the status page tells it.
type Backup struct {
	// At and Size are the newest backup in the store, and a zero At is none.
	At   time.Time
	Size int64
	// Err is why the last attempt failed, and empty when it did not.
	Err string
	// Unsupported is a database whose driver cannot write one yet (#425).
	Unsupported bool
}

// System is the machine, for the status page. A zero or negative number is
// one that could not be read, and is not shown.
type System struct {
	// DiskFree is the data directory's filesystem, which holds the scratch
	// files whatever the blob store is; SharedDisk says the blob store is on
	// it too, so the page says one number and not the same one twice.
	DiskFree   int64
	SharedDisk bool
	// MemFree and MemTotal are the memory this process may use.
	MemFree, MemTotal int64
	CPUs              int
	// Load is the one, five and fifteen minute load average.
	Load string
	// Platform is the OS and architecture this binary was built for, Kernel
	// the release it is running on and Go the toolchain that built it.
	Platform, Kernel, Go string
}

// Sweep is one pass of the collector, as the status page tells it. A zero At
// is a pass that has not run since the process started.
type Sweep struct {
	At time.Time
	// Emptied is what left the trash, Deleted the blobs nobody owned that
	// were destroyed and Unaccounted the ones put in the trash instead;
	// Uploads the abandoned uploads, and Freed the bytes all of it gave back.
	Emptied, Deleted, Unaccounted, Uploads int
	Freed                                  int64
	// Err is the first thing that went wrong, and the rest of the pass still
	// ran: each of its parts is independent of the others.
	Err string
}

// Thumbnails is the picture half: a file's own and a folder's cover art. An
// interface like the rest of this handler's dependencies rather than
// *thumb.Service, because the answers that are not pixels -- no picture, and
// the generator at capacity -- are branches here and a test should not have to
// saturate a real decoder to reach one.
type Thumbnails interface {
	File(ctx context.Context, owner, path string, px int) (io.ReadCloser, int64, error)
	Cover(ctx context.Context, owner, dir string, px int) (io.ReadCloser, int64, error)
	// CoverOf is an album's picture by one of its own tracks, which is not
	// the folder's when the folder holds several albums.
	CoverOf(ctx context.Context, owner string, f db.File, px int) (io.ReadCloser, int64, error)
}

// Library is the music half of Index.
type Library interface {
	db.Music
	AnnotationsOf(ctx context.Context, owner string, subjects []db.Subject) (map[db.Subject]db.Annotation, error)
	// LyricsOf is what outside sources said a track's words are (#217),
	// read for the album page and written by nothing here.
	LyricsOf(ctx context.Context, fileIDs []int64) (map[int64]db.Lyrics, error)
}

type handler struct {
	version   string
	buildDate string
	verifier  auth.Verifier
	sessions  *auth.Sessions
	// shares verifies the links somebody without an account can open, and is
	// derived from the same credentials as sessions with a context string of
	// its own -- so one password revokes both at once and neither value can be
	// read as the other.
	shares *auth.Shares
	files  *files.Service
	// thumbs takes the blob store directly, which is why it is passed in rather
	// than built here: a derived object has no database row and never will.
	thumbs Thumbnails
	// indexing is read, never driven: this surface reports on the indexer and
	// has no way to start, stop or hurry it.
	indexing Indexing
	// imports is the same for the import folder, and nil when there is none.
	imports Imports
	// captures is what the two grids by date read: the index by when a camera
	// made something, rather than the tree by path.
	captures db.Captures
	// library is the same index by tag, for the music pages.
	library Library
	// finder is what the search box asks, which is neither of those two: it
	// answers across the tree and the tags at once.
	finder db.Finder
	// playlists is read for the pages at /playlists/, which write nothing.
	playlists PlaylistReader
	// video is the player and HLS, and a zero one means neither is offered.
	playback Playback
	// kept is Deps.TrashRetention.
	kept time.Duration
	// calendars is read for the month pages, and nil when there are none.
	calendars CalendarReader
	// settings are the settings in force, and nil for a handler a test built.
	settings func() config.Values
	// editor is the settings page's, and nil when there is no page.
	editor SettingsEditor
	zones  []string
	// started, database, storage and sweep are Deps' own, for the status page.
	started           time.Time
	database, storage string
	sweep             func() Sweep
	system            func() System
	backup            func() Backup
}

// Handler builds the UI. It is mounted at the root, so it is also what answers
// anything the other surfaces did not claim.
//
// The CSRF protection is wired in here rather than left to the composition
// root, unlike the Basic auth in front of WebDAV: it is meaningless on any
// other surface -- a WebDAV or Subsonic client is not a browser and sends no
// cookie -- and a caller that forgot it would lose the defence silently.
func Handler(d Deps) http.Handler {
	h := &handler{
		version: d.Version, buildDate: d.BuildDate, verifier: d.Verifier, sessions: d.Sessions, shares: d.Shares,
		files: d.Files, thumbs: d.Thumbs, captures: d.Index, library: d.Index, finder: d.Index, playlists: d.Playlists,
		indexing: d.Indexing, imports: d.Imports, playback: d.Playback, kept: d.TrashRetention,
		calendars: d.Calendars, settings: d.Settings, editor: d.Editor, zones: d.Zones,
		started: d.Started, database: d.Database, storage: d.Storage, sweep: d.Sweep, system: d.System, backup: d.Backup,
	}

	mux := http.NewServeMux()
	// One canonical URL per directory, so the root is a redirect rather than a
	// second page that lists the same thing.
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		redirectLocal(w, r, filesPrefix)
	})
	mux.HandleFunc("GET /files/{path...}", h.readable(h.browse))
	// The one thing this surface holds that another surface needs: the app
	// reads has-preview in a PROPFIND listing (#136) and fetches the tile here
	// with its WebDAV Basic. **This is the extension principle 2 warns about,
	// taken knowingly.** There is no standard way to ask a WebDAV server for a
	// preview, and what keeps this on the right side of the line is that
	// nothing depends on it: a client that does not know this URL renders no
	// thumbnails and works.
	mux.HandleFunc("GET /thumb/{path...}", h.readable(h.thumbnail))
	mux.HandleFunc("GET /status", h.signedIn(h.status))
	if h.editor != nil {
		mux.HandleFunc("GET "+settingsPath, h.signedIn(h.settingsPage))
		mux.HandleFunc("POST "+settingsPath, h.signedIn(h.saveSettings))
	}
	// Signed in and not readable: this route has no path for a share's
	// signature to be checked against, and a search over somebody's whole
	// library is not what a link to one folder authorises.
	mux.HandleFunc("GET "+searchPrefix, h.signedIn(h.search))
	mux.HandleFunc("GET "+photosPrefix+"{path...}", h.signedIn(h.byDate(photoLibrary)))
	mux.HandleFunc("GET "+videosPrefix+"{path...}", h.signedIn(h.byDate(videoLibrary)))
	mux.HandleFunc("GET "+musicPrefix+"{path...}", h.signedIn(h.music))
	mux.HandleFunc("GET "+playlistsPrefix+"{path...}", h.signedIn(h.playlistPages))
	if h.calendars != nil {
		mux.HandleFunc("GET "+calendarsPrefix+"{path...}", h.signedIn(h.calendarPages))
	}
	mux.HandleFunc("POST /files/{path...}", h.signedIn(h.upload))
	// The share sheet's door, which is the upload's twin with the destination
	// decided here rather than given. See share_target.go.
	mux.HandleFunc("POST "+shareTargetPath, h.signedIn(h.shareTarget))
	mux.HandleFunc("POST /folders/{path...}", h.signedIn(h.newFolder))
	mux.HandleFunc("GET /share/{path...}", h.signedIn(h.shareForm))
	mux.HandleFunc("POST /share/{path...}", h.signedIn(h.share))
	mux.HandleFunc("GET "+infoPrefix+"{path...}", h.signedIn(h.info))
	mux.HandleFunc("GET /rename/{path...}", h.signedIn(h.renameForm))
	mux.HandleFunc("POST /rename/{path...}", h.signedIn(h.rename))
	mux.HandleFunc("GET /delete/{path...}", h.signedIn(h.deleteForm))
	mux.HandleFunc("POST /delete/{path...}", h.signedIn(h.remove))
	mux.HandleFunc("GET "+trashPrefix+"{$}", h.signedIn(h.trash))
	// Everything under the list is the collection's (#323), which is why the
	// two forms are at prefixes of their own beside /delete/ and /rename/:
	// here a path is something somebody deleted, and the bytes are the blob
	// the deletion never moved.
	mux.HandleFunc("GET "+trashPrefix+"{path...}", h.signedIn(h.trashed))
	mux.HandleFunc("GET "+destroyPrefix+"{batch}", h.signedIn(h.destroyForm))
	mux.HandleFunc("POST "+destroyPrefix+"{batch}", h.signedIn(h.destroy))
	mux.HandleFunc("POST "+restorePrefix+"{batch}", h.signedIn(h.restore))
	mux.HandleFunc("GET /robots.txt", robots)
	// Both in front of the session, and the manifest has to be: a browser
	// fetches one with credentials omitted. See manifest.go.
	mux.HandleFunc("GET "+manifestPath, h.manifest)
	// The worker is served from the root because that is what it controls, and
	// the page it answers with has to be reachable with no network and no
	// session. See sw.go.
	mux.HandleFunc("GET "+workerPath, h.worker)
	mux.HandleFunc("GET "+offlinePath, h.offline)
	mux.HandleFunc("GET /login", h.loginForm)
	mux.HandleFunc("POST /login", h.login)
	mux.HandleFunc("POST /logout", h.logout)
	mux.Handle("GET /static/", assets())
	mux.HandleFunc("/", h.notFound)

	return secureHeaders(http.NewCrossOriginProtection().Handler(mux))
}

func (h *handler) notFound(w http.ResponseWriter, r *http.Request) {
	user, _ := h.session(r)
	h.fail(w, r, user, db.ErrNotFound)
}

// assets serves the embedded files.
func assets() http.Handler {
	files := http.FileServerFS(staticFS)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No directory listings: the file server offers one for a path ending
		// in a slash, and what is in here is nobody's business but the pages'.
		if strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		// Safe because the version is in the path: these bytes never change.
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		files.ServeHTTP(w, r)
	})
}

func secureHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		head := w.Header()
		head.Set("Content-Security-Policy", contentSecurityPolicy)
		head.Set("X-Content-Type-Options", "nosniff")
		head.Set("Referrer-Policy", "same-origin")
		h.ServeHTTP(w, r)
	})
}

// keptDays is the retention as the pages say it, and zero when the trash is
// held.
func (h *handler) keptDays() int {
	return int(h.retention() / (24 * time.Hour))
}

// retention is how long a deletion is kept, as the settings have it now.
func (h *handler) retention() time.Duration {
	if h.settings != nil {
		return h.settings().TrashRetention
	}
	return h.kept
}

// indexingNow is the indexer as the status page reports it: the reads it was
// built with, and the switches as the settings have them now.
func (h *handler) indexingNow() Indexing {
	ix := h.indexing
	if h.settings != nil {
		v := h.settings()
		ix.Interval, ix.Ahead, ix.Lookup = v.IndexInterval, v.ThumbsAhead, v.MusicLookup
	}
	return ix
}

// weekStart is the day the calendar page's weeks begin on, Monday for a
// handler a test built.
func (h *handler) weekStart() time.Weekday {
	if h.settings != nil {
		return h.settings().WeekStart
	}
	return time.Monday
}

// zone is what the calendar page draws times in: the setting's, or the
// process's own for a handler a test built.
func (h *handler) zone() *time.Location {
	if h.settings != nil && h.settings().Zone != nil {
		return h.settings().Zone
	}
	return time.Local
}
