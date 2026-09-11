package subsonic_test

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/subsonic"
)

// breaking is a library and a file tree that fail one named call and pass the
// rest through to the real thing.
//
// It is the fault injection internal/db/sqlutil needed for the same reason: a
// working backend does not fail a listing halfway on request, and the branch
// where it does is the difference between "the server is broken" and "that does
// not exist" -- which is the difference between a client retrying and a client
// deciding a track is gone.
//
// Closing the database would reach the first call of each handler and hide the
// rest, so it is a name rather than a broken connection.
type breaking struct {
	music db.Music
	tree  subsonic.Tree
	art   subsonic.Art
	fail  string
}

var errOnFire = errors.New("the backend is on fire")

func (b breaking) err(name string) error {
	if b.fail == name {
		return errOnFire
	}
	return nil
}

func (b breaking) Artists(ctx context.Context, owner string) ([]db.Artist, error) {
	if err := b.err("Artists"); err != nil {
		return nil, err
	}
	return b.music.Artists(ctx, owner)
}

func (b breaking) Albums(ctx context.Context, owner, artist string) ([]db.Album, error) {
	if err := b.err("Albums"); err != nil {
		return nil, err
	}
	return b.music.Albums(ctx, owner, artist)
}

func (b breaking) Tracks(ctx context.Context, owner, artist, album string) ([]db.Track, error) {
	if err := b.err("Tracks"); err != nil {
		return nil, err
	}
	return b.music.Tracks(ctx, owner, artist, album)
}

func (b breaking) TracksIn(ctx context.Context, owner, dir string) ([]db.Track, error) {
	if err := b.err("TracksIn"); err != nil {
		return nil, err
	}
	return b.music.TracksIn(ctx, owner, dir)
}

func (b breaking) TrackByFile(ctx context.Context, owner string, fileID int64) (db.Track, error) {
	if err := b.err("TrackByFile"); err != nil {
		return db.Track{}, err
	}
	return b.music.TrackByFile(ctx, owner, fileID)
}

func (b breaking) AlbumList(ctx context.Context, owner string, f db.AlbumFilter) ([]db.Album, error) {
	if err := b.err("AlbumList"); err != nil {
		return nil, err
	}
	return b.music.AlbumList(ctx, owner, f)
}

func (b breaking) TrackList(ctx context.Context, owner string, f db.TrackFilter) ([]db.Track, error) {
	if err := b.err("TrackList"); err != nil {
		return nil, err
	}
	return b.music.TrackList(ctx, owner, f)
}

func (b breaking) Genres(ctx context.Context, owner string) ([]db.Genre, error) {
	if err := b.err("Genres"); err != nil {
		return nil, err
	}
	return b.music.Genres(ctx, owner)
}

func (b breaking) Search(ctx context.Context, owner string, f db.SearchFilter) (db.SearchResult, error) {
	if err := b.err("Search"); err != nil {
		return db.SearchResult{}, err
	}
	return b.music.Search(ctx, owner, f)
}

func (b breaking) Stat(ctx context.Context, owner, path string) (db.File, error) {
	if err := b.err("Stat"); err != nil {
		return db.File{}, err
	}
	return b.tree.Stat(ctx, owner, path)
}

func (b breaking) List(ctx context.Context, owner, dir string) ([]db.File, error) {
	if err := b.err("List"); err != nil {
		return nil, err
	}
	return b.tree.List(ctx, owner, dir)
}

func (b breaking) OpenFile(ctx context.Context, f db.File) (io.ReadSeekCloser, error) {
	if err := b.err("OpenFile"); err != nil {
		return nil, err
	}
	return b.tree.OpenFile(ctx, f)
}

func (b breaking) Cover(ctx context.Context, owner, dir string, px int) (io.ReadCloser, int64, error) {
	if err := b.err("Cover"); err != nil {
		return nil, 0, err
	}
	return b.art.Cover(ctx, owner, dir, px)
}

