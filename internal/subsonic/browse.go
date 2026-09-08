package subsonic

import (
	"errors"
	"log/slog"
	"net/http"
	"path"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// artists is getArtists: the library by tag, which is how every current client
// browses.
func (h *handler) artists(w http.ResponseWriter, r *http.Request, username string) {
	list, err := h.lib.Artists(r.Context(), username)
	if err != nil {
		h.fail(w, r, h.internal(r, "list artists", err))
		return
	}

	refs := make([]artistRef, 0, len(list))
	for _, a := range list {
		refs = append(refs, artistRef{ID: artistID(a.Name), Name: a.Name, AlbumCount: a.AlbumCount})
	}

	env := h.ok()
	env.Artists = &artistsList{Indexes: indexArtists(refs)}
	h.write(w, r, env)
}

// artist is getArtist: one artist and its albums.
func (h *handler) artist(w http.ResponseWriter, r *http.Request, username string) {
	id, apiErr := requiredID(r)
	if apiErr != nil {
		h.fail(w, r, *apiErr)
		return
	}
	name, ok := parseArtistID(id)
	if !ok {
		h.fail(w, r, notFound("artist"))
		return
	}

	albums, err := h.lib.Albums(r.Context(), username, name)
	if err != nil {
		h.fail(w, r, h.internal(r, "list albums", err))
		return
	}
	// An artist is not a row: it exists exactly as long as it has an album, so
	// none is the same answer as no such artist.
	if len(albums) == 0 {
		h.fail(w, r, notFound("artist"))
		return
	}

	detail := artistDetail{
		ID:         artistID(name),
		Name:       name,
		AlbumCount: len(albums),
		Albums:     make([]albumRef, 0, len(albums)),
	}
	for _, a := range albums {
		detail.Albums = append(detail.Albums, albumOf(a))
	}

	env := h.ok()
	env.Artist = &detail
	h.write(w, r, env)
}

// album is getAlbum: one album and its tracks.
//
// Two queries rather than one, and the aggregate is not recomputed here on
// purpose. The song count, the duration and the year come from the same query
// that produced them for the listing the client came from, so the two views
// cannot disagree -- "the lexicographically largest genre of its tracks" is not
// a rule worth implementing twice.
func (h *handler) album(w http.ResponseWriter, r *http.Request, username string) {
	id, apiErr := requiredID(r)
	if apiErr != nil {
		h.fail(w, r, *apiErr)
		return
	}
	artist, name, ok := parseAlbumID(id)
	if !ok {
		h.fail(w, r, notFound("album"))
		return
	}

	albums, err := h.lib.Albums(r.Context(), username, artist)
	if err != nil {
		h.fail(w, r, h.internal(r, "list albums", err))
		return
	}
	found := -1
	for i := range albums {
		if albums[i].Name == name {
			found = i
			break
		}
	}
	if found < 0 {
		h.fail(w, r, notFound("album"))
		return
	}

	tracks, err := h.lib.Tracks(r.Context(), username, artist, name)
	if err != nil {
		h.fail(w, r, h.internal(r, "list tracks", err))
		return
	}

	detail := albumDetail{albumRef: albumOf(albums[found]), Songs: make([]child, 0, len(tracks))}
	for _, t := range tracks {
		detail.Songs = append(detail.Songs, songOf(t))
	}

	env := h.ok()
	env.Album = &detail
	h.write(w, r, env)
}

// song is getSong, which a client calls to refresh one row it has cached.
func (h *handler) song(w http.ResponseWriter, r *http.Request, username string) {
	id, apiErr := requiredID(r)
	if apiErr != nil {
		h.fail(w, r, *apiErr)
		return
	}
	t, apiErr := h.audioTrack(r, username, id)
	if apiErr != nil {
		h.fail(w, r, *apiErr)
		return
	}

	c := songOf(t)
	env := h.ok()
	env.Song = &c
	h.write(w, r, env)
}

// indexes is getIndexes: the top of the folder tree.
//
// It is not optional, and it is the half Navidrome got wrong. DSub browses by
// folder unless its preferences are changed, and Symfonium has a folder view.
// Manufacturing folders out of tags is what breaks them -- and this server has
// no need to, because it has a real file tree underneath.
//
// musicFolderId is ignored because there is one folder, and ifModifiedSince is
// ignored because answering an unchanged listing costs two indexed queries.
func (h *handler) indexes(w http.ResponseWriter, r *http.Request, username string) {
	entries, err := h.tree.List(r.Context(), username, "")
	if err != nil {
		h.fail(w, r, h.internal(r, "list the root", err))
		return
	}
	songs, err := h.lib.TracksIn(r.Context(), username, "")
	if err != nil {
		h.fail(w, r, h.internal(r, "list the tracks at the root", err))
		return
	}

	var newest time.Time
	refs := make([]artistRef, 0, len(entries))
	for _, f := range entries {
		if !f.IsDir {
			continue
		}
		refs = append(refs, artistRef{ID: dirID(f.Path), Name: path.Base(f.Path)})
		if f.MTime.After(newest) {
			newest = f.MTime
		}
	}

	kids := make([]child, 0, len(songs))
	for _, t := range songs {
		kids = append(kids, songOf(t))
		if t.File.MTime.After(newest) {
			newest = t.File.MTime
		}
	}

	list := &indexes{Indexes: indexArtists(refs), Children: kids}
	// A zero time is not a timestamp, and UnixMilli of one is a large negative
	// number a client would cache against forever.
	if !newest.IsZero() {
		list.LastModified = newest.UnixMilli()
	}

	env := h.ok()
	env.Indexes = list
	h.write(w, r, env)
}

// musicDirectory is getMusicDirectory: one folder, with its subfolders and the
// tracks in it. Files that are not audio are left out -- they exist, over
// WebDAV, but a music client has nothing to do with them.
func (h *handler) musicDirectory(w http.ResponseWriter, r *http.Request, username string) {
	id, apiErr := requiredID(r)
	if apiErr != nil {
		h.fail(w, r, *apiErr)
		return
	}
	dir, ok := parseDirID(id)
	if !ok {
		h.fail(w, r, notFound("directory"))
		return
	}

	answer := directory{ID: dirID(dir), Name: rootName}
	if dir != "" {
		// Statted rather than inferred from an empty listing, because an empty
		// folder is a real thing here: WebDAV can make one, and answering "no
		// such directory" for it would be a lie.
		f, serr := h.tree.Stat(r.Context(), username, dir)
		switch {
		case errors.Is(serr, db.ErrNotFound):
			h.fail(w, r, notFound("directory"))
			return
		case serr != nil:
			h.fail(w, r, h.internal(r, "stat a directory", serr))
			return
		case !f.IsDir:
			h.fail(w, r, notFound("directory"))
			return
		}
		answer.Name = path.Base(dir)
		answer.Parent = dirID(db.ParentOf(dir))
	}

	entries, err := h.tree.List(r.Context(), username, dir)
	if err != nil {
		h.fail(w, r, h.internal(r, "list a directory", err))
		return
	}
	songs, err := h.lib.TracksIn(r.Context(), username, dir)
	if err != nil {
		h.fail(w, r, h.internal(r, "list the tracks in a directory", err))
		return
	}

	answer.Children = make([]child, 0, len(entries))
	for _, f := range entries {
		if f.IsDir {
			answer.Children = append(answer.Children, folderOf(f))
		}
	}
	for _, t := range songs {
		answer.Children = append(answer.Children, songOf(t))
	}

	env := h.ok()
	env.Directory = &answer
	h.write(w, r, env)
}

// audioTrack resolves a song id, and refuses everything that is not one: an id
// a client invented, a file that belongs to somebody else, and a file that is
// not audio. All of them are "not found", because from the client's side they
// are: no id this server handed out names any of those.
func (h *handler) audioTrack(r *http.Request, username, id string) (db.Track, *apiError) {
	fileID, ok := parseSongID(id)
	if !ok {
		return db.Track{}, ptr(notFound("song"))
	}

	t, err := h.lib.TrackByFile(r.Context(), username, fileID)
	switch {
	case errors.Is(err, db.ErrNotFound):
		return db.Track{}, ptr(notFound("song"))
	case err != nil:
		return db.Track{}, ptr(h.internal(r, "get a track", err))
	case t.Media.Kind != db.KindAudio:
		return db.Track{}, ptr(notFound("song"))
	}
	return t, nil
}

// requiredID reads the parameter every call that names something carries.
func requiredID(r *http.Request) (string, *apiError) {
	id := r.URL.Query().Get("id")
	if id == "" {
		return "", &apiError{errMissingParam, "the id parameter is required"}
	}
	return id, nil
}

func notFound(what string) apiError {
	return apiError{errNotFound, "no such " + what}
}

// internal is what a failing backend becomes.
//
// The reason is logged and never served, which is the rule the readiness check
// already follows: a driver error can carry the host it could not reach, and a
// client's log is not the place for it. The protocol has no code for "my fault"
// either, so it is the generic one with a message that does not pretend the
// request was wrong.
func (h *handler) internal(r *http.Request, what string, err error) apiError {
	slog.ErrorContext(r.Context(), "subsonic: cannot "+what, "err", err)
	return apiError{errGeneric, "the server could not answer that"}
}

func ptr(e apiError) *apiError { return &e }
