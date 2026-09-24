package subsonic

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/music"
)

// Playlists is what this adapter needs from internal/music. An edit is a read
// and a write in one transaction, which is why it is a feature's and not a
// port's, and why this adapter never computes an index itself.
type Playlists interface {
	Playlists(ctx context.Context, owner string) ([]db.Playlist, error)
	Playlist(ctx context.Context, owner string, id int64) (db.Playlist, []db.Track, error)
	Create(ctx context.Context, owner, name string, fileIDs []int64) (db.Playlist, error)
	Replace(ctx context.Context, owner string, id int64, name string, fileIDs []int64) (db.Playlist, error)
	Update(ctx context.Context, owner string, id int64, e music.Edit) error
	Delete(ctx context.Context, owner string, id int64) error
}

// playlistRef is a playlist without its entries, which is what a listing shows.
// No coverArt: nothing here would fill it, and a declared field is a claim.
type playlistRef struct {
	ID      string `xml:"id,attr" json:"id"`
	Name    string `xml:"name,attr" json:"name"`
	Comment string `xml:"comment,attr,omitempty" json:"comment,omitempty"`
	Owner   string `xml:"owner,attr" json:"owner"`
	// Public carries no omitempty: false is an answer.
	Public    bool   `xml:"public,attr" json:"public"`
	SongCount int    `xml:"songCount,attr" json:"songCount"`
	Duration  int    `xml:"duration,attr" json:"duration"`
	Created   string `xml:"created,attr" json:"created"`
	Changed   string `xml:"changed,attr" json:"changed"`
}

type playlistsList struct {
	Playlists []playlistRef `xml:"playlist" json:"playlist"`
}

// playlistDetail is getPlaylist, and what createPlaylist answers with.
type playlistDetail struct {
	playlistRef
	Entries []child `xml:"entry" json:"entry"`
}

func playlistOf(p db.Playlist) playlistRef {
	return playlistRef{
		ID:        playlistID(p.ID),
		Name:      p.Name,
		Comment:   p.Comment,
		Owner:     p.OwnerID,
		Public:    p.Public,
		SongCount: p.SongCount,
		Duration:  seconds(p.DurationMS),
		Created:   stamp(p.Created),
		Changed:   stamp(p.Changed),
	}
}

// playlists is getPlaylists. A username that is not the caller's is refused
// rather than answered with nothing: there is one user, and "you may not see
// theirs" is the truth the day there are two.
func (h *handler) playlists(w http.ResponseWriter, r *http.Request, username string) {
	if other := r.URL.Query().Get("username"); other != "" && other != username {
		h.fail(w, r, apiError{errNotAuthorized, "only your own playlists can be listed"})
		return
	}
	list, err := h.lists.Playlists(r.Context(), username)
	if err != nil {
		h.fail(w, r, h.internal(r, "list playlists", err))
		return
	}

	answer := playlistsList{Playlists: make([]playlistRef, 0, len(list))}
	for _, p := range list {
		answer.Playlists = append(answer.Playlists, playlistOf(p))
	}
	env := h.ok()
	env.Playlists = &answer
	h.write(w, r, env)
}

// playlist is getPlaylist.
func (h *handler) playlist(w http.ResponseWriter, r *http.Request, username string) {
	id, apiErr := requiredID(r)
	if apiErr != nil {
		h.fail(w, r, *apiErr)
		return
	}
	pid, ok := parsePlaylistID(id)
	if !ok {
		h.fail(w, r, notFound("playlist"))
		return
	}
	h.writePlaylist(w, r, username, pid)
}

// createPlaylist makes one, or with playlistId replaces what one holds -- the
// specification's two uses of the same call -- and answers the playlist.
func (h *handler) createPlaylist(w http.ResponseWriter, r *http.Request, username string) {
	q := r.URL.Query()
	fileIDs, apiErr := songIDs(q["songId"])
	if apiErr != nil {
		h.fail(w, r, *apiErr)
		return
	}

	var p db.Playlist
	var err error
	if raw := q.Get("playlistId"); raw != "" {
		pid, ok := parsePlaylistID(raw)
		if !ok {
			h.fail(w, r, notFound("playlist"))
			return
		}
		p, err = h.lists.Replace(r.Context(), username, pid, q.Get("name"), fileIDs)
	} else {
		name := q.Get("name")
		if name == "" {
			h.fail(w, r, apiError{errMissingParam, "name or playlistId is required"})
			return
		}
		p, err = h.lists.Create(r.Context(), username, name, fileIDs)
	}
	if err != nil {
		h.fail(w, r, h.playlistError(r, "write a playlist", err))
		return
	}
	h.writePlaylist(w, r, username, p.ID)
}

