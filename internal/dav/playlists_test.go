package dav_test

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strconv"
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
	return withUser(dav.Playlists(playlistsPrefix, "/files/", lists), "edu"), lists, meta, ids
}

func mkPlaylist(t *testing.T, lists *music.Service, name string, fileIDs ...int64) db.Playlist {
	t.Helper()
	p, err := lists.Create(t.Context(), "edu", name, fileIDs)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTheMountListsEveryPlaylist(t *testing.T) {
	t.Parallel()
	h, lists, _, ids := playlistServer(t)
	mixID := mkPlaylist(t, lists, "Mix", ids["loose.flac"]).ID
	mkPlaylist(t, lists, "Night drive")

	rec := do(t, h, "PROPFIND", "/playlists/", "", "Depth", "1")
	wantHrefs(t, hrefs(t, rec), "/playlists/", "/playlists/Mix.m3u8", "/playlists/Night drive.m3u8")

	props := propsOf(t, rec.Body.String())
	mix := props["/playlists/Mix.m3u8"]
	if mix["getcontenttype"] != "audio/x-mpegurl" {
		t.Errorf("getcontenttype = %q", mix["getcontenttype"])
	}
	// The length this listing promises is the length of the file the other
	// half serves, which is the same generator (#279): a client that reads
	// one and fetches the other must not find them disagreeing.
	pl, tracks, err := lists.Playlist(t.Context(), "edu", mixID)
	if err != nil {
		t.Fatal(err)
	}
	if want := strconv.Itoa(len(music.M3U8(pl, tracks, "/files/"))); mix["getcontentlength"] != want {
		t.Errorf("getcontentlength = %q, the generated file is %s bytes", mix["getcontentlength"], want)
	}
	if mix["getetag"] == "" {
		t.Error("no validator on a generated file")
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
	before := propsOf(t, do(t, h, "PROPFIND", "/playlists/", "", "Depth", "1").Body.String())

	if err := meta.MoveFile(t.Context(), "edu", "loose.flac", "renamed.flac"); err != nil {
		t.Fatal(err)
	}
	if err := meta.DeleteFile(t.Context(), "edu", "music/Homogenic/01 Hunter.flac"); err != nil {
		t.Fatal(err)
	}

	after := propsOf(t, do(t, h, "PROPFIND", "/playlists/", "", "Depth", "1").Body.String())
	b, a := before["/playlists/Mix.m3u8"], after["/playlists/Mix.m3u8"]
	if b["getetag"] == a["getetag"] {
		t.Error("the ETag did not move with the file")
	}
	if b["getcontentlength"] == a["getcontentlength"] {
		t.Error("the file is the same length after losing a track")
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
	// Still there, and still empty, after everything that was refused.
	wantHrefs(t, hrefs(t, do(t, h, "PROPFIND", "/playlists/", "", "Depth", "1")),
		"/playlists/", "/playlists/Mix.m3u8")
}

func TestTheMountRefusesWhatTheOtherDoes(t *testing.T) {
	t.Parallel()
	h, lists, _, _ := playlistServer(t)
	mkPlaylist(t, lists, "Mix")

	if rec := do(t, h, "PROPFIND", "/playlists/", ""); rec.Code != http.StatusForbidden {
		t.Errorf("PROPFIND with no Depth = %d, want the 403 /files/ answers", rec.Code)
	}
	if rec := do(t, h, "PROPFIND", "/playlists/Nothing.m3u8", "", "Depth", "0"); rec.Code != http.StatusNotFound {
		t.Errorf("PROPFIND of a playlist that is not there = %d", rec.Code)
	}
	if rec := do(t, h, "PROPFIND", "/playlists/Mix.m3u8", "", "Depth", "0"); rec.Code != http.StatusMultiStatus {
		t.Errorf("PROPFIND of one file = %d", rec.Code)
	}
	// The bytes are the browser half's, and this half says so (#279) while
	// still advertising the method: see Allow above.
	if rec := do(t, h, http.MethodGet, "/playlists/Mix.m3u8", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET straight to the mount = %d, want 405", rec.Code)
	}

	bare := dav.Playlists(playlistsPrefix, "/files/", lists)
	if rec := do(t, bare, "PROPFIND", "/playlists/", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("with nobody authenticated = %d, want 401", rec.Code)
	}
}

// TestOwnersSeeTheirOwn: the mount answers for whoever authenticated.
func TestOwnersSeeTheirOwn(t *testing.T) {
	t.Parallel()
	_, lists, _, _ := playlistServer(t)
	mkPlaylist(t, lists, "Mix")

	theirs := withUser(dav.Playlists(playlistsPrefix, "/files/", lists), "someone-else")
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
		h := withUser(dav.Playlists(playlistsPrefix, "/files/", failingSource{PlaylistSource: lists, fail: call}), "edu")
		if rec := do(t, h, "PROPFIND", "/playlists/", "", "Depth", "1"); rec.Code != http.StatusInternalServerError {
			t.Errorf("PROPFIND with %s broken = %d, want 500", call, rec.Code)
		}
	}

	// Listed and then gone before it was read: a 404, not a 500.
	gone := withUser(dav.Playlists(playlistsPrefix, "/files/", failingSource{PlaylistSource: lists, delete: true}), "edu")
	if rec := do(t, gone, "PROPFIND", "/playlists/Mix.m3u8", "", "Depth", "0"); rec.Code != http.StatusNotFound {
		t.Errorf("PROPFIND of a playlist deleted mid-request = %d, want 404", rec.Code)
	}
}
