package web_test

import (
	htmlstd "html"
	"net/http"
	"strings"
	"testing"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/db/dbtest"
	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/music"
)

// mkPlaylist stores a playlist with those files in it, through the service
// that owns what an edit means.
func mkPlaylist(t *testing.T, meta db.Store, name string, fileIDs ...int64) db.Playlist {
	t.Helper()
	pl, err := music.New(meta).Create(t.Context(), username, name, fileIDs)
	if err != nil {
		t.Fatal(err)
	}
	return pl
}

func TestPlaylistsNeedASession(t *testing.T) {
	t.Parallel()
	h := newHandler(t, nil)
	for _, target := range []string{"/playlists/", "/playlists/Mix.m3u8"} {
		rec := get(t, h, target)
		if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/login") {
			t.Errorf("%s with no session = %d", target, rec.Code)
		}
	}
}

// TestThePlaylistsAreAPageAndAFile is the whole of the playlists' half of
// #279: the list, the file a player wants, and the page about it, at the
// addresses the mount already answered for.
func TestThePlaylistsAreAPageAndAFile(t *testing.T) {
	t.Parallel()
	h, s, meta := browserOver(t)
	cookie := signIn(t, h)
	one, two := homework(t, s, meta)
	mkPlaylist(t, meta, "Night drive", one.ID, two.ID)

	index := get(t, h, "/playlists/", cookie)
	if index.Code != http.StatusOK {
		t.Fatalf("the list = %d", index.Code)
	}
	for _, want := range []string{"Night drive", `href="/playlists/Night%20drive.m3u8?view"`, `href="/playlists/Night%20drive.m3u8"`} {
		if !strings.Contains(index.Body.String(), want) {
			t.Errorf("the list has no %q:\n%s", want, index.Body.String())
		}
	}

	file := get(t, h, "/playlists/Night%20drive.m3u8", cookie)
	if file.Code != http.StatusOK {
		t.Fatalf("the file = %d", file.Code)
	}
	if got := file.Header().Get("Content-Type"); got != "audio/x-mpegurl" {
		t.Errorf("Content-Type = %q", got)
	}
	// The same bytes internal/music generates for the mount, entries and all.
	want := music.M3U8(db.Playlist{Name: "Night drive"}, []db.Track{
		{File: one, Media: db.Media{AlbumArtist: "AC/DC", Artist: "AC/DC", Album: "High Voltage", Title: "It's a Long Way", TrackNo: 1, Year: 1976, Genre: "Rock", DurationMS: 301_000}},
		{File: two, Media: db.Media{AlbumArtist: "AC/DC", Artist: "AC/DC", Album: "High Voltage", TrackNo: 2, Year: 1976, DurationMS: 250_000}},
	}, "/files/")
	if file.Body.String() != string(want) {
		t.Errorf("the file =\n%s\nwant\n%s", file.Body.String(), want)
	}

	page := get(t, h, "/playlists/Night%20drive.m3u8?view", cookie)
	if page.Code != http.StatusOK {
		t.Fatalf("the page = %d", page.Code)
	}
	body := htmlstd.UnescapeString(page.Body.String())
	for _, want := range []string{"It's a Long Way", "5:01", `src="/files/01.flac"`, "open as .m3u8"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page has no %q:\n%s", want, body)
		}
	}
}

func TestAnEmptyListOfPlaylistsSaysSo(t *testing.T) {
	t.Parallel()
	h := newHandler(t, nil)
	if body := get(t, h, "/playlists/", signIn(t, h)).Body.String(); !strings.Contains(body, "No playlists yet") {
		t.Errorf("an empty list =\n%s", body)
	}
}

func TestAPlaylistThatIsNotThereIsNotFound(t *testing.T) {
	t.Parallel()
	h, _, meta := browserOver(t)
	cookie := signIn(t, h)
	mkPlaylist(t, meta, "Mix")

	for _, target := range []string{"/playlists/Nothing.m3u8", "/playlists/Nothing.m3u8?view", "/playlists/Mix"} {
		if rec := get(t, h, target, cookie); rec.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", target, rec.Code)
		}
	}
}

// TestABrokenIndexIsNotAnEmptyShelf: a database that fails is an error page,
// never a list with nothing in it.
func TestABrokenIndexIsNotAnEmptyShelf(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ call, target string }{
		{"Playlists", "/playlists/"},
		{"PlaylistTracks", "/playlists/Mix.m3u8"},
		{"PlaylistTracks", "/playlists/Mix.m3u8?view"},
	} {
		t.Run(tc.call+tc.target, func(t *testing.T) {
			t.Parallel()
			blobs, meta := backends(t)
			mkPlaylist(t, meta, "Mix")
			broken := dbtest.FailOn(t, meta, tc.call)
			h := handlerOverIndex(t, files.New(blobs, broken), blobs, broken)
			if rec := get(t, h, tc.target, signIn(t, h)); rec.Code != http.StatusInternalServerError {
				t.Errorf("%s with %s broken = %d, want 500", tc.target, tc.call, rec.Code)
			}
		})
	}
}
