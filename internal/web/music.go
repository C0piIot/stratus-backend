package web

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/media"
	"github.com/C0piIot/stratus-backend/internal/storage"
)

// The music library (#212): artists, their albums, and an album with a player
// per track. It reads the index by tag, like the gallery reads it by date, so
// how the files are filed does not matter.
//
// A play is the one thing it does not count itself: the page asks OpenSubsonic,
// with scrobble and the session as its credential (#234), rather than grow an
// endpoint the protocol already has.
const (
	musicPrefix = "/music"
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

func (h *handler) artists(w http.ResponseWriter, r *http.Request, user string) {
	list, err := h.library.Artists(r.Context(), user)
	if err != nil {
		h.fail(w, r, user, err)
		return
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
			Name: a.Name, Href: musicLink(a.Name), Albums: a.AlbumCount,
			Starred: !notes[subjects[i]].Starred.IsZero(),
		}
	}
	h.render(w, http.StatusOK, pageArtists, view{Title: "Music", User: user, Artists: rows})
}

func (h *handler) artist(w http.ResponseWriter, r *http.Request, user string) {
	name := r.PathValue("artist")
	list, err := h.library.Albums(r.Context(), user, name)
	if err != nil {
		h.fail(w, r, user, err)
		return
	}
	if len(list) == 0 {
		h.fail(w, r, user, db.ErrNotFound)
		return
	}
	rows := make([]albumRow, len(list))
	for i, a := range list {
		rows[i] = albumRow{
			Name: a.Name, Href: musicLink(a.Artist, a.Name), Year: a.Year,
			Cover: coverURL(a.Artist, a.Name, coverTile),
		}
	}
	h.render(w, http.StatusOK, pageArtist, view{
		Title: name, User: user, Name: name, Albums: rows, Back: musicPrefix,
	})
}

func (h *handler) album(w http.ResponseWriter, r *http.Request, user string) {
	artist, name := r.PathValue("artist"), r.PathValue("album")
	tracks, err := h.library.Tracks(r.Context(), user, artist, name)
	if err != nil {
		h.fail(w, r, user, err)
		return
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
		Artist: artist, ArtistHref: musicLink(artist),
		Cover:   coverURL(artist, name, coverAlbum),
		Starred: !notes[album].Starred.IsZero(),
		Rating:  stars(notes[album].Rating),
	}
	var total int64
	for _, t := range tracks {
		m, note := t.Media, notes[db.TrackSubject(t.File.ID)]
		title := m.Title
		if title == "" {
			title = path.Base(t.File.Path)
		}
		av.Tracks = append(av.Tracks, trackRow{
			Number: m.TrackNo, Title: title, Duration: clock(m.DurationMS),
			Src:      href(t.File.Path),
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
		Title: name, User: user, Name: name, Album: av, Back: av.ArtistHref,
	})
}

// cover is an album's picture: from the folder its first track is in, beside
// the tracks or inside one, which is where getCoverArt finds it too.
func (h *handler) cover(w http.ResponseWriter, r *http.Request, user string) {
	tracks, err := h.library.Tracks(r.Context(), user, r.PathValue("artist"), r.PathValue("album"))
	if err != nil {
		h.fail(w, r, user, err)
		return
	}
	if len(tracks) == 0 {
		http.NotFound(w, r)
		return
	}
	size := coverTile
	if px, perr := strconv.Atoi(r.URL.Query().Get("size")); perr == nil && px > 0 {
		size = px
	}

	body, length, err := h.thumbs.Cover(r.Context(), user, db.ParentOf(tracks[0].File.Path), size)
	switch {
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

// musicLink escapes each tag as one segment, so an artist called AC/DC is one
// step down and not two.
func musicLink(tags ...string) string {
	var b strings.Builder
	b.WriteString(musicPrefix)
	for _, t := range tags {
		b.WriteString("/" + url.PathEscape(t))
	}
	return b.String()
}

func coverURL(artist, album string, px int) string {
	return musicLink(artist, album) + "/cover?size=" + strconv.Itoa(px)
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