// updatePlaylist edits one. A field is changed only when it is sent, so an
// empty comment is how a comment is cleared and an absent one is left alone.
func (h *handler) updatePlaylist(w http.ResponseWriter, r *http.Request, username string) {
	q := r.URL.Query()
	raw := q.Get("playlistId")
	if raw == "" {
		h.fail(w, r, apiError{errMissingParam, "the playlistId parameter is required"})
		return
	}
	pid, ok := parsePlaylistID(raw)
	if !ok {
		h.fail(w, r, notFound("playlist"))
		return
	}

	var e music.Edit
	if q.Has("name") {
		name := q.Get("name")
		e.Name = &name
	}
	if q.Has("comment") {
		comment := q.Get("comment")
		e.Comment = &comment
	}
	if q.Has("public") {
		public, err := strconv.ParseBool(q.Get("public"))
		if err != nil {
			h.fail(w, r, apiError{errMissingParam, "public is true or false"})
			return
		}
		e.Public = &public
	}
	for _, s := range q["songIndexToRemove"] {
		i, err := strconv.Atoi(s)
		if err != nil {
			h.fail(w, r, apiError{errMissingParam, "songIndexToRemove is a number"})
			return
		}
		e.Remove = append(e.Remove, i)
	}
	add, apiErr := songIDs(q["songIdToAdd"])
	if apiErr != nil {
		h.fail(w, r, *apiErr)
		return
	}
	e.Add = add

	if err := h.lists.Update(r.Context(), username, pid, e); err != nil {
		h.fail(w, r, h.playlistError(r, "edit a playlist", err))
		return
	}
	h.write(w, r, h.ok())
}

// deletePlaylist removes one. The tracks in it are untouched.
func (h *handler) deletePlaylist(w http.ResponseWriter, r *http.Request, username string) {
	id, apiErr := requiredID(r)
	if apiErr != nil {
		h.fail(w, r, *apiErr)
		return
	}
	pid, ok := parsePlaylistID(id)
	if !ok {
		h.fail(w, r, notFound("playlist"))
		return
	}
	if err := h.lists.Delete(r.Context(), username, pid); err != nil {
		h.fail(w, r, h.playlistError(r, "delete a playlist", err))
		return
	}
	h.write(w, r, h.ok())
}

func (h *handler) writePlaylist(w http.ResponseWriter, r *http.Request, username string, id int64) {
	p, tracks, err := h.lists.Playlist(r.Context(), username, id)
	if err != nil {
		h.fail(w, r, h.playlistError(r, "read a playlist", err))
		return
	}

	detail := playlistDetail{playlistRef: playlistOf(p), Entries: make([]child, 0, len(tracks))}
	for _, t := range tracks {
		detail.Entries = append(detail.Entries, songOf(t))
	}
	if apiErr := h.annotate(r, username, each(detail.Entries)); apiErr != nil {
		h.fail(w, r, *apiErr)
		return
	}

	env := h.ok()
	env.Playlist = &detail
	h.write(w, r, env)
}

// playlistError is what internal/music's refusals become. Not found covers a
// playlist and a song alike, because either is an id the client should drop.
func (h *handler) playlistError(r *http.Request, what string, err error) apiError {
	switch {
	case errors.Is(err, db.ErrNotFound):
		return notFound("playlist or song")
	case errors.Is(err, music.ErrIndex):
		return apiError{errMissingParam, "no such position in the playlist"}
	default:
		return h.internal(r, what, err)
	}
}

// songIDs reads a list of song ids, refusing the whole list for one that is not
// an id: a client told "ok" believes every one of them went in.
func songIDs(raw []string) ([]int64, *apiError) {
	out := make([]int64, 0, len(raw))
	for _, id := range raw {
		fileID, ok := parseSongID(id)
		if !ok {
			return nil, ptr(notFound("song"))
		}
		out = append(out, fileID)
	}
	return out, nil
}
