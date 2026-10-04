package web

import (
	"fmt"
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

// The buckets a search answers, by the name the URL uses. A page with none of
// them shows them all; one with a name shows that bucket alone and pages it.
const (
	inFiles   = "files"
	inTracks  = "tracks"
	inPhotos  = "photos"
	inArtists = "artists"
	inAlbums  = "albums"
)

// The fragments htmx asks for when it extends one of the buckets, as
// listFragment is for the files one.
const (
	searchTracks  = "tracks"
	searchPhotos  = "photos"
	searchArtists = "artists"
	searchAlbums  = "albums"
)

// search answers the box.
func (h *handler) search(w http.ResponseWriter, r *http.Request, user string) {
	q := r.URL.Query()
	term := strings.TrimSpace(q.Get("q"))

	only := q.Get("in")
	switch only {
	case "", inFiles, inTracks, inPhotos, inArtists, inAlbums:
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

	// A cursor is read in the shape of the bucket it belongs to -- a path for
	// the three made of files, a name for the two made of tags -- which is
	// why it can be one parameter: a page that carries one is a page narrowed
	// to one bucket.
	var after db.Cursor
	var afterTag db.TagCursor
	var err error
	switch only {
	case inArtists, inAlbums:
		afterTag, err = parseTagCursor(q.Get("after"))
	default:
		after, err = parseCursor(db.SortName, q.Get("after"))
	}
	if err != nil {
		h.badRequest(w, user, "That is not a place in these results to carry on from.")
		return
	}

	// One more than a page of whichever buckets were asked for, so the page
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
	if only == "" || only == inArtists {
		filter.Artists = db.TagWindow{After: afterTag, Limit: searchPageSize + 1}
	}
	if only == "" || only == inAlbums {
		filter.Albums = db.TagWindow{After: afterTag, Limit: searchPageSize + 1}
	}
	found, findErr := h.finder.Find(r.Context(), user, filter)
	if findErr != nil {
		h.fail(w, r, user, findErr)
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

	artists, moreArtists := trim(found.Artists)
	v.FoundArtists = foundArtists(artists)
	if moreArtists {
		v.MoreArtists = searchTagLink(term, inArtists,
			db.TagCursor{Artist: artists[len(artists)-1].Name})
	}

	albums, moreAlbums := trim(found.Albums)
	v.FoundAlbums = foundAlbums(albums)
	if moreAlbums {
		last := albums[len(albums)-1]
		v.MoreAlbums = searchTagLink(term, inAlbums,
			db.TagCursor{Artist: last.Artist, Album: last.Name})
	}

	// htmx is extending one bucket, which is the only time a fragment is asked
	// for -- and it asks for the one it is in, since a narrowed page shows one.
	if r.Header.Get("HX-Request") == "true" {
		fragment := listFragment
		switch only {
		case inTracks:
			fragment = searchTracks
		case inPhotos:
			fragment = searchPhotos
		case inArtists:
			fragment = searchArtists
		case inAlbums:
			fragment = searchAlbums
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

// searchLink is the rest of one bucket: the same search, narrowed to it and
// resumed. Narrowed because two cursors in one URL is a URL nobody can read,
// and because the bucket somebody is reading is the one they want more of.
func searchLink(term, only string, after db.Cursor) string {
	return searchResumed(term, only, encodeCursor(db.SortName, after))
}

// searchTagLink is searchLink for the two buckets a name resumes.
func searchTagLink(term, only string, after db.TagCursor) string {
	return searchResumed(term, only, encodeTagCursor(after))
}

func searchResumed(term, only, after string) string {
	q := url.Values{}
	q.Set("q", term)
	q.Set("in", only)
	q.Set("after", after)
	return searchPrefix + "?" + q.Encode()
}

// encodeTagCursor writes a name, or a name under a name, into one parameter.
// Each half is escaped, so the slash between them is the only one there is --
// an album called AC/DC Live cannot be read as two.
func encodeTagCursor(c db.TagCursor) string {
	if c.Album == "" {
		return url.QueryEscape(c.Artist)
	}
	return url.QueryEscape(c.Artist) + "/" + url.QueryEscape(c.Album)
}

func parseTagCursor(v string) (db.TagCursor, error) {
	if v == "" {
		return db.TagCursor{}, nil
	}
	artist, album, _ := strings.Cut(v, "/")
	name, err := url.QueryUnescape(artist)
	if err != nil {
		return db.TagCursor{}, fmt.Errorf("cursor %q: %w", v, err)
	}
	under, err := url.QueryUnescape(album)
	if err != nil {
		return db.TagCursor{}, fmt.Errorf("cursor %q: %w", v, err)
	}
	return db.TagCursor{Artist: name, Album: under}, nil
}

// foundArtist and foundAlbum are a result's two summary rows: the answer to a
// word that is somebody's name is the name, not the two hundred tracks under
// it (#262). Each is a line and not a cover, because fifty covers on a page
// somebody reads is fifty thumbnails to make.
type foundArtist struct {
	Name   string
	Href   string
	Albums int
}

type foundAlbum struct {
	Name       string
	Href       string
	Artist     string
	ArtistHref string
	Songs      int
	Year       int
}

func foundArtists(artists []db.Artist) []foundArtist {
	out := make([]foundArtist, 0, len(artists))
	for _, a := range artists {
		out = append(out, foundArtist{Name: a.Name, Href: musicByTagHref(a.Name, ""), Albums: a.AlbumCount})
	}
	return out
}

func foundAlbums(albums []db.Album) []foundAlbum {
	out := make([]foundAlbum, 0, len(albums))
	for _, a := range albums {
		out = append(out, foundAlbum{
			Name: a.Name, Href: musicByTagHref(a.Artist, a.Name),
			Artist: a.Artist, ArtistHref: musicByTagHref(a.Artist, ""),
			Songs: a.SongCount, Year: a.Year,
		})
	}
	return out
}

// shots is the photographs of a result, as the cells the gallery's grid is made
// of. The viewer is the gallery's too: a photograph found here opens where it
// would have opened there, with the ones either side of it a click away.
func shots(photos []db.File) []tile {
	out := make([]tile, 0, len(photos))
	for _, p := range photos {
		cell := tile{Href: photoByFileHref(p.Path), Name: path.Base(p.Path)}
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
		// No fallback to the file's name, and that is not an omission: this
		// bucket matches the title, so a track whose tags nothing has read has
		// no title to match and is found by its name in the files bucket --
		// which is where a row with nothing read about it belongs.
		row := foundTrack{
			Title:    t.Media.Title,
			By:       by(t.Media),
			Href:     href(t.File.Path),
			Duration: duration(t.Media.DurationMS),
		}
		if t.Media.AlbumArtist != "" && t.Media.Album != "" {
			row.Album = musicByTagHref(t.Media.AlbumArtist, t.Media.Album)
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
