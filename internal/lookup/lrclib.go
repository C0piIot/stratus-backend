package lookup

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// maxLyrics bounds an answer. The words of a song are a few kilobytes, with
// a time in front of each line twice that.
const maxLyrics = 1 << 20

// lyricsAnswer is what LRCLIB says about a track.
type lyricsAnswer struct {
	Plain, Synced string
	Instrumental  bool
}

// lyrics asks LRCLIB for a track's words by what the tags say about it.
//
// The exact lookup and not the search: /get matches the title and the artist
// as recorded and the duration within a couple of seconds, so what comes back
// is this recording's words or nothing, while a search answers the nearest
// thing it has and the nearest thing is sometimes a different song. The album
// and the duration go along when the tags know them, since both narrow it.
func (s *Service) lyrics(ctx context.Context, m db.Media) (lyricsAnswer, error) {
	q := url.Values{"artist_name": {m.Artist}, "track_name": {m.Title}}
	if m.Album != "" {
		q.Set("album_name", m.Album)
	}
	if m.DurationMS > 0 {
		q.Set("duration", strconv.FormatInt((m.DurationMS+500)/1000, 10))
	}
	body, err := s.get(ctx, "lrclib", s.lrclib+"/api/get?"+q.Encode(), maxLyrics, "application/json")
	if err != nil {
		return lyricsAnswer{}, err
	}

	var answer struct {
		Plain        string `json:"plainLyrics"`
		Synced       string `json:"syncedLyrics"`
		Instrumental bool   `json:"instrumental"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return lyricsAnswer{}, fmt.Errorf("%w: lrclib answered something that is not JSON: %w", errUnreachable, err)
	}
	found := lyricsAnswer{Plain: answer.Plain, Synced: answer.Synced, Instrumental: answer.Instrumental}
	if !found.Instrumental && found.Plain == "" && found.Synced == "" {
		return lyricsAnswer{}, fmt.Errorf("%w: lrclib answered a record with no words in it", errNothing)
	}
	return found, nil
}
