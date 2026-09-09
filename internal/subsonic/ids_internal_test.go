package subsonic

import "testing"

// The id parsers all take a string a client sent, so every way of getting one
// wrong is a case here rather than a branch nothing reaches. The round trips
// that matter -- an id handed out and handed back -- are asserted end to end in
// browse_test.go, which never builds an id itself.
func TestParseIDs(t *testing.T) {
	t.Parallel()

	t.Run("song", func(t *testing.T) {
		t.Parallel()
		if got, ok := parseSongID(songID(42)); !ok || got != 42 {
			t.Errorf("parseSongID = %d, %v", got, ok)
		}
		for _, id := range []string{"", "42", "ar-42", "tr-", "tr-abc", "tr-0", "tr--1", "tr-1.5"} {
			if _, ok := parseSongID(id); ok {
				t.Errorf("parseSongID(%q) accepted it", id)
			}
		}
	})

	t.Run("artist", func(t *testing.T) {
		t.Parallel()
		// A name that means something in a URL and in base64 alike.
		const name = "AC/DC & Friends +1"
		if got, ok := parseArtistID(artistID(name)); !ok || got != name {
			t.Errorf("parseArtistID = %q, %v", got, ok)
		}
		// "ar-Q" is one base64 character, which is a length no encoding can
		// have produced.
		for _, id := range []string{"", "ar", "al-QQ", "ar-", "ar-!!!!", "ar-Q"} {
			if _, ok := parseArtistID(id); ok {
				t.Errorf("parseArtistID(%q) accepted it", id)
			}
		}
	})

	t.Run("album", func(t *testing.T) {
		t.Parallel()
		artist, album, ok := parseAlbumID(albumID("Various Artists", "Warp10"))
		if !ok || artist != "Various Artists" || album != "Warp10" {
			t.Errorf("parseAlbumID = %q, %q, %v", artist, album, ok)
		}
		// An album artist may legitimately be empty -- a record filed under no
		// artist is still a record -- but an album with no name is not one.
		if _, name, aok := parseAlbumID(albumID("", "Warp10")); !aok || name != "Warp10" {
			t.Errorf("an album with no artist = %q, %v", name, aok)
		}
		for _, id := range []string{
			"",
			"al",
			"ar-QQ",
			"al-!!!!",
			// Valid base64 with no separator in it, so there are not two halves.
			"al-" + idEncoding.EncodeToString([]byte("Warp10")),
			// A separator and nothing after it.
			"al-" + idEncoding.EncodeToString([]byte("Various Artists"+idSeparator)),
		} {
			if _, _, aok := parseAlbumID(id); aok {
				t.Errorf("parseAlbumID(%q) accepted it", id)
			}
		}
	})

	t.Run("directory", func(t *testing.T) {
		t.Parallel()
		if got, ok := parseDirID(dirID("music/Homogenic")); !ok || got != "music/Homogenic" {
			t.Errorf("parseDirID = %q, %v", got, ok)
		}
		// The root is a directory, and its id is the prefix and nothing else.
		if got, ok := parseDirID(dirID("")); !ok || got != "" {
			t.Errorf("parseDirID for the root = %q, %v", got, ok)
		}
		for _, id := range []string{
			"",
			"d",
			"ar-QQ",
			"d-!!!!",
			// Decodes, but not to something this server would store: the path
			// came off a query string, and a listing is not where to find out.
			dirID("../etc"),
			dirID("/music"),
			dirID("music//x"),
		} {
			if _, ok := parseDirID(id); ok {
				t.Errorf("parseDirID(%q) accepted it", id)
			}
		}
	})
}

// TestInitialOf covers the jump-bar heading, including the name no query can
// return: Artists excludes an empty album artist and a folder always has a base
// name, so the empty case is only reachable from here.
func TestInitialOf(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"Björk":          "B",
		"björk":          "B",
		"the Beatles":    "T",
		"Ólafur Arnalds": "Ó",
		"3 Doors Down":   "#",
		"...And Justice": "#",
		"":               "#",
	}
	for name, want := range tests {
		if got := initialOf(name); got != want {
			t.Errorf("initialOf(%q) = %q, want %q", name, got, want)
		}
	}
}
