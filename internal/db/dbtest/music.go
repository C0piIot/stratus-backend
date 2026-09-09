package dbtest

import (
	"errors"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// RunMusic executes the library cases against the repository built by newRepo.
//
// It takes a db.Repo because a track is a file and its metadata: the cases have
// to write both halves before there is anything to browse.
func RunMusic(t *testing.T, newRepo func(t *testing.T) db.Repo) {
	t.Helper()

	cases := []struct {
		name string
		fn   func(t *testing.T, s db.Repo)
	}{
		{"tracks of one album group into one album", musicGroupsAlbums},
		{"a compilation stays one album", musicCompilation},
		{"an artist counts its albums", musicArtistsCountAlbums},
		{"albums can be listed for one artist or for all", musicAlbumsByArtist},
		{"tracks come back in disc then track order", musicTrackOrder},
		{"a folder lists the audio directly inside it", musicTracksInDir},
		{"a track is a file and its metadata", musicTrackByFile},
		{"a missing track is ErrNotFound", musicTrackMissing},
		{"owners do not see each other's music", musicOwnersAreSeparate},
		{"what is not audio is not music", musicIgnoresOtherKinds},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.fn(t, newRepo(t))
		})
	}
}

func musicGroupsAlbums(t *testing.T, s db.Repo) {
	track(t, s, owner, "music/a.flac", song("Boards of Canada", "Boards of Canada", "Geogaddi", "Dandelion", 1))
	track(t, s, owner, "music/b.flac", song("Boards of Canada", "Boards of Canada", "Geogaddi", "Sunshine Recorder", 2))

	albums, err := s.Albums(t.Context(), owner, "")
	if err != nil {
		t.Fatalf("Albums: %v", err)
	}
	if len(albums) != 1 {
		t.Fatalf("Albums returned %d, want the two tracks grouped into one", len(albums))
	}
	if albums[0].SongCount != 2 {
		t.Errorf("SongCount = %d, want 2", albums[0].SongCount)
	}
	// song() gives every track the same duration, so the sum is the assertion
	// that the column is summed rather than taken from one row.
	if albums[0].DurationMS != 2*254_000 {
		t.Errorf("DurationMS = %d, want the tracks added up", albums[0].DurationMS)
	}
	if albums[0].Created.IsZero() {
		t.Error("Created is zero, and the Subsonic schema requires it on every album")
	}
}

// musicCompilation is why album_artist is a column of its own. Filed under the
// track artist, a record by several artists becomes one album per track, which
// is the first thing anyone notices in a client.
func musicCompilation(t *testing.T, s db.Repo) {
	track(t, s, owner, "music/1.flac", song("Various Artists", "Aphex Twin", "Warp10", "Xtal", 1))
	track(t, s, owner, "music/2.flac", song("Various Artists", "Autechre", "Warp10", "Basscadet", 2))

	albums, err := s.Albums(t.Context(), owner, "")
	if err != nil {
		t.Fatalf("Albums: %v", err)
	}
	if len(albums) != 1 {
		t.Fatalf("Albums returned %d, want one: the tracks share an album artist", len(albums))
	}
	if albums[0].Artist != "Various Artists" {
		t.Errorf("Artist = %q, want the album artist and not a track's", albums[0].Artist)
	}

	// And the tracks keep their own artists, which is the other half of the
	// point: the album groups them without flattening them.
	tracks, err := s.Tracks(t.Context(), owner, "Various Artists", "Warp10")
	if err != nil {
		t.Fatalf("Tracks: %v", err)
	}
	if len(tracks) != 2 || tracks[0].Media.Artist == tracks[1].Media.Artist {
		t.Errorf("the tracks lost their own artists: %+v", tracks)
	}
}

func musicArtistsCountAlbums(t *testing.T, s db.Repo) {
	track(t, s, owner, "music/1.flac", song("Björk", "Björk", "Homogenic", "Hunter", 1))
	track(t, s, owner, "music/2.flac", song("Björk", "Björk", "Vespertine", "Cocoon", 1))
	track(t, s, owner, "music/3.flac", song("Björk", "Björk", "Vespertine", "Pagan Poetry", 2))

	artists, err := s.Artists(t.Context(), owner)
	if err != nil {
		t.Fatalf("Artists: %v", err)
	}
	if len(artists) != 1 {
		t.Fatalf("Artists returned %d, want 1", len(artists))
	}
	if artists[0].AlbumCount != 2 {
		t.Errorf("AlbumCount = %d, want 2: three tracks across two albums", artists[0].AlbumCount)
	}
}

