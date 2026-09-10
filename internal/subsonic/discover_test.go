package subsonic_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// TestGetAlbumList2 walks the types a client's home screen is made of. The
// library is built so that no two orders agree, which is what makes the
// assertions distinguish an order from a coincidence.
func TestGetAlbumList2(t *testing.T) {
	t.Parallel()
	l := newLibrary(t)
	l.add(t, "music/1.flac", dated("Zomby", "Aaron", 2011, "Electronic"))
	l.add(t, "music/2.flac", dated("Autechre", "Zeta", 1994, "Electronic"))
	l.add(t, "music/3.flac", dated("Móveis", "Meia", 2003, "Rock"))

	tests := []struct {
		name  string
		query []string
		want  []string
	}{
		{name: "by name", query: []string{"type", "alphabeticalByName"}, want: []string{"Aaron", "Meia", "Zeta"}},
		{name: "by artist", query: []string{"type", "alphabeticalByArtist"}, want: []string{"Zeta", "Meia", "Aaron"}},
		{
			name:  "by year",
			query: []string{"type", "byYear", "fromYear", "1990", "toYear", "2020"},
			want:  []string{"Zeta", "Meia", "Aaron"},
		},
		{
			// The protocol asks for a descending list by giving the range
			// backwards. A client that does this is not making a mistake.
			name:  "by year, backwards",
			query: []string{"type", "byYear", "fromYear", "2020", "toYear", "1990"},
			want:  []string{"Aaron", "Meia", "Zeta"},
		},
		{
			name:  "by year, a range that excludes most of it",
			query: []string{"type", "byYear", "fromYear", "2000", "toYear", "2005"},
			want:  []string{"Meia"},
		},
		{name: "by genre", query: []string{"type", "byGenre", "genre", "Rock"}, want: []string{"Meia"}},
		{
			// Ordered by arrival, newest first, which is the reverse of the
			// order they were written above.
			name:  "newest",
			query: []string{"type", "newest"},
			want:  []string{"Meia", "Zeta", "Aaron"},
		},
		{
			// Accepted and empty: the shelf is honest about there being no play
			// counts rather than filled with something else.
			name:  "most played, which nothing counts",
			query: []string{"type", "frequent"},
			want:  []string{},
		},
		{name: "highest rated, which nothing rates", query: []string{"type", "highest"}, want: []string{}},
		{name: "recently played", query: []string{"type", "recent"}, want: []string{}},
		{name: "starred", query: []string{"type", "starred"}, want: []string{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := response(t, get(t, l, "getAlbumList2", query(append(tt.query, "f", "json")...)))
			list, ok := env["albumList2"].(map[string]any)
			if !ok {
				t.Fatalf("no albumList2 object in %v", env)
			}
			if got := names(t, list["album"], "name"); !same(got, tt.want) {
				t.Errorf("albums = %v, want %v", got, tt.want)
			}
		})
	}

	// A random listing is a different set by definition, so what is pinned is
	// that the size is honoured.
	env := response(t, get(t, l, "getAlbumList2", query("f", "json", "type", "random", "size", "2")))
	list, _ := env["albumList2"].(map[string]any)
	if got, ok := list["album"].([]any); !ok || len(got) != 2 {
		t.Errorf("a random listing of two returned %#v", list["album"])
	}
}

// TestAlbumListPages is the property a client depends on to walk a library it
// cannot hold: size and offset, with nothing repeated and nothing skipped.
func TestAlbumListPages(t *testing.T) {
	t.Parallel()
	l := newLibrary(t)
	l.add(t, "music/1.flac", dated("A", "One", 2001, "Rock"))
	l.add(t, "music/2.flac", dated("B", "Two", 2002, "Rock"))
	l.add(t, "music/3.flac", dated("C", "Three", 2003, "Rock"))

	var seen []string
	for offset := 0; offset < 4; offset += 2 {
		q := query("f", "json", "type", "alphabeticalByArtist", "size", "2", "offset", strconv.Itoa(offset))
		env := response(t, get(t, l, "getAlbumList2", q))
		list, _ := env["albumList2"].(map[string]any)
		seen = append(seen, names(t, list["album"], "name")...)
	}
	if want := []string{"One", "Two", "Three"}; !same(seen, want) {
		t.Errorf("paging returned %v, want %v exactly once each", seen, want)
	}

	// No size at all is ten, which is the specification's default and not this
	// server's preference. A client that omits it is asking for ten.
	env := response(t, get(t, l, "getAlbumList2", query("f", "json", "type", "alphabeticalByName")))
	list, _ := env["albumList2"].(map[string]any)
	if got := names(t, list["album"], "name"); len(got) != 3 {
		t.Errorf("albums with no size = %v, want all three of a library smaller than the default", got)
	}
}

