package lookup

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
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
//
// Asked once with the artist as tagged and, when that finds nothing and the
// tag reads as several names, once more with the first of them. A credit
// such as "Architecture In Helsinki, Cameron Bird" is what a track ripped
// from a video site carries, and MusicBrainz files the record under the band:
// the full credit matched nothing and the first name matched with a score of
// a hundred, on the first real library this ran against.
//
// Then the same again with the album's edition taken off, when it has one:
// the search is a phrase, so "The Suburbs (Deluxe)" finds nothing at all
// while "The Suburbs" scores a hundred. On that same library it was 20 of the
// 73 albums nothing was found for, and 18 of them came back that way.
func (s *Service) releaseGroup(ctx context.Context, artist, album string) (string, error) {
	titles := []string{album}
	if plain := withoutEdition(album); plain != album {
		titles = append(titles, plain)
	}
	for _, title := range titles {
		for _, name := range creditNames(artist) {
			id, err := s.searchReleaseGroup(ctx, name, title)
			if err != nil || id != "" {
				return id, err
			}
		}
	}
	return "", nil
}

// editionWords are what a bracket at the end of an album's title says when
// it names a pressing of the record rather than a different one. Not "live",
// "remix" or "acoustic", which are records of their own with covers of their
// own, and a wrong cover is worse than none.
var editionWords = regexp.MustCompile(`(?i)deluxe|remaster|edition|reissue|bonus|expanded|anniversary`)

// editionSuffix is a bracket at the end of a title, or a ": Deluxe" or
// " - Deluxe ..." tail, which is the other way a store writes the same thing.
var editionSuffix = regexp.MustCompile(`(?i)\s*(?:\(([^()]*)\)|\[([^\[\]]*)\]|[-:]\s+(deluxe.*))\s*$`)

// withoutEdition is the album's title with its edition taken off, or the
// title as it was when it names none.
func withoutEdition(album string) string {
	m := editionSuffix.FindStringSubmatchIndex(album)
	if m == nil || m[0] == 0 {
		return album
	}
	said := editionSuffix.FindStringSubmatch(album)
	if !editionWords.MatchString(said[1] + said[2] + said[3]) {
		return album
	}
	return album[:m[0]]
}

// creditSeparators are what joins several names into one artist tag, each
// with the spaces that keep it from cutting a name in two: "AC/DC" has no
// space around its slash and "Simon & Garfunkel" is one act, which is why
// the ampersand is the last tried and only after a comma has not split it.
var creditSeparators = []string{", ", "; ", " feat. ", " feat ", " ft. ", " featuring ", " & "}

// creditNames is the artist as tagged and, when the tag reads as several
// names, the first of them: at most two questions for one album.
func creditNames(artist string) []string {
	names := []string{artist}
	for _, sep := range creditSeparators {
		first, _, found := strings.Cut(artist, sep)
		first = strings.TrimSpace(first)
		if found && first != "" && !strings.EqualFold(first, artist) {
			return append(names, first)
		}
	}
	return names
}

// searchReleaseGroup is one search: the album as a phrase under the artist
// as a phrase, and the first result scored high enough to believe.
func (s *Service) searchReleaseGroup(ctx context.Context, artist, album string) (string, error) {
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