func musicAlbumsByArtist(t *testing.T, s db.Repo) {
	track(t, s, owner, "music/1.flac", song("Björk", "Björk", "Homogenic", "Hunter", 1))
	track(t, s, owner, "music/2.flac", song("Aphex Twin", "Aphex Twin", "Drukqs", "Jynweythek", 1))

	all, err := s.Albums(t.Context(), owner, "")
	if err != nil {
		t.Fatalf("Albums: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("Albums with no artist returned %d, want every album", len(all))
	}
	// Ordered by artist, so the alphabetically first comes first.
	if len(all) == 2 && all[0].Artist != "Aphex Twin" {
		t.Errorf("Albums are not ordered by artist: %+v", all)
	}

	one, err := s.Albums(t.Context(), owner, "Björk")
	if err != nil {
		t.Fatalf("Albums: %v", err)
	}
	if len(one) != 1 || one[0].Name != "Homogenic" {
		t.Errorf("Albums for one artist returned %+v", one)
	}
}

// musicTrackOrder pins the order the port promises, including the last
// tie-break: an album whose tags carry no track numbers still has to come back
// the same way twice.
func musicTrackOrder(t *testing.T, s db.Repo) {
	second := song("A", "A", "Album", "Second", 2)
	first := song("A", "A", "Album", "First", 1)
	onDiscTwo := song("A", "A", "Album", "On disc two", 1)
	onDiscTwo.DiscNo = 2

	track(t, s, owner, "music/c.flac", onDiscTwo)
	track(t, s, owner, "music/b.flac", second)
	track(t, s, owner, "music/a.flac", first)

	tracks, err := s.Tracks(t.Context(), owner, "A", "Album")
	if err != nil {
		t.Fatalf("Tracks: %v", err)
	}
	if len(tracks) != 3 {
		t.Fatalf("Tracks returned %d, want 3", len(tracks))
	}
	got := []string{tracks[0].Media.Title, tracks[1].Media.Title, tracks[2].Media.Title}
	want := []string{"First", "Second", "On disc two"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Tracks = %v, want %v", got, want)
			break
		}
	}
}

// musicTracksInDir is the folder view, which exists because half the clients
// browse by folder rather than by tag. What it must not do is recurse or
// answer with anything that is not a playable track.
func musicTracksInDir(t *testing.T, s db.Repo) {
	track(t, s, owner, "music/Homogenic/02 Joga.flac", song("Björk", "Björk", "Homogenic", "Joga", 2))
	track(t, s, owner, "music/Homogenic/01 Hunter.flac", song("Björk", "Björk", "Homogenic", "Hunter", 1))
	// One level deeper, so a listing that recursed would be caught.
	track(t, s, owner, "music/Homogenic/extras/demo.flac", song("Björk", "Björk", "Homogenic", "Demo", 1))
	// A photo in the same folder, which a music client has no way to play.
	cover := song("Björk", "Björk", "Homogenic", "Cover", 1)
	cover.Kind = db.KindImage
	track(t, s, owner, "music/Homogenic/cover.jpg", cover)
	// And a file the indexer has not reached, which is not in the library yet.
	put(t, s, file("music/Homogenic/03 Unravel.flac"))

	tracks, err := s.TracksIn(t.Context(), owner, "music/Homogenic")
	if err != nil {
		t.Fatalf("TracksIn: %v", err)
	}
	got := make([]string, len(tracks))
	for i, tr := range tracks {
		got[i] = tr.Media.Title
	}
	// Path order, which is what a folder view means: the numbers in the file
	// names are the order somebody chose, and they are all a folder has.
	if len(got) != 2 || got[0] != "Hunter" || got[1] != "Joga" {
		t.Errorf("TracksIn = %v, want the two indexed tracks in path order", got)
	}

	// The root is a directory too, and this library has nothing in it.
	if root, err := s.TracksIn(t.Context(), owner, ""); err != nil || len(root) != 0 {
		t.Errorf("TracksIn at the root = %+v, %v", root, err)
	}
}

func musicTrackByFile(t *testing.T, s db.Repo) {
	f := track(t, s, owner, "music/a.flac", song("Björk", "Björk", "Homogenic", "Hunter", 1))

	got, err := s.TrackByFile(t.Context(), owner, f.ID)
	if err != nil {
		t.Fatalf("TrackByFile: %v", err)
	}
	// Both halves, because either one alone cannot be played: the tags say what
	// it is and the row says where the bytes are.
	if got.File.BlobKey != f.BlobKey || got.File.Size != f.Size {
		t.Errorf("the file half is wrong: %+v", got.File)
	}
	if got.Media.Title != "Hunter" || got.Media.DurationMS == 0 {
		t.Errorf("the metadata half is wrong: %+v", got.Media)
	}
}