// TestSearch3 is the endpoint a client's search box is, and the empty query is
// how it downloads a library for offline use.
func TestSearch3(t *testing.T) {
	t.Parallel()
	l := newLibrary(t)
	l.add(t, "music/1.flac", dated("Boards of Canada", "Geogaddi", 2002, "Electronic"))
	l.add(t, "music/2.flac", dated("Autechre", "Tri Repetae", 1995, "Electronic"))

	env := response(t, get(t, l, "search3", query("f", "json", "query", "aute")))
	found, ok := env["searchResult3"].(map[string]any)
	if !ok {
		t.Fatalf("no searchResult3 object in %v", env)
	}
	if got := names(t, found["artist"], "name"); !same(got, []string{"Autechre"}) {
		t.Errorf("artists = %v", got)
	}
	// The album's own name does not contain the term, so that bucket is empty
	// even though its artist matched: three questions, three answers.
	if got := names(t, found["album"], "name"); len(got) != 0 {
		t.Errorf("albums = %v, want none", got)
	}
	// A track is matched on its artist as well as its title, because that is
	// what a search box is used for.
	if got := names(t, found["song"], "title"); len(got) != 1 {
		t.Errorf("songs = %v, want that artist's track", got)
	}

	// The empty query, which the specification requires and every client uses.
	env = response(t, get(t, l, "search3", query("f", "json")))
	found, _ = env["searchResult3"].(map[string]any)
	if got := names(t, found["artist"], "name"); len(got) != 2 {
		t.Errorf("an empty query found %v artists, want the library", got)
	}
	if got := names(t, found["album"], "name"); len(got) != 2 {
		t.Errorf("an empty query found %v albums, want the library", got)
	}

	// Each bucket pages on its own, which is how a client walks them.
	env = response(t, get(t, l, "search3", query("f", "json", "artistCount", "1", "albumCount", "0")))
	found, _ = env["searchResult3"].(map[string]any)
	if got := names(t, found["artist"], "name"); len(got) != 1 {
		t.Errorf("artistCount=1 returned %v", got)
	}
	if got := names(t, found["album"], "name"); len(got) != 0 {
		t.Errorf("albumCount=0 returned %v", got)
	}
}

// TestSearch3IsCaseInsensitive is the protocol-level end of the decision that
// the folding happens in Go: the two drivers do not agree on what lower() means
// for an accented capital, so neither is asked.
func TestSearch3IsCaseInsensitive(t *testing.T) {
	t.Parallel()
	l := newLibrary(t)
	l.add(t, "music/1.flac", dated("BJÖRK", "HOMOGENIC", 1997, "Electronic"))

	for _, term := range []string{"björk", "BJÖRK", "BjÖrK"} {
		env := response(t, get(t, l, "search3", query("f", "json", "query", term)))
		found, _ := env["searchResult3"].(map[string]any)
		if got := names(t, found["artist"], "name"); !same(got, []string{"BJÖRK"}) {
			t.Errorf("search for %q found %v", term, got)
		}
	}
}

// TestGetGenresIsXMLText is the tag trap of this PR. Everywhere else in the API
// a name is an attribute; the genre is the element's own text, and writing it as
// an attribute produces valid XML that no client reads.
func TestGetGenresIsXMLText(t *testing.T) {
	t.Parallel()
	l := newLibrary(t)
	l.add(t, "music/1.flac", dated("A", "One", 2001, "Rock"))
	l.add(t, "music/2.flac", dated("B", "Two", 2002, "Rock"))
	l.add(t, "music/3.flac", dated("C", "Three", 2003, "Jazz"))

	rec := get(t, l, "getGenres", query())
	if rec.Code != 200 {
		t.Fatalf("getGenres = %d", rec.Code)
	}
	want := `<genres>` +
		`<genre songCount="1" albumCount="1">Jazz</genre>` +
		`<genre songCount="2" albumCount="2">Rock</genre>` +
		`</genres>`
	if got := rec.Body.String(); !strings.Contains(got, want) {
		t.Errorf("getGenres =\n%s\nwant it to contain\n%s", got, want)
	}

	// And "value" in JSON, which is a third name for the same thing.
	env := response(t, get(t, l, "getGenres", query("f", "json")))
	list, ok := env["genres"].(map[string]any)
	if !ok {
		t.Fatalf("no genres object in %v", env)
	}
	if got := names(t, list["genre"], "value"); !same(got, []string{"Jazz", "Rock"}) {
		t.Errorf("genres = %v", got)
	}
}

