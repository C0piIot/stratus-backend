package dav_test

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/dav"
	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/db/sqlite"
	"github.com/C0piIot/stratus-backend/internal/music"
)

const playlistsPrefix = "/playlists/"

// playlistServer is the mount over a real database and the real playlist
// service, with the named tracks in it.
func playlistServer(t *testing.T) (http.Handler, *music.Service, *sqlite.Store, map[string]int64) {
	t.Helper()
	meta, err := sqlite.New(t.Context(), filepath.Join(t.TempDir(), "stratus.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = meta.Close() })
	if err := meta.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}

	ids := map[string]int64{}
	for _, tr := range []struct{ path, artist, title string }{
		{"music/Homogenic/01 Hunter.flac", "Björk", "Hunter"},
		{"music/Tri Repetae/01 Rotar & Stud?.flac", "Autechre", "Rotar"},
		{"loose.flac", "", ""},
	} {
		f, err := meta.PutFile(t.Context(), db.File{
			OwnerID: "edu", Path: tr.path, BlobKey: "k" + tr.path, Size: 10,
			MTime: time.Now(), ETag: `"e"`, MIMEType: "audio/flac",
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := meta.PutMedia(t.Context(), db.Media{
			FileID: f.ID, Kind: db.KindAudio, IndexedAt: time.Now(), Version: 1,
			Artist: tr.artist, Title: tr.title, DurationMS: 254_600,
		}); err != nil {
			t.Fatal(err)
		}
		ids[tr.path] = f.ID
	}

	lists := music.New(meta)
	return withUser(dav.Playlists(playlistsPrefix, "/dav/", lists), "edu"), lists, meta, ids
}

func mkPlaylist(t *testing.T, lists *music.Service, name string, fileIDs ...int64) db.Playlist {
	t.Helper()
	p, err := lists.Create(t.Context(), "edu", name, fileIDs)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestAPlaylistIsAnM3U8 is what #203 is for: a player that has never heard of
// Subsonic opens the file and finds the tracks.
func TestAPlaylistIsAnM3U8(t *testing.T) {
	t.Parallel()
	h, lists, _, ids := playlistServer(t)
	mkPlaylist(t, lists, "Night drive",
		ids["music/Tri Repetae/01 Rotar & Stud?.flac"], ids["music/Homogenic/01 Hunter.flac"], ids["loose.flac"])

	rec := do(t, h, http.MethodGet, "/playlists/Night%20drive.m3u8", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "audio/x-mpegurl" {
		t.Errorf("Content-Type = %q", got)
	}
	want := "#EXTM3U\n" +
		"#PLAYLIST:Night drive\n" +
		"#EXTINF:255,Autechre - Rotar\n" +
		// Escaped, because a player reads this line as a URL: the ampersand
		// and the question mark would otherwise end the path.
		"/dav/music/Tri%20Repetae/01%20Rotar%20&%20Stud%3F.flac\n" +
		"#EXTINF:255,Björk - Hunter\n" +
		"/dav/music/Homogenic/01%20Hunter.flac\n" +
		// No tags at all: the file name is the title.
		"#EXTINF:255,loose.flac\n" +
		"/dav/loose.flac\n"
	if got := rec.Body.String(); got != want {
		t.Errorf("body =\n%s\nwant\n%s", got, want)
	}
	if rec.Header().Get("ETag") == "" {
		t.Error("no ETag on the file")
	}
}

func TestTheMountListsEveryPlaylist(t *testing.T) {
	t.Parallel()
	h, lists, _, ids := playlistServer(t)
	mkPlaylist(t, lists, "Mix", ids["loose.flac"])
	mkPlaylist(t, lists, "Night drive")

	rec := do(t, h, "PROPFIND", "/playlists/", "", "Depth", "1")
	wantHrefs(t, hrefs(t, rec), "/playlists/", "/playlists/Mix.m3u8", "/playlists/Night drive.m3u8")

	props := propsOf(t, rec.Body.String())
	mix := props["/playlists/Mix.m3u8"]
	if mix["getcontenttype"] != "audio/x-mpegurl" {
		t.Errorf("getcontenttype = %q", mix["getcontenttype"])
	}
	body := do(t, h, http.MethodGet, "/playlists/Mix.m3u8", "")
	if mix["getcontentlength"] != strconv.Itoa(body.Body.Len()) {
		t.Errorf("getcontentlength = %q, the file is %d bytes", mix["getcontentlength"], body.Body.Len())
	}
	// The same validator from both methods, or a client comparing them decides
	// the file changed.
	if mix["getetag"] != body.Header().Get("ETag") {
		t.Errorf("PROPFIND etag %q, GET etag %q", mix["getetag"], body.Header().Get("ETag"))
	}
}

// TestNamesCannotCollide covers the collisions a mount of its own does not
// remove: between playlists, including ones a case-folding client would see as
// the same file, and names that are not one path element.
func TestNamesCannotCollide(t *testing.T) {
	t.Parallel()
	h, lists, _, _ := playlistServer(t)
	for _, name := range []string{"Mix", "mix", "Mix", "AC/DC", "  ", "..", "tab\there", `what? "now": <yes>|*`} {
		mkPlaylist(t, lists, name)
	}

	rec := do(t, h, "PROPFIND", "/playlists/", "", "Depth", "1")
	wantHrefs(t, hrefs(t, rec),
		"/playlists/",
		// The oldest keeps the name, whatever the case of the others.
		"/playlists/Mix.m3u8", "/playlists/mix (2).m3u8", "/playlists/Mix (3).m3u8",
		"/playlists/AC_DC.m3u8", "/playlists/Playlist.m3u8", "/playlists/Playlist (2).m3u8",
		"/playlists/tab_here.m3u8", "/playlists/what_ _now__ _yes___.m3u8",
	)
}

// TestAnEditShowsAtOnce: the file is generated, so there is no copy to go
// stale -- a rename of a track and a delete are in the next read.
func TestAnEditShowsAtOnce(t *testing.T) {
	t.Parallel()
	h, lists, meta, ids := playlistServer(t)
	mkPlaylist(t, lists, "Mix", ids["loose.flac"], ids["music/Homogenic/01 Hunter.flac"])
	before := do(t, h, http.MethodGet, "/playlists/Mix.m3u8", "")

	if err := meta.MoveFile(t.Context(), "edu", "loose.flac", "renamed.flac"); err != nil {
		t.Fatal(err)
	}
	if err := meta.DeleteFile(t.Context(), "edu", "music/Homogenic/01 Hunter.flac"); err != nil {
		t.Fatal(err)
	}

	after := do(t, h, http.MethodGet, "/playlists/Mix.m3u8", "")
	if body := after.Body.String(); !strings.Contains(body, "/dav/renamed.flac\n") || strings.Contains(body, "Hunter") {
		t.Errorf("after a rename and a delete =\n%s", body)
	}
	if before.Header().Get("ETag") == after.Header().Get("ETag") {
		t.Error("the ETag did not move with the file")
	}
}

// TestTheMountIsReadOnly is what the OPTIONS answer promises: class 1, so a
// client mounts it read-only, and every write refused rather than half-done.
func TestTheMountIsReadOnly(t *testing.T) {
	t.Parallel()
	h, lists, _, _ := playlistServer(t)
	mkPlaylist(t, lists, "Mix")

	opts := do(t, h, http.MethodOptions, "/playlists/", "")
	if opts.Code != http.StatusOK || opts.Header().Get("DAV") != "1" {
		t.Errorf("OPTIONS = %d, DAV %q, want 200 and class 1", opts.Code, opts.Header().Get("DAV"))
	}
	for _, method := range []string{"PUT", "DELETE", "MKCOL", "MOVE", "COPY", "PROPPATCH", "LOCK", "UNLOCK", "POST"} {
		rec := do(t, h, method, "/playlists/Mix.m3u8", "x")
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s = %d, want 405", method, rec.Code)
		}
		if got := rec.Header().Get("Allow"); got != "OPTIONS, GET, HEAD, PROPFIND" {
			t.Errorf("%s Allow = %q", method, got)
		}
	}
	if got := do(t, h, http.MethodGet, "/playlists/Mix.m3u8", "").Body.String(); got != "#EXTM3U\n#PLAYLIST:Mix\n" {
		t.Errorf("after the refused writes the file is %q", got)
	}
}

func TestTheMountRefusesWhatTheOtherDoes(t *testing.T) {
	t.Parallel()
	h, lists, _, _ := playlistServer(t)
	mkPlaylist(t, lists, "Mix")

	if rec := do(t, h, "PROPFIND", "/playlists/", ""); rec.Code != http.StatusForbidden {
		t.Errorf("PROPFIND with no Depth = %d, want the 403 /dav/ answers", rec.Code)
	}
	if rec := do(t, h, http.MethodGet, "/playlists/Nothing.m3u8", ""); rec.Code != http.StatusNotFound {
		t.Errorf("GET of a playlist that is not there = %d", rec.Code)
	}
	if rec := do(t, h, http.MethodHead, "/playlists/Mix.m3u8", ""); rec.Code != http.StatusOK {
		t.Errorf("HEAD = %d", rec.Code)
	}
	if rec := do(t, h, "PROPFIND", "/playlists/Mix.m3u8", "", "Depth", "0"); rec.Code != http.StatusMultiStatus {
		t.Errorf("PROPFIND of one file = %d", rec.Code)
	}

	bare := dav.Playlists(playlistsPrefix, "/dav/", lists)
	if rec := do(t, bare, http.MethodGet, "/playlists/", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("with nobody authenticated = %d, want 401", rec.Code)
	}
}

// TestOwnersSeeTheirOwn: the mount answers for whoever authenticated.
func TestOwnersSeeTheirOwn(t *testing.T) {
	t.Parallel()
	_, lists, _, _ := playlistServer(t)
	mkPlaylist(t, lists, "Mix")

	theirs := withUser(dav.Playlists(playlistsPrefix, "/dav/", lists), "someone-else")
	wantHrefs(t, hrefs(t, do(t, theirs, "PROPFIND", "/playlists/", "", "Depth", "1")), "/playlists/")
}

// failingSource breaks one of the two reads the mount makes.
type failingSource struct {
	dav.PlaylistSource
	fail   string
	delete bool
}

var errBroken = errors.New("the database is on fire")

func (f failingSource) Playlists(ctx context.Context, owner string) ([]db.Playlist, error) {
	if f.fail == "Playlists" {
		return nil, errBroken
	}
	return f.PlaylistSource.Playlists(ctx, owner)
}

func (f failingSource) Playlist(ctx context.Context, owner string, id int64) (db.Playlist, []db.Track, error) {
	switch {
	case f.fail == "Playlist":
		return db.Playlist{}, nil, errBroken
	case f.delete:
		return db.Playlist{}, nil, db.ErrNotFound
	}
	return f.PlaylistSource.Playlist(ctx, owner, id)
}

func TestAFailureIsNotANotFound(t *testing.T) {
	t.Parallel()
	_, lists, _, _ := playlistServer(t)
	mkPlaylist(t, lists, "Mix")

	for _, call := range []string{"Playlists", "Playlist"} {
		h := withUser(dav.Playlists(playlistsPrefix, "/dav/", failingSource{PlaylistSource: lists, fail: call}), "edu")
		if rec := do(t, h, http.MethodGet, "/playlists/Mix.m3u8", ""); rec.Code != http.StatusInternalServerError {
			t.Errorf("GET with %s broken = %d, want 500", call, rec.Code)
		}
		if rec := do(t, h, "PROPFIND", "/playlists/", "", "Depth", "1"); rec.Code == http.StatusMultiStatus && call == "Playlists" {
			t.Errorf("PROPFIND with %s broken = 207", call)
		}
	}

	// Listed and then gone before it was read: a 404, not a 500.
	gone := withUser(dav.Playlists(playlistsPrefix, "/dav/", failingSource{PlaylistSource: lists, delete: true}), "edu")
	if rec := do(t, gone, http.MethodGet, "/playlists/Mix.m3u8", ""); rec.Code != http.StatusNotFound {
		t.Errorf("GET of a playlist deleted mid-request = %d, want 404", rec.Code)
	}
}

// TestANewlineCannotForgeAnEntry: every line of an .m3u8 that is not a comment
// is a URL a player will fetch, so a newline in a name must not start one.
func TestANewlineCannotForgeAnEntry(t *testing.T) {
	t.Parallel()
	h, lists, _, _ := playlistServer(t)
	mkPlaylist(t, lists, "Side A\nhttp://elsewhere/")

	rec := do(t, h, http.MethodGet, "/playlists/Side%20A_http___elsewhere_.m3u8", "")
	if got := rec.Body.String(); got != "#EXTM3U\n#PLAYLIST:Side A http://elsewhere/\n" {
		t.Errorf("body = %q", got)
	}
}
