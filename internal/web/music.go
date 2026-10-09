package web

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/media"
	"github.com/C0piIot/stratus-backend/internal/music"
	"github.com/C0piIot/stratus-backend/internal/storage"
)

// The music library (#212, #279): artists, their albums, and an album with a
// player per track. It reads the index by tag, like the gallery reads it by
// date, so how the files are filed does not matter.
//
// **One address, both protocols**, like the photographs: these are the same
// URLs the WebDAV mount answers for, and the composition root sends a
// browser's methods here. A track's own address is the track, downloaded; an
// album's is the page, and `?cover=<px>` on it is the picture -- a query
// because the cover is derived and not one of the album's files, which keeps
// the collection's names exactly the tracks.
//
// The names in those addresses are internal/music's, shared with the mount: an
// artist called AC/DC is a tag, a collection cannot have a slash in its name,
// and a link that spelled it differently would 404 against the collection
// behind it.
//
// A play is the one thing it does not count itself: the page asks OpenSubsonic,
// with scrobble and the session as its credential (#234), rather than grow an
// endpoint the protocol already has.
const (
	musicPrefix = "/music/"
	// coverParam is how a page asks for an album's picture.
	coverParam = "cover"
	// artistParam and albumParam are how a caller holding tags reaches the
	// address they are served at: see musicByTag.
	artistParam = "artist"
	albumParam  = "album"
	// subsonicPrefix is where app mounts OpenSubsonic, under the same condition
	// as this surface: configured credentials.
	subsonicPrefix = "/rest/"
	// subsonicClient is the c every call from a page carries.
	subsonicClient = "stratus-web"
	// subsonicSong is how OpenSubsonic spells a track's id: the prefix and the
	// file id. Written out here because inbound adapters do not import each
	// other; TestAPlayFromTheWebUICounts in internal/app is what holds the two
	// together.
	subsonicSong = "tr-"
)

// The cover's sizes on the thumbnail ladder: a grid cell and the album page.
const (
	coverTile  = 300
	coverAlbum = 1200
)

// albumPolicy is the pages' policy plus the audio elements' media.
const albumPolicy = contentSecurityPolicy + "; media-src 'self'"

type artistRow struct {
	Name    string
	Href    string
	Albums  int
	Starred bool
}

type albumRow struct {
	Name  string
	Href  string
	Cover string
	Year  int
}

type albumView struct {
	Artist, ArtistHref string
	Cover              string
	Year               int
	Genre              string
	Duration           string
	Starred            bool
	Rating             string
	Tracks             []trackRow
}

type trackRow struct {
	Number   int
	Title    string
	Duration string
	Src      string
	// Scrobble is the call htmx makes when the track has played to the end;
	// with no JavaScript nothing makes it, and the track plays uncounted.
	Scrobble string
	Starred  bool
	Rating   string
	Plays    int64
}

// music is every address under /music/, split by what is at it. Resolution is
// internal/music's, which is what makes a 404 here and a 404 from the mount
// the same 404.
func (h *handler) music(w http.ResponseWriter, r *http.Request, user string) {
	tree := music.NewTree(h.library, user)
	raw := strings.Trim(r.PathValue("path"), "/")

	if raw == "" {
		if tag := r.URL.Query().Get(artistParam); tag != "" {
			h.musicByTag(w, r, user, tree, tag, r.URL.Query().Get(albumParam))
			return
		}
	}

	n, err := tree.Resolve(r.Context(), raw)
	switch {
	case errors.Is(err, os.ErrNotExist):
		h.fail(w, r, user, db.ErrNotFound)
		return
	case err != nil:
		h.fail(w, r, user, err)
		return
	}

	switch {
	case n.ArtistName == "":
		h.artists(w, r, user, tree)
	case n.AlbumName == "":
		h.artist(w, r, user, tree, n)
	case n.TrackName == "" && r.URL.Query().Has(coverParam):
		h.cover(w, r, user, n)
	case n.TrackName == "":
		h.album(w, r, user, tree, n)
	default:
		// A track's own address is the track, like a file's is under /files/.
		// This is also the GET a WebDAV client makes.
		h.download(w, r, user, n.Track.File)
	}
}

