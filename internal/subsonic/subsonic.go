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
	"context"
	"io"
	"net/http"
	"strings"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// rootName is what the top of the folder tree is called. It is not a real
// directory -- the root has no row -- so it needs a name from somewhere, and
// this is the same one getMusicFolders answers with.
const rootName = "Music"

// Tree is what this adapter needs from internal/files: the directories the
// library model cannot answer, because a folder is not a tag, and the bytes of
// a track.
//
// Declared here rather than taking *files.Service whole, for the reason that
// package gives itself: a dependency should say what it uses.
type Tree interface {
	Stat(ctx context.Context, owner, path string) (db.File, error)
	List(ctx context.Context, owner, dir string) ([]db.File, error)
	OpenFile(ctx context.Context, f db.File) (io.ReadSeekCloser, error)
}

type handler struct {
	verifier Verifier
	// lib is the library by tag and tree is the same library by folder. Both,
	// because clients are split down the middle on which one they browse.
	lib  Library
	tree Tree
	// lists is the playlists, which are edited through a feature rather than
	// the port: see Playlists.
	lists Playlists
	art   Art
	// transcoder is nil where there is none, and then every stream is the
	// original and nothing is advertised that would say otherwise.
	transcoder Transcoder
	// serverVersion is this build, which OpenSubsonic requires in every
	// envelope so a client can notice an upgrade and ask again what it does.
	serverVersion string
}

// Handler serves the API under prefix.
//
// The prefix is stripped here rather than by the caller, for the reason the
// WebDAV adapter gives: exactly one place should know the difference between
// the path a client asks for and the method being called.
func Handler(prefix, serverVersion string, v Verifier, lib Library, tree Tree, lists Playlists, art Art, tr Transcoder) http.Handler {
	h := &handler{verifier: v, lib: lib, tree: tree, lists: lists, art: art, transcoder: tr, serverVersion: serverVersion}

	mux := http.NewServeMux()

	// The one endpoint that must answer without credentials. The specification
	// makes it mandatory and public in the same sentence: a client has to be
	// able to ask what a server supports before it can log in.
	mux.HandleFunc("GET /getOpenSubsonicExtensions", h.extensions)

	mux.HandleFunc("GET /ping", h.authed(h.ping))
	mux.HandleFunc("GET /getLicense", h.authed(h.license))
	mux.HandleFunc("GET /getMusicFolders", h.authed(h.musicFolders))
	mux.HandleFunc("GET /getUser", h.authed(h.user))

	// Browsing by tag, which is what a current client does, and by folder,
	// which is what DSub does without being told to.
	mux.HandleFunc("GET /getArtists", h.authed(h.artists))
	mux.HandleFunc("GET /getArtist", h.authed(h.artist))
	mux.HandleFunc("GET /getAlbum", h.authed(h.album))
	mux.HandleFunc("GET /getSong", h.authed(h.song))
	mux.HandleFunc("GET /getIndexes", h.authed(h.indexes))
	mux.HandleFunc("GET /getMusicDirectory", h.authed(h.musicDirectory))

	// The listings a home screen and a search box are made of. The pairs are
	// one endpoint and its predecessor: same data, older element names, for the
	// clients that ask for those.
	mux.HandleFunc("GET /getAlbumList2", h.authed(h.albumList2))
	mux.HandleFunc("GET /getAlbumList", h.authed(h.albumList))
	mux.HandleFunc("GET /search3", h.authed(h.search3))
	mux.HandleFunc("GET /search2", h.authed(h.search2))
	mux.HandleFunc("GET /getGenres", h.authed(h.genres))
	mux.HandleFunc("GET /getSongsByGenre", h.authed(h.songsByGenre))
	mux.HandleFunc("GET /getRandomSongs", h.authed(h.randomSongs))

	// What the user has said about the library. Every listing above carries
	// the same stars and ratings on each row: see annotate.
	mux.HandleFunc("GET /getStarred2", h.authed(h.starred2))
	mux.HandleFunc("GET /getStarred", h.authed(h.starred))
	mux.HandleFunc("GET /star", h.authed(h.star))
	mux.HandleFunc("GET /unstar", h.authed(h.unstar))
	mux.HandleFunc("GET /setRating", h.authed(h.setRating))
	mux.HandleFunc("GET /scrobble", h.authed(h.scrobble))

	mux.HandleFunc("GET /getPlaylists", h.authed(h.playlists))
	mux.HandleFunc("GET /getPlaylist", h.authed(h.playlist))
	mux.HandleFunc("GET /createPlaylist", h.authed(h.createPlaylist))
	mux.HandleFunc("GET /updatePlaylist", h.authed(h.updatePlaylist))
	mux.HandleFunc("GET /deletePlaylist", h.authed(h.deletePlaylist))

	// The bytes. Their errors are XML whatever f said, so they authenticate
	// through their own wrapper.
	mux.HandleFunc("GET /stream", h.authedBinary(h.stream))
	// The transcoding extension, only where there is a transcoder: see
	// transcoding.go. The decision is a POST because its body is the client.
	if tr != nil {
		mux.HandleFunc("POST /getTranscodeDecision", h.authed(h.transcodeDecision))
		mux.HandleFunc("GET /getTranscodeStream", h.authedBinary(h.transcodeStream))
	}
	mux.HandleFunc("GET /download", h.authedBinary(h.download))
	mux.HandleFunc("GET /getCoverArt", h.authedBinary(h.coverArt))

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

// authedBinary is authed for an endpoint that answers bytes: same check, and a
// refusal rendered the way the specification requires for one.
func (h *handler) authedBinary(fn func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		username, err := h.authenticate(r)
		if err != nil {
			h.failXML(w, r, *err)
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
	env.MusicFolders = &musicFolders{Folders: []musicFolder{{ID: 1, Name: rootName}}}
	h.write(w, r, env)
}

// user reports what this server can actually do. Everything absent is false,
// so a client is better told now than refused later.
func (h *handler) user(w http.ResponseWriter, r *http.Request, username string) {
	env := h.ok()
	env.User = &user{
		Username:     username,
		StreamRole:   true,
		DownloadRole: true,
		PlaylistRole: true,
		// A client that reads false here never sends a scrobble, and plays
		// have been counted since #195.
		ScrobblingEnabled: true,
	}
	h.write(w, r, env)
}

// extensions answers what this server does beyond the base protocol. The
// envelope fields and this endpoint are what OpenSubsonic requires, and every
// extension listed is one a client will start relying on the moment it reads
// it, so each is here only once it works.
//
// transcodeOffset is timeOffset honoured for a track: without it a client that
// transcodes cannot seek, and knows not to offer the bar (#50). transcoding is
// the extension where a client describes itself and the server chooses.
func (h *handler) extensions(w http.ResponseWriter, r *http.Request) {
	env := h.ok()
	list := []extension{}
	if h.transcoder != nil {
		list = append(list,
			extension{Name: "transcodeOffset", Versions: []int{1}},
			extension{Name: "transcoding", Versions: []int{1}})
	}
	env.Extensions = &list
	h.write(w, r, env)
}

func (h *handler) unknown(w http.ResponseWriter, r *http.Request) {
	h.fail(w, r, apiError{errNotFound, "this server does not implement " + strings.TrimPrefix(r.URL.Path, "/")})
}
