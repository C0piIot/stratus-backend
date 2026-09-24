package subsonic_test

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

// starredLibrary is two albums by two artists, one track each, which is enough
// for every kind of subject to be told apart from the others.
func starredLibrary(t *testing.T) (l *library, hunter, rotar string) {
	t.Helper()
	l = newLibrary(t)
	h := l.add(t, "music/Homogenic/01 Hunter.flac", song("Björk", "Homogenic", "Hunter", 1))
	r := l.add(t, "music/Tri Repetae/01 Rotar.flac", song("Autechre", "Tri Repetae", "Rotar", 1))
	return l, songIDOf(h.ID), songIDOf(r.ID)
}

func mustOK(t *testing.T, l *library, method string, params ...string) map[string]any {
	t.Helper()
	env := response(t, get(t, l, method, query(append([]string{"f", "json"}, params...)...)))
	if got, _ := env["status"].(string); got != "ok" {
		t.Fatalf("%s %v = %v: %v", method, params, env["status"], env["error"])
	}
	return env
}

// starredStamp reads a starred attribute and checks it is a time, which is what
// a client parses it as.
func starredStamp(t *testing.T, what string, v map[string]any) bool {
	t.Helper()
	raw, present := v["starred"]
	if !present {
		return false
	}
	s, _ := raw.(string)
	if _, err := time.Parse(time.RFC3339, s); err != nil {
		t.Errorf("%s starred = %#v, want an RFC 3339 time", what, raw)
	}
	return true
}

// TestAStarComesBackEverywhere is the acceptance of #194: a star is a thing a
// client reads back in getStarred2, and a row it has already cached carries it
// wherever it appears next.
func TestAStarComesBackEverywhere(t *testing.T) {
	t.Parallel()
	l, hunter, _ := starredLibrary(t)

	mustOK(t, l, "star", "id", hunter)
	mustOK(t, l, "star", "albumId", albumIDOf("Björk", "Homogenic"))
	mustOK(t, l, "star", "artistId", artistIDOf("Björk"))

	env := mustOK(t, l, "getStarred2")
	list, _ := env["starred2"].(map[string]any)
	for bucket, want := range map[string]struct {
		field string
		names []string
	}{
		"artist": {"name", []string{"Björk"}},
		"album":  {"name", []string{"Homogenic"}},
		"song":   {"title", []string{"Hunter"}},
	} {
		if got := names(t, list[bucket], want.field); !same(got, want.names) {
			t.Errorf("starred2.%s = %v, want %v", bucket, got, want.names)
		}
		if items, _ := list[bucket].([]any); len(items) == 1 {
			item, _ := items[0].(map[string]any)
			if !starredStamp(t, "starred2."+bucket, item) {
				t.Errorf("starred2.%s carries no starred time: %v", bucket, item)
			}
		}
	}

	// And on the rows a client browses to, not only on the list of stars.
	album, _ := mustOK(t, l, "getAlbum", "id", albumIDOf("Björk", "Homogenic"))["album"].(map[string]any)
	if !starredStamp(t, "getAlbum", album) {
		t.Errorf("getAlbum is not starred: %v", album)
	}
	songs, _ := album["song"].([]any)
	if len(songs) != 1 || !starredStamp(t, "getAlbum's song", songs[0].(map[string]any)) {
		t.Errorf("getAlbum's song is not starred: %v", songs)
	}
	artist, _ := mustOK(t, l, "getArtist", "id", artistIDOf("Björk"))["artist"].(map[string]any)
	if !starredStamp(t, "getArtist", artist) {
		t.Errorf("getArtist is not starred: %v", artist)
	}

	// The other album is not, and says nothing rather than something empty.
	other, _ := mustOK(t, l, "getAlbum", "id", albumIDOf("Autechre", "Tri Repetae"))["album"].(map[string]any)
	if _, present := other["starred"]; present {
		t.Errorf("an album nobody starred carries starred: %v", other)
	}
}

// TestStarredIsAnAttributeInXML is the tag that goes wrong silently, as it
// does for every scalar in this protocol.
func TestStarredIsAnAttributeInXML(t *testing.T) {
	t.Parallel()
	l, hunter, _ := starredLibrary(t)
	mustOK(t, l, "star", "id", hunter)
	mustOK(t, l, "setRating", "id", hunter, "rating", "4")

	body := get(t, l, "getSong", query("id", hunter)).Body.String()
	if !strings.Contains(body, ` starred="`) || !strings.Contains(body, ` userRating="4"`) {
		t.Errorf("getSong = %s, want starred and userRating as attributes", body)
	}
}

