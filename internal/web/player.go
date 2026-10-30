package web

import (
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/stratus-private-cloud/stratus-backend/internal/db"
	"github.com/stratus-private-cloud/stratus-backend/internal/music"
)

// The player bar: one track at a time at the foot of the music pages, and the
// next one when it ends. htmx and nothing else -- the audio element's own ended
// event asks for the bar that follows and swaps it in, and links inside the
// library swap the page's main and leave the bar alone, so it keeps playing
// while somebody browses. A link out of the library is an ordinary navigation
// and stops it, which is the bargain: no script of ours keeping a page alive.
//
// The queue is the album or the playlist the track was started from, and the
// position in it travels in the URL, `?track=<n>` on the queue's own address:
// nothing about it is kept on the server, and the bar is that address's other
// view the way the cover is.
//
// Every new track is a new audio element, which is what an element that is
// swapped is. Desktop browsers and Android let one start itself once the page
// has been touched; iOS does not, and a phone with the screen off is no place
// for a chain of requests either. That is what a Subsonic client is for.
const trackParam = "track"

// barView is the track the bar is playing, and where it goes next.
type barView struct {
	Title, Artist string
	// Album is the album's page, and Cover its picture.
	Album, Cover string
	Src          string
	Scrobble     string
	// Next is asked when the track ends, and answers an empty bar past the
	// end of the queue; Prev and Skip are the buttons, empty where there is
	// nothing to go to.
	Next, Prev, Skip string
}

// playHref is the bar for the nth track, counted from one, of the queue at base.
func playHref(base string, n int) string {
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	return base + sep + trackParam + "=" + strconv.Itoa(n)
}

// playAt reads the position a bar was asked for, and false when it is not one.
func playAt(r *http.Request) (int, bool) {
	n, err := strconv.Atoi(r.URL.Query().Get(trackParam))
	return n, err == nil && n > 0
}

// bar answers the bar for the nth track of a queue: the tracks, their
// addresses in the library tree, and the queue's own address.
func (h *handler) bar(w http.ResponseWriter, r *http.Request, user string, tree *music.Tree,
	queue string, tracks []db.Track, srcs []string, n int,
) {
	v := view{User: user}
	if n <= len(tracks) {
		t := tracks[n-1]
		b := &barView{
			Title: music.Title(t), Artist: t.Media.Artist,
			Src: srcs[n-1], Scrobble: scrobbleURL(t.File.ID),
			Next: playHref(queue, n+1),
		}
		if b.Artist == "" {
			b.Artist = t.Media.AlbumArtist
		}
		// The album the track is on, which for a playlist is not the queue. A
		// track the tree cannot place has no album to show, not an error.
		if at, err := tree.PathOf(r.Context(), t); err == nil {
			b.Album, b.Cover = musicHref(path.Dir(at), true), coverHref(path.Dir(at), coverTile)
		}
		if n > 1 {
			b.Prev = playHref(queue, n-1)
		}
		if n < len(tracks) {
			b.Skip = b.Next
		}
		v.Bar = b
	}
	h.renderTemplate(w, http.StatusOK, pageAlbum, "player", v)
}