func musicTrackMissing(t *testing.T, s db.Repo) {
	// A file with no metadata row is not a track either: the join drops it.
	f := put(t, s, file("music/untagged.flac"))

	if _, err := s.TrackByFile(t.Context(), owner, f.ID); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("TrackByFile for an unindexed file = %v, want ErrNotFound", err)
	}
	if _, err := s.TrackByFile(t.Context(), owner, f.ID+1000); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("TrackByFile for no such file = %v, want ErrNotFound", err)
	}
}

// musicOwnersAreSeparate is load-bearing rather than routine: media carries no
// owner column, so every query here joins files for no other reason than to
// answer "mine". If that join is ever dropped the tests must fail here.
func musicOwnersAreSeparate(t *testing.T, s db.Repo) {
	mine := file("music/mine.flac")
	theirs := file("music/theirs.flac")
	theirs.OwnerID = "someone-else"
	theirs.BlobKey = "blobs/theirs"

	for _, f := range []db.File{mine, theirs} {
		stored := put(t, s, f)
		m := song("Björk", "Björk", "Homogenic", "Hunter", 1)
		m.FileID = stored.ID
		if err := s.PutMedia(t.Context(), m); err != nil {
			t.Fatal(err)
		}
		if f.OwnerID == "someone-else" {
			theirs = stored
		}
	}

	albums, err := s.Albums(t.Context(), owner, "")
	if err != nil {
		t.Fatalf("Albums: %v", err)
	}
	if len(albums) != 1 || albums[0].SongCount != 1 {
		t.Errorf("Albums = %+v, want only the one track this owner has", albums)
	}
	if _, terr := s.TrackByFile(t.Context(), owner, theirs.ID); !errors.Is(terr, db.ErrNotFound) {
		t.Errorf("TrackByFile across owners = %v, want ErrNotFound", terr)
	}
	// Both files sit in the same folder, so a folder listing that forgot the
	// owner would return two tracks here and nothing else would notice.
	in, err := s.TracksIn(t.Context(), owner, "music")
	if err != nil {
		t.Fatalf("TracksIn: %v", err)
	}
	if len(in) != 1 || in[0].File.Path != "music/mine.flac" {
		t.Errorf("TracksIn = %+v, want only this owner's track", in)
	}
}

func musicIgnoresOtherKinds(t *testing.T, s db.Repo) {
	// A video with album tags is not a track. ffprobe fills the same columns
	// for both, so the kind is the only thing keeping them apart.
	video := song("Boards of Canada", "Boards of Canada", "Geogaddi", "Dandelion", 1)
	video.Kind = db.KindVideo
	track(t, s, owner, "videos/clip.mkv", video)

	albums, err := s.Albums(t.Context(), owner, "")
	if err != nil {
		t.Fatalf("Albums: %v", err)
	}
	if len(albums) != 0 {
		t.Errorf("Albums = %+v, want nothing: the only file is a video", albums)
	}
	artists, err := s.Artists(t.Context(), owner)
	if err != nil {
		t.Fatalf("Artists: %v", err)
	}
	if len(artists) != 0 {
		t.Errorf("Artists = %+v, want nothing", artists)
	}
}

// song builds a plausible audio row. Every track gets the same duration so that
// a sum is distinguishable from a single row's value.
func song(albumArtist, artist, album, title string, trackNo int) db.Media {
	return db.Media{
		Kind:        db.KindAudio,
		IndexedAt:   time.Date(2024, 7, 2, 9, 15, 0, 0, time.UTC),
		Version:     1,
		DurationMS:  254_000,
		Codec:       "flac",
		AlbumArtist: albumArtist,
		Artist:      artist,
		Album:       album,
		Title:       title,
		TrackNo:     trackNo,
		Year:        1997,
		Genre:       "Electronic",
	}
}

// track writes both halves and returns the stored file, which is what a caller
// needs to ask for the track back.
func track(t *testing.T, s db.Repo, ownerID, path string, m db.Media) db.File {
	t.Helper()

	f := file(path)
	f.OwnerID = ownerID
	stored := put(t, s, f)

	m.FileID = stored.ID
	if err := s.PutMedia(t.Context(), m); err != nil {
		t.Fatalf("PutMedia(%q): %v", path, err)
	}
	return stored
}
