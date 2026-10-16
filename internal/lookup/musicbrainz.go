package lookup

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// maxSearch bounds a search answer. Five results with their releases listed
// is a few kilobytes; a megabyte is not a search answer.
const maxSearch = 1 << 20

// minScore is the score a result has to reach to be believed. MusicBrainz
// scores a search out of a hundred, and an exact title under an exact artist
// scores a hundred; what sits under ninety is a different record that shares
// a word, and a wrong cover is worse than none.
const minScore = 90

// releaseGroup searches MusicBrainz for the album, and answers its release
// group id or nothing.
//
// A release group and not a release: the archive files front covers under
// either, and a group is every pressing of the record at once, so it has a
// cover when any one of them does. Which pressing the user has is not a
// question the tags can answer without the id Picard writes.
func (s *Service) releaseGroup(ctx context.Context, artist, album string) (string, error) {
	q := url.Values{
		"query": {`releasegroup:` + lucene(album) + ` AND artist:` + lucene(artist)},
		"fmt":   {"json"},
		"limit": {"5"},
	}
	body, err := s.get(ctx, "musicbrainz", s.musicBrainz+"/ws/2/release-group/?"+q.Encode(), maxSearch, "application/json")
	switch {
	case err == nil:
	case isNothing(err):
		return "", nil
	default:
		return "", err
	}

	var answer struct {
		Groups []struct {
			ID    string `json:"id"`
			Score score  `json:"score"`
		} `json:"release-groups"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return "", fmt.Errorf("%w: musicbrainz answered something that is not JSON: %w", errUnreachable, err)
	}
	for _, g := range answer.Groups {
		if int(g.Score) >= minScore && isUUID(g.ID) {
			return g.ID, nil
		}
	}
	return "", nil
}

// score is the relevance MusicBrainz attaches to a search result. A number
// in its JSON today and a string in its XML, so both are taken.
type score int

func (s *score) UnmarshalJSON(b []byte) error {
	text := strings.Trim(string(b), `"`)
	if text == "" || text == "null" {
		*s = 0
		return nil
	}
	n, err := strconv.Atoi(text)
	if err != nil {
		return err
	}
	*s = score(n)
	return nil
}

// lucene quotes a tag as a phrase in the query language the search speaks,
// escaping the two characters that would end or alter it.
func lucene(text string) string {
	text = strings.ReplaceAll(text, `\`, `\\`)
	text = strings.ReplaceAll(text, `"`, `\"`)
	return `"` + text + `"`
}

// isUUID says whether an id is the shape MusicBrainz issues, which is the
// shape that may go into a URL path.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				return false
			}
		}
	}
	return true
}