func (h *handler) artists(w http.ResponseWriter, r *http.Request, user string, tree *music.Tree) {
	children, err := tree.Children(r.Context(), music.Node{Dir: true})
	if err != nil {
		h.fail(w, r, user, err)
		return
	}
	list := make([]db.Artist, 0, len(children))
	for _, c := range children {
		list = append(list, c.Artist)
	}
	subjects := make([]db.Subject, len(list))
	for i, a := range list {
		subjects[i] = db.ArtistSubject(a.Name)
	}
	notes, err := h.library.AnnotationsOf(r.Context(), user, subjects)
	if err != nil {
		h.fail(w, r, user, err)
		return
	}
	rows := make([]artistRow, len(list))
	for i, a := range list {
		rows[i] = artistRow{
			Name: a.Name, Href: musicHref(children[i].Path(), true), Albums: a.AlbumCount,
			Starred: !notes[subjects[i]].Starred.IsZero(),
		}
	}
	h.render(w, http.StatusOK, pageArtists, view{Title: "Music", User: user, Gallery: "Music", Artists: rows})
}

func (h *handler) artist(w http.ResponseWriter, r *http.Request, user string, tree *music.Tree, n music.Node) {
	children, err := tree.Children(r.Context(), n)
	if err != nil {
		h.fail(w, r, user, err)
		return
	}
	rows := make([]albumRow, len(children))
	for i, c := range children {
		rows[i] = albumRow{
			Name: c.Album.Name, Href: musicHref(c.Path(), true), Year: c.Album.Year,
			Cover: coverHref(c.Path(), coverTile),
		}
	}
	h.render(w, http.StatusOK, pageArtist, view{
		Title: n.Artist.Name, User: user, Gallery: "Music", Name: n.Artist.Name, Albums: rows, Back: musicPrefix,
	})
}

func (h *handler) album(w http.ResponseWriter, r *http.Request, user string, tree *music.Tree, n music.Node) {
	artist, name := n.Artist.Name, n.Album.Name
	children, err := tree.Children(r.Context(), n)
	if err != nil {
		h.fail(w, r, user, err)
		return
	}
	tracks := make([]db.Track, 0, len(children))
	for _, c := range children {
		tracks = append(tracks, c.Track)
	}
	if len(tracks) == 0 {
		h.fail(w, r, user, db.ErrNotFound)
		return
	}

	album := db.AlbumSubject(artist, name)
	subjects := []db.Subject{album}
	for _, t := range tracks {
		subjects = append(subjects, db.TrackSubject(t.File.ID))
	}
	notes, err := h.library.AnnotationsOf(r.Context(), user, subjects)
	if err != nil {
		h.fail(w, r, user, err)
		return
	}

	av := &albumView{
		Artist: artist, ArtistHref: musicHref(n.ArtistName, true),
		Cover:   coverHref(n.Path(), coverAlbum),
		Starred: !notes[album].Starred.IsZero(),
		Rating:  stars(notes[album].Rating),
	}
	var total int64
	for i, t := range tracks {
		m, note := t.Media, notes[db.TrackSubject(t.File.ID)]
		title := m.Title
		if title == "" {
			title = path.Base(t.File.Path)
		}
		av.Tracks = append(av.Tracks, trackRow{
			Number: m.TrackNo, Title: title, Duration: clock(m.DurationMS),
			// The track's own address here, not the file's under /files/:
			// this page is the collection's other half, and one URL per thing
			// is what the rest of this adapter promises.
			Src:      musicHref(children[i].Path(), false),
			Scrobble: scrobbleURL(t.File.ID),
			Starred:  !note.Starred.IsZero(), Rating: stars(note.Rating), Plays: note.PlayCount,
		})
		total += m.DurationMS
		if av.Year == 0 {
			av.Year = m.Year
		}
		if av.Genre == "" {
			av.Genre = m.Genre
		}
	}
	av.Duration = clock(total)

	w.Header().Set("Content-Security-Policy", albumPolicy)
	h.render(w, http.StatusOK, pageAlbum, view{
		Title: name, User: user, Gallery: "Music", Name: name, Album: av, Back: av.ArtistHref,
	})
}