// TestGetSongsByGenre is why getGenres is worth answering at all: without it a
// client shows a list of genres that cannot be opened.
func TestGetSongsByGenre(t *testing.T) {
	t.Parallel()
	l := newLibrary(t)
	rock := dated("A", "One", 2001, "Rock")
	rock.Title = "Rocking"
	l.add(t, "music/1.flac", rock)
	l.add(t, "music/2.flac", dated("C", "Three", 2003, "Jazz"))

	env := response(t, get(t, l, "getSongsByGenre", query("f", "json", "genre", "Rock")))
	list, ok := env["songsByGenre"].(map[string]any)
	if !ok {
		t.Fatalf("no songsByGenre object in %v", env)
	}
	if got := names(t, list["song"], "title"); !same(got, []string{"Rocking"}) {
		t.Errorf("songs = %v", got)
	}

	// A genre nothing is in is an empty list and not an error: the genre may
	// have existed when the client cached it.
	env = response(t, get(t, l, "getSongsByGenre", query("f", "json", "genre", "Skiffle")))
	list, _ = env["songsByGenre"].(map[string]any)
	if got := names(t, list["song"], "title"); len(got) != 0 {
		t.Errorf("an unknown genre returned %v", got)
	}
}

func TestGetRandomSongs(t *testing.T) {
	t.Parallel()
	l := newLibrary(t)
	l.add(t, "music/1.flac", dated("A", "One", 2001, "Rock"))
	l.add(t, "music/2.flac", dated("B", "Two", 2002, "Jazz"))

	env := response(t, get(t, l, "getRandomSongs", query("f", "json", "size", "1")))
	list, ok := env["randomSongs"].(map[string]any)
	if !ok {
		t.Fatalf("no randomSongs object in %v", env)
	}
	if got := names(t, list["song"], "title"); len(got) != 1 {
		t.Errorf("songs = %v, want the size asked for", got)
	}

	// It takes the same filters as the album listing, which is how a client
	// shuffles one genre or one decade.
	env = response(t, get(t, l, "getRandomSongs", query("f", "json", "genre", "Jazz", "fromYear", "2002", "toYear", "2002")))
	list, _ = env["randomSongs"].(map[string]any)
	if got := names(t, list["song"], "title"); len(got) != 1 {
		t.Errorf("a filtered shuffle returned %v", got)
	}
}

// TestTheLegacyListingsAnswerTheSameData covers the endpoints that predate the
// ID3 ones. They are not aliases: an album is a Child there, with isDir set,
// which is a different shape for the same album.
func TestTheLegacyListingsAnswerTheSameData(t *testing.T) {
	t.Parallel()
	l := newLibrary(t)
	l.add(t, "music/1.flac", dated("Autechre", "Tri Repetae", 1995, "Electronic"))

	env := response(t, get(t, l, "getAlbumList", query("f", "json", "type", "alphabeticalByName")))
	list, ok := env["albumList"].(map[string]any)
	if !ok {
		t.Fatalf("no albumList object in %v", env)
	}
	album := only(t, list["album"])
	if got, _ := album["title"].(string); got != "Tri Repetae" {
		t.Errorf("title = %v", album["title"])
	}
	// A Child says whether it can be played, and an album cannot.
	if isDir, isBool := album["isDir"].(bool); !isBool || !isDir {
		t.Errorf("isDir = %#v, want true on an album rendered as a child", album["isDir"])
	}

	env = response(t, get(t, l, "search2", query("f", "json", "query", "repetae")))
	found, ok := env["searchResult2"].(map[string]any)
	if !ok {
		t.Fatalf("no searchResult2 object in %v", env)
	}
	album = only(t, found["album"])
	if isDir, _ := album["isDir"].(bool); !isDir {
		t.Errorf("search2 album = %v, want a child with isDir", album)
	}
	if got := names(t, found["song"], "title"); len(got) != 1 {
		t.Errorf("search2 songs = %v", got)
	}

	// Its artists are the ID3 shape even here, because the older Artist type
	// carries an id and a name and so does this one.
	env = response(t, get(t, l, "search2", query("f", "json", "query", "aute")))
	found, _ = env["searchResult2"].(map[string]any)
	if got := names(t, found["artist"], "name"); !same(got, []string{"Autechre"}) {
		t.Errorf("search2 artists = %v", got)
	}
}

