package subsonic

import (
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/stratus-private-cloud/stratus-backend/internal/db"
)

// What outside sources said about the library (#217), answered the way the
// protocol asks for it: the words of a song through the songLyrics extension,
// and an album's MusicBrainz id through getAlbumInfo2. Read here and never
// written -- the pass that fills the rows is internal/lookup's, and this
// adapter answers from whatever it has, which with the setting off is
// nothing and is still an answer.

// lyricsList is getLyricsBySongId's payload: one entry per form the words
// come in. The array has to be present even when empty, so it is never nil.
type lyricsList struct {
	Lyrics []structuredLyrics `xml:"structuredLyrics" json:"structuredLyrics"`
}

// structuredLyrics is one form of the words, synced or not, as the extension
// defines it.
type structuredLyrics struct {
	DisplayArtist string `xml:"displayArtist,attr,omitempty" json:"displayArtist,omitempty"`
	DisplayTitle  string `xml:"displayTitle,attr,omitempty" json:"displayTitle,omitempty"`
	// Lang is "und", undetermined: LRCLIB does not say which language a
	// song is in, and guessing one is worse than saying so.
	Lang   string      `xml:"lang,attr" json:"lang"`
	Offset int         `xml:"offset,attr" json:"offset"`
	Synced bool        `xml:"synced,attr" json:"synced"`
	Lines  []lyricLine `xml:"line" json:"line"`
}

// lyricLine is one line, with when it is sung in milliseconds for the synced
// form and no start at all otherwise -- which is what the pointer is for.
type lyricLine struct {
	Start *int64 `xml:"start,attr,omitempty" json:"start,omitempty"`
	Value string `xml:",chardata" json:"value"`
}

// albumInfo is getAlbumInfo2's payload. Only what is known: the id, with the
// notes and the images the schema allows left out rather than invented.
type albumInfo struct {
	Notes         string `xml:"notes,omitempty" json:"notes,omitempty"`
	MusicBrainzID string `xml:"musicBrainzId,omitempty" json:"musicBrainzId,omitempty"`
}

// undetermined is ISO 639-2 for a language nobody has said.
const undetermined = "und"

// lyricsBySong is getLyricsBySongId.
func (h *handler) lyricsBySong(w http.ResponseWriter, r *http.Request, username string) {
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

	rows, err := h.lib.LyricsOf(r.Context(), []int64{t.File.ID})
	if err != nil {
		h.fail(w, r, h.internal(r, "read lyrics", err))
		return
	}

	list := lyricsList{Lyrics: []structuredLyrics{}}
	if l, ok := rows[t.File.ID]; ok {
		if l.Synced != "" {
			list.Lyrics = append(list.Lyrics, lyricsOf(t, parseLRC(l.Synced), true))
		}
		if l.Plain != "" {
			list.Lyrics = append(list.Lyrics, lyricsOf(t, plainLines(l.Plain), false))
		}
	}
	env := h.ok()
	env.LyricsList = &list
	h.write(w, r, env)
}

func lyricsOf(t db.Track, lines []lyricLine, synced bool) structuredLyrics {
	return structuredLyrics{
		DisplayArtist: t.Media.Artist, DisplayTitle: titleOf(t),
		Lang: undetermined, Synced: synced, Lines: lines,
	}
}

func plainLines(text string) []lyricLine {
	raw := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	lines := make([]lyricLine, 0, len(raw))
	for _, l := range raw {
		lines = append(lines, lyricLine{Value: l})
	}
	return lines
}

// parseLRC reads the LRC form LRCLIB stores: "[mm:ss.xx] words" per line, a
// line with several times in front of it sung at each, and tag lines such as
// "[ar: ...]" that are not lyrics. The result is in the order the words are
// sung, which the file usually but not always is.
func parseLRC(text string) []lyricLine {
	var lines []lyricLine
	for raw := range strings.SplitSeq(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		rest := raw
		var starts []int64
		for strings.HasPrefix(rest, "[") {
			end := strings.IndexByte(rest, ']')
			if end < 0 {
				break
			}
			ms, ok := lrcTime(rest[1:end])
			if !ok {
				break
			}
			starts = append(starts, ms)
			rest = rest[end+1:]
		}
		if len(starts) == 0 {
			// A tag line, or a line with no time: not part of the song.
			continue
		}
		value := strings.TrimSpace(rest)
		for _, ms := range starts {
			lines = append(lines, lyricLine{Start: ptrTo(ms), Value: value})
		}
	}
	sort.SliceStable(lines, func(i, j int) bool { return *lines[i].Start < *lines[j].Start })
	return lines
}

// lrcTime reads "mm:ss.xx", "mm:ss.xxx" or "mm:ss" as milliseconds.
func lrcTime(s string) (int64, bool) {
	minutes, rest, ok := strings.Cut(s, ":")
	if !ok {
		return 0, false
	}
	m, err := strconv.Atoi(minutes)
	if err != nil || m < 0 {
		return 0, false
	}
	seconds, fraction, _ := strings.Cut(rest, ".")
	sec, err := strconv.Atoi(seconds)
	if err != nil || sec < 0 || sec > 59 {
		return 0, false
	}
	var ms int
	if fraction != "" {
		// Two digits are hundredths and three are thousandths; anything
		// longer is cut, since nothing sings to the microsecond.
		if len(fraction) > 3 {
			fraction = fraction[:3]
		}
		f, err := strconv.Atoi(fraction)
		if err != nil || f < 0 {
			return 0, false
		}
		for i := len(fraction); i < 3; i++ {
			f *= 10
		}
		ms = f
	}
	return int64(m)*60_000 + int64(sec)*1000 + int64(ms), true
}

func ptrTo[T any](v T) *T { return &v }

// albumInfo2 is getAlbumInfo2: the album's identity outside, by its id or by
// one of its songs, which the specification allows either of.
func (h *handler) albumInfo2(w http.ResponseWriter, r *http.Request, username string) {
	id, apiErr := requiredID(r)
	if apiErr != nil {
		h.fail(w, r, *apiErr)
		return
	}

	var artist, name string
	switch {
	case isAlbumID(id):
		var ok bool
		if artist, name, ok = parseAlbumID(id); !ok {
			h.fail(w, r, notFound("album"))
			return
		}
	case isSongID(id):
		t, apiErr := h.audioTrack(r, username, id)
		if apiErr != nil {
			h.fail(w, r, *apiErr)
			return
		}
		artist, name = t.Media.AlbumArtist, t.Media.Album
	default:
		h.fail(w, r, notFound("album"))
		return
	}

	// An album is not a row, so whether it exists is whether it has tracks,
	// which is the same check getAlbum makes.
	albums, err := h.lib.Albums(r.Context(), username, artist)
	if err != nil {
		h.fail(w, r, h.internal(r, "list albums", err))
		return
	}
	known := false
	for _, a := range albums {
		if a.Name == name {
			known = true
			break
		}
	}
	if !known {
		h.fail(w, r, notFound("album"))
		return
	}

	info := albumInfo{}
	switch row, err := h.lib.AlbumLookup(r.Context(), username, artist, name); {
	case err == nil:
		info.MusicBrainzID = row.ReleaseGroup
	case !errors.Is(err, db.ErrNotFound):
		h.fail(w, r, h.internal(r, "read the album's lookup", err))
		return
	}
	env := h.ok()
	env.AlbumInfo = &info
	h.write(w, r, env)
}