// TestTheOldListingsAreStarredToo covers the folder-era clients, which send an
// album's id in id -- getAlbumList handed it to them as a Child -- and read the
// stars back from getStarred, where an album is a Child as well.
func TestTheOldListingsAreStarredToo(t *testing.T) {
	t.Parallel()
	l, _, rotar := starredLibrary(t)

	mustOK(t, l, "star", "id", albumIDOf("Autechre", "Tri Repetae"))
	mustOK(t, l, "star", "id", artistIDOf("Autechre"))
	mustOK(t, l, "star", "id", rotar)

	list, _ := mustOK(t, l, "getStarred")["starred"].(map[string]any)
	if got := names(t, list["artist"], "name"); !same(got, []string{"Autechre"}) {
		t.Errorf("getStarred artists = %v", got)
	}
	if got := names(t, list["song"], "title"); !same(got, []string{"Rotar"}) {
		t.Errorf("getStarred songs = %v", got)
	}
	albums, _ := list["album"].([]any)
	if len(albums) != 1 {
		t.Fatalf("getStarred albums = %v, want one", list["album"])
	}
	album, _ := albums[0].(map[string]any)
	if isDir, _ := album["isDir"].(bool); !isDir || album["title"] != "Tri Repetae" {
		t.Errorf("getStarred album = %v, want a Child with isDir", album)
	}

	old, _ := mustOK(t, l, "getAlbumList", "type", "starred")["albumList"].(map[string]any)
	if got := names(t, old["album"], "title"); !same(got, []string{"Tri Repetae"}) {
		t.Errorf("getAlbumList starred = %v", got)
	}
}

func TestUnstarForgets(t *testing.T) {
	t.Parallel()
	l, hunter, _ := starredLibrary(t)

	mustOK(t, l, "star", "id", hunter, "albumId", albumIDOf("Björk", "Homogenic"))
	mustOK(t, l, "unstar", "id", hunter, "albumId", albumIDOf("Björk", "Homogenic"))
	// Twice is the same answer: a client retries.
	mustOK(t, l, "unstar", "id", hunter)

	list, _ := mustOK(t, l, "getStarred2")["starred2"].(map[string]any)
	for _, bucket := range []string{"artist", "album", "song"} {
		if got, _ := list[bucket].([]any); len(got) != 0 {
			t.Errorf("starred2.%s = %v after unstarring, want nothing", bucket, got)
		}
	}
}

// TestRatingsOrderTheHighestShelf is setRating end to end: on a song, on an
// album, replaced, taken away, and what a home screen's "top rated" shows.
func TestRatingsOrderTheHighestShelf(t *testing.T) {
	t.Parallel()
	l, hunter, _ := starredLibrary(t)

	mustOK(t, l, "setRating", "id", albumIDOf("Björk", "Homogenic"), "rating", "3")
	mustOK(t, l, "setRating", "id", albumIDOf("Autechre", "Tri Repetae"), "rating", "5")
	mustOK(t, l, "setRating", "id", hunter, "rating", "2")

	shelf, _ := mustOK(t, l, "getAlbumList2", "type", "highest")["albumList2"].(map[string]any)
	if got := names(t, shelf["album"], "name"); !same(got, []string{"Tri Repetae", "Homogenic"}) {
		t.Errorf("highest = %v, want by rating", got)
	}
	if albums, _ := shelf["album"].([]any); len(albums) > 0 {
		if got := albums[0].(map[string]any)["userRating"]; got != float64(5) {
			t.Errorf("the top album's userRating = %v, want 5", got)
		}
	}

	s, _ := mustOK(t, l, "getSong", "id", hunter)["song"].(map[string]any)
	if got := s["userRating"]; got != float64(2) {
		t.Errorf("userRating = %v, want 2", got)
	}

	mustOK(t, l, "setRating", "id", hunter, "rating", "0")
	s, _ = mustOK(t, l, "getSong", "id", hunter)["song"].(map[string]any)
	if _, present := s["userRating"]; present {
		t.Errorf("after rating 0 the song still carries %v", s["userRating"])
	}
}

