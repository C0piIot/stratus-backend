package music_test

import (
	"strings"
	"testing"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/music"
)

// TestAPlaylistIsAnM3U8 is what #203 is for: a player that has never heard of
// Subsonic opens the file and finds the tracks. Here rather than in an adapter
// because both of them serve this now (#279) and neither generates it.
func TestAPlaylistIsAnM3U8(t *testing.T) {
	t.Parallel()
	tracks := []db.Track{
		{
			File:  db.File{ID: 1, Path: "music/Tri Repetae/01 Rotar & Stud?.flac"},
			Media: db.Media{Artist: "Autechre", Title: "Rotar", DurationMS: 254_600},
		},
		{
			File:  db.File{ID: 2, Path: "music/Homogenic/01 Hunter.flac"},
			Media: db.Media{Artist: "Björk", Title: "Hunter", DurationMS: 254_600},
		},
		{File: db.File{ID: 3, Path: "loose.flac"}, Media: db.Media{DurationMS: 254_600}},
	}

	want := "#EXTM3U\n" +
		"#PLAYLIST:Night drive\n" +
		"#EXTINF:255,Autechre - Rotar\n" +
		// Escaped, because a player reads this line as a URL: the ampersand
		// and the question mark would otherwise end the path.
		"/files/music/Tri%20Repetae/01%20Rotar%20&%20Stud%3F.flac\n" +
		"#EXTINF:255,Björk - Hunter\n" +
		"/files/music/Homogenic/01%20Hunter.flac\n" +
		// No tags at all: the file name is the title.
		"#EXTINF:255,loose.flac\n" +
		"/files/loose.flac\n"
	got := string(music.M3U8(db.Playlist{Name: "Night drive"}, tracks, "/files/"))
	if got != want {
		t.Errorf("file =\n%s\nwant\n%s", got, want)
	}
}

// TestANewlineCannotForgeAnEntry: every line of an .m3u8 that is not a comment
// is a URL a player will fetch, so a newline in a name must not start one.
func TestANewlineCannotForgeAnEntry(t *testing.T) {
	t.Parallel()
	got := string(music.M3U8(db.Playlist{Name: "Side A\nhttp://elsewhere/"}, nil, "/files/"))
	if got != "#EXTM3U\n#PLAYLIST:Side A http://elsewhere/\n" {
		t.Errorf("file = %q", got)
	}

	title := []db.Track{{
		File:  db.File{Path: "a.flac"},
		Media: db.Media{Title: "One\nhttp://elsewhere/", DurationMS: 1000},
	}}
	if got := string(music.M3U8(db.Playlist{Name: "Mix"}, title, "/files/")); strings.Count(got, "\n") != 4 {
		t.Errorf("a newline in a title made another line:\n%s", got)
	}
}

// TestPlaylistNamesCannotCollide covers the collisions a mount of its own does
// not remove: between playlists, including ones a case-folding client would
// see as the same file, and names that are not one path element.
func TestPlaylistNamesCannotCollide(t *testing.T) {
	t.Parallel()
	var lists []db.Playlist
	for i, name := range []string{"Mix", "mix", "Mix", "AC/DC", "  ", "..", "tab\there"} {
		lists = append(lists, db.Playlist{ID: int64(i + 1), Name: name})
	}

	named, order := music.PlaylistNames(lists)
	if len(named) != len(lists) {
		t.Fatalf("%d playlists got %d names: %v", len(lists), len(named), order)
	}
	for _, want := range []string{"Mix.m3u8", "mix (2).m3u8", "Mix (3).m3u8", "AC_DC.m3u8", "Playlist.m3u8"} {
		if _, ok := named[want]; !ok {
			t.Errorf("no playlist is called %q; the names are %v", want, order)
		}
	}
	// The oldest keeps the plain name, whatever anything is renamed to later.
	if named["Mix.m3u8"].ID != 1 {
		t.Errorf("Mix.m3u8 is playlist %d", named["Mix.m3u8"].ID)
	}
	for _, name := range order {
		if strings.ContainsAny(name, `/\<>:"|?*`) || strings.Contains(name, "\t") {
			t.Errorf("%q is not one path element", name)
		}
	}
}
