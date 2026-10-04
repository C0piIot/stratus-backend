package dav_test

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/dav"
	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/db/sqlite"
)

const musicPrefix = "/music/"

// musicServer is the mount over a real database with those tracks in it.
func musicServer(t *testing.T, tracks ...[3]string) (http.Handler, *sqlite.Store) {
	t.Helper()
	meta, err := sqlite.New(t.Context(), filepath.Join(t.TempDir(), "stratus.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = meta.Close() })
	if err := meta.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}

	for i, tr := range tracks {
		path, artist, album := tr[0], tr[1], tr[2]
		f, err := meta.PutFile(t.Context(), db.File{
			OwnerID: "edu", Path: path, BlobKey: "k" + path, Size: 10,
			MTime: time.Now(), ETag: `"e"`, MIMEType: "audio/flac",
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := meta.PutMedia(t.Context(), db.Media{
			FileID: f.ID, Kind: db.KindAudio, IndexedAt: time.Now(), Version: 1,
			AlbumArtist: artist, Artist: artist, Album: album, Title: path,
			TrackNo: i + 1, DurationMS: 1000,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return withUser(dav.Music(musicPrefix, meta), "edu"), meta
}

// TestTheLibraryIsFoldersByTag is the mount: artists, their albums, and the
// tracks of an album wherever their files were filed.
func TestTheLibraryIsFoldersByTag(t *testing.T) {
	t.Parallel()
	h, _ := musicServer(t,
		[3]string{"incoming/a.flac", "Autechre", "Amber"},
		[3]string{"sorted/Amber/b.flac", "Autechre", "Amber"},
		[3]string{"c.flac", "Björk", "Homogenic"})

	wantHrefs(t, hrefs(t, do(t, h, "PROPFIND", "/music/", "", "Depth", "1")),
		"/music/", "/music/Autechre/", "/music/Björk/")
	wantHrefs(t, hrefs(t, do(t, h, "PROPFIND", "/music/Autechre/", "", "Depth", "1")),
		"/music/Autechre/", "/music/Autechre/Amber/")
	wantHrefs(t, hrefs(t, do(t, h, "PROPFIND", "/music/Autechre/Amber/", "", "Depth", "1")),
		"/music/Autechre/Amber/", "/music/Autechre/Amber/a.flac", "/music/Autechre/Amber/b.flac")
}

// TestATagWithASlashIsOneCollection: a collection cannot hold a slash, so the
// name is generated rather than escaped -- and the page links to the same one.
func TestATagWithASlashIsOneCollection(t *testing.T) {
	t.Parallel()
	h, _ := musicServer(t, [3]string{"x.flac", "AC/DC", "High Voltage"})

	wantHrefs(t, hrefs(t, do(t, h, "PROPFIND", "/music/", "", "Depth", "1")),
		"/music/", "/music/AC_DC/")
	if rec := do(t, h, "PROPFIND", "/music/AC/DC/", "", "Depth", "1"); rec.Code != http.StatusNotFound {
		t.Errorf("the tag itself answered as a path: %d", rec.Code)
	}
}

func TestTheMusicMountIsReadOnly(t *testing.T) {
	t.Parallel()
	h, _ := musicServer(t, [3]string{"a.flac", "A", "B"})

	opts := do(t, h, http.MethodOptions, "/music/", "")
	if opts.Code != http.StatusOK || opts.Header().Get("DAV") != "1" {
		t.Errorf("OPTIONS = %d, DAV %q, want 200 and class 1", opts.Code, opts.Header().Get("DAV"))
	}
	if got := opts.Header().Get("Allow"); got != "OPTIONS, GET, HEAD, PROPFIND" {
		t.Errorf("Allow = %q", got)
	}
	for _, method := range []string{"PUT", "DELETE", "MKCOL", "MOVE", "COPY", "PROPPATCH", "LOCK", "POST"} {
		if rec := do(t, h, method, "/music/A/B/a.flac", "x"); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s = %d, want 405", method, rec.Code)
		}
	}
	// The bytes are the browser half's, and this half says so while still
	// advertising the method (#279).
	if rec := do(t, h, http.MethodGet, "/music/A/B/a.flac", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET straight to the mount = %d, want 405", rec.Code)
	}
	if rec := do(t, h, "PROPFIND", "/music/", ""); rec.Code != http.StatusForbidden {
		t.Errorf("PROPFIND with no Depth = %d, want 403", rec.Code)
	}
}

func TestWhatIsNotInTheLibraryIsNotFound(t *testing.T) {
	t.Parallel()
	h, meta := musicServer(t, [3]string{"a.flac", "A", "B"})

	for _, target := range []string{"/music/Nobody/", "/music/A/Nothing/", "/music/A/B/b.flac", "/music/A/B/a.flac/x"} {
		if rec := do(t, h, "PROPFIND", target, "", "Depth", "0"); rec.Code != http.StatusNotFound {
			t.Errorf("PROPFIND %s = %d, want 404", target, rec.Code)
		}
	}
	if rec := do(t, dav.Music(musicPrefix, meta), "PROPFIND", "/music/", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("with nobody authenticated = %d, want 401", rec.Code)
	}
	// Somebody else's library is empty, not somebody else's.
	theirs := withUser(dav.Music(musicPrefix, meta), "someone-else")
	wantHrefs(t, hrefs(t, do(t, theirs, "PROPFIND", "/music/", "", "Depth", "1")), "/music/")
}

// failingLibrary breaks one of the three reads the mount makes.
type failingLibrary struct {
	db.Music
	fail string
}

func (f failingLibrary) Artists(ctx context.Context, owner string) ([]db.Artist, error) {
	if f.fail == "Artists" {
		return nil, errors.New("the database is on fire")
	}
	return f.Music.Artists(ctx, owner)
}

func (f failingLibrary) Albums(ctx context.Context, owner, artist string) ([]db.Album, error) {
	if f.fail == "Albums" {
		return nil, errors.New("the database is on fire")
	}
	return f.Music.Albums(ctx, owner, artist)
}

func (f failingLibrary) Tracks(ctx context.Context, owner, artist, album string) ([]db.Track, error) {
	if f.fail == "Tracks" {
		return nil, errors.New("the database is on fire")
	}
	return f.Music.Tracks(ctx, owner, artist, album)
}

// TestABrokenLibraryIsNotAnEmptyOne: x/net answers 404 for any error opening a
// file, so the request is resolved before it sees it.
func TestABrokenLibraryIsNotAnEmptyOne(t *testing.T) {
	t.Parallel()
	_, meta := musicServer(t, [3]string{"a.flac", "A", "B"})

	for _, call := range []string{"Artists", "Albums", "Tracks"} {
		h := withUser(dav.Music(musicPrefix, failingLibrary{Music: meta, fail: call}), "edu")
		if rec := do(t, h, "PROPFIND", "/music/A/B/", "", "Depth", "1"); rec.Code != http.StatusInternalServerError {
			t.Errorf("PROPFIND with %s broken = %d, want 500", call, rec.Code)
		}
	}
}