// TestAnnotationsAreRefusedWhenTheyNameNothing covers what a client can get
// wrong, and that a request is refused whole: a client told "ok" believes every
// id in it took.
func TestAnnotationsAreRefusedWhenTheyNameNothing(t *testing.T) {
	t.Parallel()
	l, hunter, _ := starredLibrary(t)

	tests := []struct {
		name   string
		method string
		params url.Values
		want   float64
	}{
		{"no id at all", "star", url.Values{}, 10},
		{"a song that is not there", "star", url.Values{"id": {songIDOf(424242)}}, 70},
		{"an id that is not an id", "star", url.Values{"id": {"nonsense"}}, 70},
		{"a folder", "star", url.Values{"id": {dirIDOf("music")}}, 70},
		{"an album nobody has", "star", url.Values{"albumId": {albumIDOf("Björk", "Vespertine")}}, 70},
		{"an artist nobody has", "star", url.Values{"artistId": {artistIDOf("Nobody")}}, 70},
		{"a song id in albumId", "star", url.Values{"albumId": {hunter}}, 70},
		{"one good and one bad", "star", url.Values{"id": {hunter, songIDOf(424242)}}, 70},
		{"unstar with no id", "unstar", url.Values{}, 10},
		{"a rating with no id", "setRating", url.Values{"rating": {"3"}}, 10},
		{"a rating of a folder", "setRating", url.Values{"id": {dirIDOf("music")}, "rating": {"3"}}, 70},
		{"no rating", "setRating", url.Values{"id": {hunter}}, 10},
		{"a rating off the scale", "setRating", url.Values{"id": {hunter}, "rating": {"6"}}, 10},
		{"a negative rating", "setRating", url.Values{"id": {hunter}, "rating": {"-1"}}, 10},
		{"a rating of a song that is not there", "setRating", url.Values{"id": {songIDOf(424242)}, "rating": {"3"}}, 70},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := url.Values{"c": {"stratus-tests"}, "u": {username}, "p": {password}, "f": {"json"}}
			for k, v := range tt.params {
				q[k] = v
			}
			if code := errorCode(t, get(t, l, tt.method, q.Encode())); code != tt.want {
				t.Errorf("code = %v, want %v", code, tt.want)
			}
		})
	}

	list, _ := mustOK(t, l, "getStarred2")["starred2"].(map[string]any)
	if got, _ := list["song"].([]any); len(got) != 0 {
		t.Errorf("a refused request starred %v", got)
	}
}

// TestSeveralIDsInOneStar is the shape a client uses to star a selection.
func TestSeveralIDsInOneStar(t *testing.T) {
	t.Parallel()
	l, hunter, rotar := starredLibrary(t)

	q := url.Values{"c": {"stratus-tests"}, "u": {username}, "p": {password}, "f": {"json"},
		"id": {hunter, rotar}}
	if env := response(t, get(t, l, "star", q.Encode())); env["status"] != "ok" {
		t.Fatalf("star = %v", env["error"])
	}

	list, _ := mustOK(t, l, "getStarred2")["starred2"].(map[string]any)
	if got := names(t, list["song"], "title"); len(got) != 2 {
		t.Errorf("starred songs = %v, want both", got)
	}
}

// TestAScrobbleIsCounted is the acceptance of #195: a scrobble counts a play,
// the song says so, and its album moves onto the two shelves plays order.
func TestAScrobbleIsCounted(t *testing.T) {
	t.Parallel()
	l, hunter, rotar := starredLibrary(t)

	mustOK(t, l, "scrobble", "id", hunter, "time", "1700000000000")
	mustOK(t, l, "scrobble", "id", hunter, "time", "1700000100000")
	mustOK(t, l, "scrobble", "id", rotar, "time", "1700000200000")

	s, _ := mustOK(t, l, "getSong", "id", hunter)["song"].(map[string]any)
	if got := s["playCount"]; got != float64(2) {
		t.Errorf("playCount = %v, want 2", got)
	}
	if got := s["played"]; got != "2023-11-14T22:15:00Z" {
		t.Errorf("played = %v, want the later of the two", got)
	}

	frequent, _ := mustOK(t, l, "getAlbumList2", "type", "frequent")["albumList2"].(map[string]any)
	if got := names(t, frequent["album"], "name"); !same(got, []string{"Homogenic", "Tri Repetae"}) {
		t.Errorf("frequent = %v, want most played first", got)
	}
	if albums, _ := frequent["album"].([]any); len(albums) > 0 {
		if got := albums[0].(map[string]any)["playCount"]; got != float64(2) {
			t.Errorf("the most played album's playCount = %v, want its tracks' 2", got)
		}
	}
	recent, _ := mustOK(t, l, "getAlbumList", "type", "recent")["albumList"].(map[string]any)
	if got := names(t, recent["album"], "title"); !same(got, []string{"Tri Repetae", "Homogenic"}) {
		t.Errorf("recent = %v, want most recently played first", got)
	}
}

