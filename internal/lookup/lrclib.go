package lookup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
		// LRCLIB's 503 is about this lookup and not about this client: a
		// track it does not hold is one it goes and fetches from outside
		// while answering, and "the server is busy" is what it says instead
		// when it will not. Measured against the live service: the same
		// title answers 200 when it is in their cache and 503 when it is
		// not. So it is this track in an hour, and the pass goes on -- a
		// 429 is still a refusal, since that one is about the client.
		var status statusError
		if errors.As(err, &status) && status.code == http.StatusServiceUnavailable {
			return lyricsAnswer{}, fmt.Errorf("%w: busy, not held: %w", errUnreachable, status)
		}
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