// cover is an album's picture: from the folder its first track is in, beside
// the tracks or inside one, which is where getCoverArt finds it too.
//
// A query on the album's own address rather than a name inside it, because it
// is derived: a file called "cover" in the collection would be a name no track
// could have, and a listing would have to ask the thumbnailer about every
// album to know whether to show it.
func (h *handler) cover(w http.ResponseWriter, r *http.Request, user string, n music.Node) {
	tracks, err := h.library.Tracks(r.Context(), user, n.Artist.Name, n.Album.Name)
	if err != nil {
		h.fail(w, r, user, err)
		return
	}
	if len(tracks) == 0 {
		http.NotFound(w, r)
		return
	}
	size := coverTile
	if px, perr := strconv.Atoi(r.URL.Query().Get(coverParam)); perr == nil && px > 0 {
		size = px
	}

	body, length, err := h.thumbs.Cover(r.Context(), user, db.ParentOf(tracks[0].File.Path), size)
	switch {
	case errors.Is(err, media.ErrBusy):
		w.Header().Set("Retry-After", "5")
		http.Error(w, "busy", http.StatusServiceUnavailable)
		return
	case errors.Is(err, storage.ErrNotFound), errors.Is(err, media.ErrNoEmbeddedCover), errors.Is(err, media.ErrNoThumbnail):
		// Half a library has no picture. The page draws the empty square it
		// was going to fill.
		http.NotFound(w, r)
		return
	case err != nil:
		h.fail(w, r, user, err)
		return
	}
	defer func() { _ = body.Close() }()

	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	_, _ = io.Copy(w, body)
}

// musicByTag sends a caller holding tags to the address they are served at.
//
// A redirect rather than a second page, for the reason the photographs have
// one: a search result is a row of the index, and working out the generated
// name of every row on a page would be a query per row. One resolves when one
// is clicked.
func (h *handler) musicByTag(w http.ResponseWriter, r *http.Request, user string, tree *music.Tree, artist, album string) {
	var (
		at  string
		err error
	)
	if album == "" {
		at, err = tree.PathOfArtist(r.Context(), artist)
	} else {
		at, err = tree.PathOfAlbum(r.Context(), artist, album)
	}
	switch {
	case errors.Is(err, os.ErrNotExist):
		h.fail(w, r, user, db.ErrNotFound)
		return
	case err != nil:
		h.fail(w, r, user, err)
		return
	}
	redirectLocal(w, r, musicHref(at, true))
}

// musicByTagHref is that redirect, for the pages that hold tags.
func musicByTagHref(artist, album string) string {
	q := url.Values{artistParam: {artist}}
	if album != "" {
		q.Set(albumParam, album)
	}
	return musicPrefix + "?" + q.Encode()
}

// musicHref is the URL of a place in the library tree. The path is already one
// segment per name, because internal/music made it so; a collection ends in a
// slash, as a collection's URL does, and a track does not.
func musicHref(p string, dir bool) string {
	if p == "" {
		return musicPrefix
	}
	at := link(musicPrefix, p)
	if dir {
		at += "/"
	}
	return at
}

// coverHref is an album's picture, at the album's own address.
func coverHref(album string, px int) string {
	return musicHref(album, true) + "?" + coverParam + "=" + strconv.Itoa(px)
}

func scrobbleURL(fileID int64) string {
	return subsonicPrefix + "scrobble?" + url.Values{
		"id": {subsonicSong + strconv.FormatInt(fileID, 10)}, "submission": {"true"}, "c": {subsonicClient},
	}.Encode()
}

// stars draws a rating out of five, and nothing for no rating.
func stars(rating int) string {
	if rating <= 0 {
		return ""
	}
	return strings.Repeat("★", rating) + strings.Repeat("☆", 5-rating)
}

func clock(ms int64) string {
	if ms <= 0 {
		return ""
	}
	d := time.Duration(ms) * time.Millisecond
	if d >= time.Hour {
		return fmt.Sprintf("%d:%02d:%02d", int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60)
	}
	return fmt.Sprintf("%d:%02d", int(d.Minutes()), int(d.Seconds())%60)
}
