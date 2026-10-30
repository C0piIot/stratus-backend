package web

import (
	"net/http"
	"strings"

	"github.com/stratus-private-cloud/stratus-backend/internal/db"
	"github.com/stratus-private-cloud/stratus-backend/internal/music"
)

// The playlists (#203, #279): the same addresses the WebDAV mount serves as
// .m3u8 files, answering a browser as well.
//
// `/playlists/` is the list, `/playlists/Mix.m3u8` is the file a player wants,
// and the same URL with ?view is the page about it -- the shape a photograph's
// address has, for the same reason: one URL per thing, and the derived view
// behind a query.
//
// **Read-only, and that is a decision rather than an omission.** The database
// is what a playlist is and these files are a view of it; creating and editing
// go through internal/music in one transaction each, from a Subsonic client.
// A page with forms here would be a second way to write what nothing else in
// this adapter writes.
//
// The names and the file are internal/music's, shared with the mount, so the
// two halves cannot generate different bytes for one address.
const playlistsPrefix = "/playlists/"

// playlistRow is one playlist in the list.
type playlistRow struct {
	Name   string
	Href   string
	File   string
	Tracks int
}

// playlistView is one playlist and what is in it.
type playlistView struct {
	Name   string
	File   string
	Tracks []trackRow
}

// playlistPages is every address under /playlists/.
func (h *handler) playlistPages(w http.ResponseWriter, r *http.Request, user string) {
	all, err := h.playlists.Playlists(r.Context(), user)
	if err != nil {
		h.fail(w, r, user, err)
		return
	}
	named, order := music.PlaylistNames(all)

	raw := strings.Trim(r.PathValue("path"), "/")
	if raw == "" {
		h.playlistIndex(w, r, user, named, order)
		return
	}
	pl, ok := named[raw]
	if !ok {
		h.fail(w, r, user, db.ErrNotFound)
		return
	}

	pl, tracks, err := h.playlists.Playlist(r.Context(), user, pl.ID)
	if err != nil {
		h.fail(w, r, user, err)
		return
	}
	if !r.URL.Query().Has(viewParam) {
		// The file itself, which is what a player asked for. Written here
		// rather than served through ServeContent: it is generated, so there
		// is no modification time to condition on and nothing to seek in.
		w.Header().Set("Content-Type", music.M3U8MIME)
		_, _ = w.Write(music.M3U8(pl, tracks, filesPrefix))
		return
	}

	queue := link(playlistsPrefix, raw) + "?" + viewParam
	if at, ok := playAt(r); ok {
		srcs := make([]string, len(tracks))
		for i, t := range tracks {
			srcs[i] = href(t.File.Path)
		}
		h.bar(w, r, user, music.NewTree(h.library, user), queue, tracks, srcs, at)
		return
	}

	pv := &playlistView{Name: pl.Name, File: link(playlistsPrefix, raw)}
	for i, t := range tracks {
		title := music.Title(t)
		pv.Tracks = append(pv.Tracks, trackRow{
			Number: i + 1, Title: title, Duration: duration(t.Media.DurationMS),
			// The file's own address, which is where the .m3u8 points too: a
			// playlist is an order over the tree, not a place in it.
			Src:  href(t.File.Path),
			Play: playHref(queue, i+1),
		})
	}

	w.Header().Set("Content-Security-Policy", albumPolicy)
	h.render(w, http.StatusOK, pagePlaylist, view{
		Title: pl.Name, User: user, Gallery: "Playlists", Player: true, Playlist: pv, Back: playlistsPrefix,
	})
}

func (h *handler) playlistIndex(w http.ResponseWriter, r *http.Request, user string,
	named map[string]db.Playlist, order []string,
) {
	rows := make([]playlistRow, 0, len(order))
	for _, name := range order {
		pl := named[name]
		rows = append(rows, playlistRow{
			Name: pl.Name,
			Href: link(playlistsPrefix, name) + "?" + viewParam,
			File: link(playlistsPrefix, name),
		})
	}
	w.Header().Set("Content-Security-Policy", albumPolicy)
	h.render(w, http.StatusOK, pagePlaylists, view{Title: "Playlists", User: user, Gallery: "Playlists", Player: true, Playlists: rows})
}
