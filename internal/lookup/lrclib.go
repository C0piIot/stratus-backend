package lookup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/stratus-private-cloud/stratus-backend/internal/db"
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
//
// Asked with the artist as tagged and, when LRCLIB has nothing under that and
// the tag reads as several names, once more with the first of them -- the
// same second question the album search asks, for the same tracks: a credit
// such as "Estelle, Kanye West, Keith Harris, William Adams" is what a track
// ripped from a video site carries, and LRCLIB files the words under the
// first name. Only "nothing there" earns the second question; anything else
// is an answer about the service and not about the name.
func (s *Service) lyrics(ctx context.Context, m db.Media) (lyricsAnswer, error) {
	var err error
	for _, name := range creditNames(m.Artist) {
		var found lyricsAnswer
		found, err = s.lyricsBy(ctx, name, m)
		if !errors.Is(err, errNothing) {
			return found, err
		}
	}
	return lyricsAnswer{}, err
}

// lyricsBy is one exact lookup, under one artist name.
func (s *Service) lyricsBy(ctx context.Context, artist string, m db.Media) (lyricsAnswer, error) {
	q := url.Values{"artist_name": {artist}, "track_name": {m.Title}}
	if m.Album != "" {
		q.Set("album_name", m.Album)
	}
	if m.DurationMS > 0 {
		q.Set("duration", strconv.FormatInt((m.DurationMS+500)/1000, 10))
	}
	body, err := s.get(ctx, "lrclib", s.lrclib+"/api/get?"+q.Encode(), maxLyrics, "application/json")
	if err != nil {
		// LRCLIB's 503 is about this track and not about this client: one
		// it does not hold is one it goes and fetches from outside while
		// answering, and "the server is busy" is what it says while that
		// fetch is under way. Measured against the live service: a title
		// answered 503 and then 200 with its words minutes later, another
		// 503 twice and then 404. So it is no answer yet -- not a refusal,
		// since the pass goes on to the next track, and not "they do not
		// hold it", which parked most of the first real library for a
		// month behind an answer an hour away. A 429 is still a refusal,
		// since that one is about the client.
		var status statusError
		if errors.As(err, &status) && status.code == http.StatusServiceUnavailable {
			return lyricsAnswer{}, fmt.Errorf("%w: LRCLIB was still fetching it: %w", errUnreachable, status)
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
