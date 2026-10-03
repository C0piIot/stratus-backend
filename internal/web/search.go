package web

import (
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/media"
)

// searchPrefix is where the box in the navbar submits. A GET form, so a search
// is a URL somebody can send, bookmark or come back to -- which is also why
// there is no live dropdown: that would be a request per keystroke and nothing
// to link to at the end of it.
const searchPrefix = "/search"

// searchPageSize is how many of each kind one page holds. Smaller than a
// folder's hundred on purpose: a search is read, not scrolled, and the answer
// to fifty of the wrong thing is a better term rather than more rows.
const searchPageSize = 50

// The two halves a search answers, by the name the URL uses. A page with
// neither shows both; one with a name shows that half alone and pages it.
const (
	inFiles  = "files"
	inTracks = "tracks"
	inPhotos = "photos"
)

// The fragments htmx asks for when it extends one of the halves, as
// listFragment is for the files one.
const (
	searchTracks = "tracks"
	searchPhotos = "photos"
)

// search answers the box.
func (h *handler) search(w http.ResponseWriter, r *http.Request, user string) {
	q := r.URL.Query()
	term := strings.TrimSpace(q.Get("q"))

	only := q.Get("in")
	switch only {
	case "", inFiles, inTracks, inPhotos:
	default:
		h.badRequest(w, user, "There is nothing here to search called that.")
		return
	}

	v := view{Title: "Search", User: user, Query: term, Here: searchPrefix}
	// An empty box is not an error and not a search: it is the page it was
	// typed into.
	if term == "" {
		h.render(w, http.StatusOK, pageSearch, v)
		return
	}

	after, err := parseCursor(db.SortName, q.Get("after"))
	if err != nil {
		h.badRequest(w, user, "That is not a place in these results to carry on from.")
		return
	}

	// One more than a page of whichever halves were asked for, so the page
	// knows whether to offer more without a second query.
	filter := db.FindFilter{Text: term}
	if only == "" || only == inFiles {
		filter.Files = db.Window{After: after, Limit: searchPageSize + 1}
	}
	if only == "" || only == inTracks {
		filter.Tracks = db.Window{After: after, Limit: searchPageSize + 1}
	}
	if only == "" || only == inPhotos {
		filter.Photos = db.Window{After: after, Limit: searchPageSize + 1}
	}
	found, err := h.finder.Find(r.Context(), user, filter)
	if err != nil {
		h.fail(w, r, user, err)
		return
	}

	files, moreFiles := trim(found.Files)
	v.Entries = entries(files, h.indexingOf(r, files), "")
	v.Only = only
	if moreFiles {
		v.NextPage = searchLink(term, inFiles, db.After(files[len(files)-1]))
	}

	tracks, moreTracks := trim(found.Tracks)
	v.Found = foundTracks(tracks)
	if moreTracks {
		v.MoreTracks = searchLink(term, inTracks, db.After(tracks[len(tracks)-1].File))
	}

	photos, morePhotos := trim(found.Photos)
	v.Tiles = shots(photos)
	if morePhotos {
		v.MorePhotos = searchLink(term, inPhotos, db.After(photos[len(photos)-1]))
	}

	// htmx is extending one half, which is the only time a fragment is asked
	// for -- and it asks for the half it is in, since a bucket page shows one.
	if r.Header.Get("HX-Request") == "true" {
		fragment := listFragment
		switch only {
		case inTracks:
			fragment = searchTracks
		case inPhotos:
			fragment = searchPhotos
		}
		h.renderTemplate(w, http.StatusOK, pageSearch, fragment, v)
		return
	}
	h.render(w, http.StatusOK, pageSearch, v)
}

// trim takes the extra row a page asked for and reports that it was there.
func trim[T any](rows []T) ([]T, bool) {
	if len(rows) > searchPageSize {
		return rows[:searchPageSize], true
	}
	return rows, false
}

// searchLink is the rest of one half: the same search, narrowed to that half
// and resumed. Narrowed because two cursors in one URL is a URL nobody can
// read, and because the half somebody is reading is the half they want more of.
func searchLink(term, only string, after db.Cursor) string {
	q := url.Values{}
	q.Set("q", term)
	q.Set("in", only)
	q.Set("after", encodeCursor(db.SortName, after))
	return searchPrefix + "?" + q.Encode()
}

// shots is the photographs of a result, as the cells the gallery's grid is made
// of. The viewer is the gallery's too: a photograph found here opens where it
// would have opened there, with the ones either side of it a click away.
func shots(photos []db.File) []tile {
	out := make([]tile, 0, len(photos))
	for _, p := range photos {
		cell := tile{Href: link(photoPrefix, p.Path), Name: path.Base(p.Path)}
		if media.CanThumbnail(p.Path, p.Size) {
			cell.Thumb = thumbURL(p, tileThumb)
		}
		out = append(out, cell)
	}
	return out
}

// foundTrack is one track in a result: what it is, where to hear it and where
// it sits in the library.
type foundTrack struct {
	Title    string
	By       string
	Href     string
	Album    string
	Duration string
}

func foundTracks(tracks []db.Track) []foundTrack {
	out := make([]foundTrack, 0, len(tracks))
	for _, t := range tracks {
		row := foundTrack{
			Title:    t.Media.Title,
			By:       by(t.Media),
			Href:     href(t.File.Path),
			Duration: duration(t.Media.DurationMS),
		}
		// A track with no title is one nothing has read the tags of, or one
		// that has none: its name is all there is to call it.
		if row.Title == "" {
			row.Title = path.Base(t.File.Path)
		}
		if t.Media.AlbumArtist != "" && t.Media.Album != "" {
			row.Album = musicLink(t.Media.AlbumArtist, t.Media.Album)
		}
		out = append(out, row)
	}
	return out
}

// by is the line under a track's title: who it is by and what it is on, with
// whichever of the two there is.
func by(m db.Media) string {
	parts := make([]string, 0, 3)
	for _, p := range []string{m.Artist, m.Album} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if m.Year > 0 {
		parts = append(parts, strconv.Itoa(m.Year))
	}
	return strings.Join(parts, " · ")
}
