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
	"net/http"
	"strings"

	"github.com/C0piIot/stratus-backend/internal/auth"
	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/media"
)

// Each vendored library carries its own version in its path, which is what
// makes the far-future cache header below safe: an upgrade is a new path, never
// a stale copy somebody has to shift-reload.
const (
	assetPrefix = "/static/bootstrap-5.3.8"
	htmxPrefix  = "/static/htmx-2.0.10"
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
const contentSecurityPolicy = "default-src 'none'; style-src 'self'; script-src 'self'; " +
	"connect-src 'self'; img-src 'self' data:; form-action 'self'; frame-ancestors 'none'; base-uri 'none'"

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
	thumbs *media.Thumbs
	// indexing is read, never driven: this surface reports on the indexer and
	// has no way to start, stop or hurry it.
	indexing Indexing
}

// Handler builds the UI. It is mounted at the root, so it is also what answers
// anything the other surfaces did not claim.
//
// The CSRF protection is wired in here rather than left to the composition
// root, unlike the Basic auth in front of WebDAV: it is meaningless on any
// other surface -- a WebDAV or Subsonic client is not a browser and sends no
// cookie -- and a caller that forgot it would lose the defence silently.
func Handler(version, buildDate string, v auth.Verifier, s *auth.Sessions, shares *auth.Shares,
	service *files.Service, thumbs *media.Thumbs, indexing Indexing,
) http.Handler {
	h := &handler{
		version: version, buildDate: buildDate, verifier: v, sessions: s, shares: shares,
		files: service, thumbs: thumbs, indexing: indexing,
	}

	mux := http.NewServeMux()
	// One canonical URL per directory, so the root is a redirect rather than a
	// second page that lists the same thing.
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		redirectLocal(w, r, filesPrefix)
	})
	mux.HandleFunc("GET /files/{path...}", h.readable(h.browse))
	mux.HandleFunc("GET /thumb/{path...}", h.withPicture(h.thumbnail))
	mux.HandleFunc("GET /status", h.signedIn(h.status))
	mux.HandleFunc("POST /files/{path...}", h.signedIn(h.upload))
	mux.HandleFunc("POST /folders/{path...}", h.signedIn(h.newFolder))
	mux.HandleFunc("GET /share/{path...}", h.signedIn(h.shareForm))
	mux.HandleFunc("POST /share/{path...}", h.signedIn(h.share))
	mux.HandleFunc("GET /rename/{path...}", h.signedIn(h.renameForm))
	mux.HandleFunc("POST /rename/{path...}", h.signedIn(h.rename))
	mux.HandleFunc("GET /delete/{path...}", h.signedIn(h.deleteForm))
	mux.HandleFunc("POST /delete/{path...}", h.signedIn(h.remove))
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