// TestStarredIsEmptyAndPresent is the difference between "no favourites" and "I
// cannot answer that", which a client acts on differently: the first is a blank
// list, the second is a broken server.
func TestStarredIsEmptyAndPresent(t *testing.T) {
	t.Parallel()
	l := newLibrary(t)
	l.add(t, "music/1.flac", dated("A", "One", 2001, "Rock"))

	for _, method := range []string{"getStarred2", "getStarred"} {
		env := response(t, get(t, l, method, query("f", "json")))
		if got, _ := env["status"].(string); got != "ok" {
			t.Fatalf("%s = %v: %v", method, env["status"], env["error"])
		}
		key := strings.TrimPrefix(method, "get")
		key = strings.ToLower(key[:1]) + key[1:]
		list, ok := env[key].(map[string]any)
		if !ok {
			t.Fatalf("no %s object in %v", key, env)
		}
		for _, bucket := range []string{"artist", "album", "song"} {
			if got, isList := list[bucket].([]any); !isList || len(got) != 0 {
				t.Errorf("%s.%s = %#v, want an empty array", key, bucket, list[bucket])
			}
		}
	}
}

// TestListingParameters covers what a client can get wrong, and what it cannot.
// A missing type is a real error; a misspelled size is not, because every size
// has a default the client is entitled to rely on.
func TestListingParameters(t *testing.T) {
	t.Parallel()
	l := newLibrary(t)
	l.add(t, "music/1.flac", dated("A", "One", 2001, "Rock"))

	refused := []struct {
		name   string
		method string
		query  []string
		want   float64
	}{
		{name: "no type", method: "getAlbumList2", query: nil, want: 10},
		{name: "a type nothing knows", method: "getAlbumList2", query: []string{"type", "byVibe"}, want: 10},
		{name: "by genre with no genre", method: "getAlbumList2", query: []string{"type", "byGenre"}, want: 10},
		{
			name:   "by year with only one end",
			method: "getAlbumList2",
			query:  []string{"type", "byYear", "fromYear", "1990"},
			want:   10,
		},
		{name: "no type on the legacy listing", method: "getAlbumList", query: nil, want: 10},
		{name: "songs of no genre", method: "getSongsByGenre", query: nil, want: 10},
	}
	for _, tt := range refused {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			q := query(append(tt.query, "f", "json")...)
			if code := errorCode(t, get(t, l, tt.method, q)); code != tt.want {
				t.Errorf("code = %v, want %v", code, tt.want)
			}
		})
	}

	// Nonsense where a number belongs falls back to the default rather than
	// refusing a whole screen.
	env := response(t, get(t, l, "getAlbumList2", query("f", "json", "type", "alphabeticalByName", "size", "lots")))
	list, _ := env["albumList2"].(map[string]any)
	if got := names(t, list["album"], "name"); len(got) != 1 {
		t.Errorf("a nonsense size returned %v", got)
	}
	// And a size beyond the specification's cap is capped, not refused: it is
	// what stands between one request and a library in memory.
	env = response(t, get(t, l, "getAlbumList2", query("f", "json", "type", "alphabeticalByName", "size", "100000")))
	if got, _ := env["status"].(string); got != "ok" {
		t.Errorf("an oversized request = %v: %v", env["status"], env["error"])
	}
}

// dated is a track with the two things every listing sorts or filters by.
func dated(artist, album string, year int, genre string) db.Media {
	m := song(artist, album, "Track", 1)
	m.Year, m.Genre = year, genre
	return m
}

// names pulls one field out of a JSON array of objects, which is what most of
// these assertions are. A missing array is an empty answer and not a failure:
// several cases are about a listing being legitimately empty.
func names(t *testing.T, v any, field string) []string {
	t.Helper()
	list, ok := v.([]any)
	if !ok {
		if v != nil {
			t.Fatalf("got %#v, want an array", v)
		}
		return nil
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		m, isObject := item.(map[string]any)
		if !isObject {
			t.Fatalf("got %#v, want an object", item)
		}
		out = append(out, str(t, m[field]))
	}
	return out
}

func same(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