// TestAnOfflineSessionIsOneScrobble is how a client that played with no network
// catches up: every id at once, each with its own time.
func TestAnOfflineSessionIsOneScrobble(t *testing.T) {
	t.Parallel()
	l, hunter, rotar := starredLibrary(t)

	q := url.Values{"c": {"stratus-tests"}, "u": {username}, "p": {password}, "f": {"json"},
		"id": {hunter, rotar, hunter}, "time": {"1700000000000", "1700000100000"}}
	if env := response(t, get(t, l, "scrobble", q.Encode())); env["status"] != "ok" {
		t.Fatalf("scrobble = %v", env["error"])
	}

	s, _ := mustOK(t, l, "getSong", "id", rotar)["song"].(map[string]any)
	if got := s["played"]; got != "2023-11-14T22:15:00Z" {
		t.Errorf("played = %v, want the time sent with it", got)
	}
	// The third id had no time, so it was played now, which is later.
	s, _ = mustOK(t, l, "getSong", "id", hunter)["song"].(map[string]any)
	if got := s["playCount"]; got != float64(2) {
		t.Errorf("playCount = %v, want 2", got)
	}
	if got, _ := s["played"].(string); got <= "2023-11-14T22:15:00Z" {
		t.Errorf("played = %v, want now", got)
	}
}

// TestOnlyAScrobbleCounts covers the two ways a play could be counted that are
// not one: "now playing", and the bytes being fetched.
func TestOnlyAScrobbleCounts(t *testing.T) {
	t.Parallel()
	l, hunter, _ := starredLibrary(t)

	mustOK(t, l, "scrobble", "id", hunter, "submission", "false")
	if rec := get(t, l, "stream", query("id", hunter)); rec.Code != 200 {
		t.Fatalf("stream = %d", rec.Code)
	}

	s, _ := mustOK(t, l, "getSong", "id", hunter)["song"].(map[string]any)
	if got, present := s["playCount"]; present {
		t.Errorf("playCount = %v after now-playing and a stream, want none", got)
	}
}

func TestAScrobbleIsRefusedWhole(t *testing.T) {
	t.Parallel()
	l, hunter, _ := starredLibrary(t)

	tests := []struct {
		name   string
		params url.Values
		want   float64
	}{
		{"no id", url.Values{}, 10},
		{"a song that is not there", url.Values{"id": {songIDOf(424242)}}, 70},
		{"an album", url.Values{"id": {albumIDOf("Björk", "Homogenic")}}, 70},
		{"a time that is not a time", url.Values{"id": {hunter}, "time": {"yesterday"}}, 10},
		{"a time before the epoch", url.Values{"id": {hunter}, "time": {"-5"}}, 10},
		{"more times than ids", url.Values{"id": {hunter}, "time": {"1", "2"}}, 10},
		{"one good and one bad", url.Values{"id": {hunter, songIDOf(424242)}}, 70},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := url.Values{"c": {"stratus-tests"}, "u": {username}, "p": {password}, "f": {"json"}}
			for k, v := range tt.params {
				q[k] = v
			}
			if code := errorCode(t, get(t, l, "scrobble", q.Encode())); code != tt.want {
				t.Errorf("code = %v, want %v", code, tt.want)
			}
		})
	}

	s, _ := mustOK(t, l, "getSong", "id", hunter)["song"].(map[string]any)
	if got, present := s["playCount"]; present {
		t.Errorf("a refused scrobble counted %v", got)
	}
}