// TestABrokenBackendIsNotANotFound walks every call a browse or stream request
// makes and breaks it, one at a time.
//
// The answer has to be the generic code and never 70. A client told "no such
// album" caches that: it removes the row, and a database that was merely
// unreachable for a second has cost the user their library.
func TestABrokenBackendIsNotANotFound(t *testing.T) {
	t.Parallel()

	// The id of the track is only known once it is stored, so it is a function
	// of the row rather than a constant.
	fixed := func(id string) func(db.File) string { return func(db.File) string { return id } }
	theTrack := func(f db.File) string { return songIDOf(f.ID) }

	tests := []struct {
		name   string
		call   string
		method string
		id     func(track db.File) string
		// extra is whatever else the endpoint needs before it will reach the
		// call being broken.
		extra  []string
		binary bool
	}{
		{name: "listing artists", call: "Artists", method: "getArtists"},
		{name: "listing an artist's albums", call: "Albums", method: "getArtist", id: fixed(artistIDOf("Björk"))},
		{name: "finding an album", call: "Albums", method: "getAlbum", id: fixed(albumIDOf("Björk", "Homogenic"))},
		{
			name: "listing an album's tracks", call: "Tracks",
			method: "getAlbum", id: fixed(albumIDOf("Björk", "Homogenic")),
		},
		{name: "reading one track", call: "TrackByFile", method: "getSong", id: theTrack},
		{name: "listing the root", call: "List", method: "getIndexes"},
		{name: "listing the tracks at the root", call: "TracksIn", method: "getIndexes"},
		{name: "statting a directory", call: "Stat", method: "getMusicDirectory", id: fixed(dirIDOf("music"))},
		{name: "listing a directory", call: "List", method: "getMusicDirectory", id: fixed(dirIDOf("music"))},
		{
			name: "listing the tracks in a directory", call: "TracksIn",
			method: "getMusicDirectory", id: fixed(dirIDOf("music")),
		},
		{name: "reading the track to stream", call: "TrackByFile", method: "stream", id: theTrack, binary: true},
		{name: "opening the bytes", call: "OpenFile", method: "stream", id: theTrack, binary: true},
		{name: "listing albums", call: "AlbumList", method: "getAlbumList2", extra: []string{"type", "newest"}},
		{name: "listing albums for an old client", call: "AlbumList", method: "getAlbumList", extra: []string{"type", "newest"}},
		{name: "searching", call: "Search", method: "search3"},
		{name: "searching for an old client", call: "Search", method: "search2"},
		{name: "listing genres", call: "Genres", method: "getGenres"},
		{name: "listing a genre", call: "TrackList", method: "getSongsByGenre", extra: []string{"genre", "Rock"}},
		{name: "shuffling", call: "TrackList", method: "getRandomSongs"},
		{
			name: "finding the folder a cover is in", call: "Tracks",
			method: "getCoverArt", id: fixed(albumIDOf("Björk", "Homogenic")), binary: true,
		},
		{
			name: "reading the cover", call: "Cover",
			method: "getCoverArt", id: fixed(albumIDOf("Björk", "Homogenic")), binary: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			l := newLibrary(t)
			track := l.add(t, "music/Homogenic/01 Hunter.flac", song("Björk", "Homogenic", "Hunter", 1))
			// A cover beside it, so the cases that break reading one get that
			// far: without a picture the answer is "there is none" and the call
			// under test is never made.
			l.addImage(t, "music/Homogenic/cover.jpg", 100, 100)

			b := breaking{music: l.meta, tree: l.files, art: l.art, fail: tt.call}
			h := subsonic.Handler(prefix, serverVersion, l.verifier, b, b, b)

			params := append([]string{"f", "json"}, tt.extra...)
			if tt.id != nil {
				params = append(params, "id", tt.id(track))
			}
			rec := get(t, h, tt.method, query(params...))
			if tt.binary {
				assertXMLError(t, rec, 0)
				return
			}
			if code := errorCode(t, rec); code != 0 {
				t.Errorf("code = %v, want the generic one and never 70", code)
			}
		})
	}
}

// TestTheRootIsNotStatted is the other half of statting a directory: the root
// has no row to stat, so it must not be looked for.
func TestTheRootIsNotStatted(t *testing.T) {
	t.Parallel()
	l := newLibrary(t)
	l.add(t, "loose.flac", song("Loose", "Singles", "Loose", 1))

	b := breaking{music: l.meta, tree: l.files, art: l.art, fail: "Stat"}
	h := subsonic.Handler(prefix, serverVersion, l.verifier, b, b, b)

	env := response(t, get(t, h, "getMusicDirectory", query("f", "json", "id", dirIDOf(""))))
	if got, _ := env["status"].(string); got != "ok" {
		t.Fatalf("the root = %v, want ok: %v", env["status"], env["error"])
	}
	dir, _ := env["directory"].(map[string]any)
	if got, _ := dir["name"].(string); got != "Music" {
		t.Errorf("name = %v, want the name getMusicFolders answers with", dir["name"])
	}
	// The root has nothing above it, so a client is not offered a way up.
	if _, present := dir["parent"]; present {
		t.Errorf("the root reports a parent: %v", dir)
	}
	if _, ok := dir["child"].([]any); !ok {
		t.Errorf("child = %#v, want the track at the top level", dir["child"])
	}
}
