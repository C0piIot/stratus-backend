// Package subsonic is the inbound adapter for the OpenSubsonic API.
//
// The protocol is unlike the others this server speaks, in three ways that
// shape everything here:
//
//   - Every response is wrapped in a subsonic-response, and the *status* lives
//     inside it. An error is an HTTP 200 with a code in the body, because a
//     client that receives a transport error cannot read the reason.
//   - Credentials arrive on the query string, either as a password or as a
//     digest of one. There is no header to authenticate, so no middleware can
//     do it: see auth.go.
//   - There is no library. It is written by hand against the specification, the
//     1.16.1 schema, and what clients actually send.
package subsonic

import (
	"net/http"
	"strings"
)

type handler struct {
	verifier Verifier
	// serverVersion is this build, which OpenSubsonic requires in every
	// envelope so a client can notice an upgrade and ask again what it does.
	serverVersion string
}

// Handler serves the API under prefix.
//
// The prefix is stripped here rather than by the caller, for the reason the
// WebDAV adapter gives: exactly one place should know the difference between
// the path a client asks for and the method being called.
func Handler(prefix, serverVersion string, v Verifier) http.Handler {
	h := &handler{verifier: v, serverVersion: serverVersion}

	mux := http.NewServeMux()

	// The one endpoint that must answer without credentials. The specification
	// makes it mandatory and public in the same sentence: a client has to be
	// able to ask what a server supports before it can log in.
	mux.HandleFunc("GET /getOpenSubsonicExtensions", h.extensions)

	mux.HandleFunc("GET /ping", h.authed(h.ping))
	mux.HandleFunc("GET /getLicense", h.authed(h.license))
	mux.HandleFunc("GET /getMusicFolders", h.authed(h.musicFolders))
	mux.HandleFunc("GET /getUser", h.authed(h.user))

	// A method this server does not implement gets an envelope with an error,
	// not an HTML 404. Clients probe endpoints to decide which of their own
	// features to enable, and a page they cannot parse tells them nothing.
	mux.HandleFunc("/", h.unknown)

	return http.StripPrefix(strings.TrimSuffix(prefix, "/"), trimView(mux))
}

// trimView makes /ping and /ping.view the same endpoint. The specification
// documents the first and every one of its own examples uses the second, so a
// server that accepts only one of them is wrong about half the time.
func trimView(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if trimmed, ok := strings.CutSuffix(r.URL.Path, ".view"); ok {
			r = r.Clone(r.Context())
			r.URL.Path = trimmed
		}
		next.ServeHTTP(w, r)
	})
}

// authed wraps a handler that needs a caller. Authentication cannot be a
// middleware here because its failure has to be rendered as a payload, and only
// this package knows how.
func (h *handler) authed(fn func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		username, err := h.authenticate(r)
		if err != nil {
			h.fail(w, r, *err)
			return
		}
		fn(w, r, username)
	}
}

func (h *handler) ping(w http.ResponseWriter, r *http.Request, _ string) {
	h.write(w, r, h.ok())
}

// license answers valid, always. There is nothing to license and a client that
// believes otherwise hides half its interface -- DSub asks this before it will
// browse at all.
func (h *handler) license(w http.ResponseWriter, r *http.Request, _ string) {
	env := h.ok()
	env.License = &license{Valid: true}
	h.write(w, r, env)
}

// musicFolders answers one folder, which is the whole library. Stratus has no
// concept of separate roots and inventing several would only give clients a
// filter that filters nothing.
func (h *handler) musicFolders(w http.ResponseWriter, r *http.Request, _ string) {
	env := h.ok()
	env.MusicFolders = &musicFolders{Folders: []musicFolder{{ID: 1, Name: "Music"}}}
	h.write(w, r, env)
}

// user reports what this server can actually do. Everything absent is false:
// there is no cover art yet, nothing counts a play, and there are no playlists,
// so a client is better told now than refused later.
func (h *handler) user(w http.ResponseWriter, r *http.Request, username string) {
	env := h.ok()
	env.User = &user{
		Username:     username,
		StreamRole:   true,
		DownloadRole: true,
	}
	h.write(w, r, env)
}

// extensions answers an empty list: the envelope fields and this endpoint are
// what OpenSubsonic requires, and every extension beyond them is optional.
func (h *handler) extensions(w http.ResponseWriter, r *http.Request) {
	env := h.ok()
	env.Extensions = &[]extension{}
	h.write(w, r, env)
}

func (h *handler) unknown(w http.ResponseWriter, r *http.Request) {
	h.fail(w, r, apiError{errNotFound, "this server does not implement " + strings.TrimPrefix(r.URL.Path, "/")})
}
