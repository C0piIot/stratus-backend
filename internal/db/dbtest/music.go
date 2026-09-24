package dbtest

import (
	"errors"
	"fmt"
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
		{"a listing comes back in the order it was asked for", musicAlbumListOrders},
		{"a listing pages without gaps or repeats", musicAlbumListPages},
		{"filtering a listing does not distort what it counts", musicAlbumListFilters},
		{"a listing of tracks filters and orders", musicTrackList},
		{"an unknown order is refused rather than guessed", musicUnknownOrders},
		{"genres count what they hold", musicGenres},
		{"search matches all three things by name", musicSearch},
		{"search does not depend on the engine's idea of case", musicSearchCase},
		{"a wildcard in a search is text and not a wildcard", musicSearchWildcards},
		{"an empty search is the whole library", musicSearchEmpty},
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
	// A track is read through its own scan, not MediaByFile's, so the audio
	// stream is asserted on this path too.
	if got.Media.SampleRate != 96_000 || got.Media.BitDepth != 24 || got.Media.Bitrate != 2_300_000 || got.Media.Channels != 2 {
		t.Errorf("the audio stream is wrong: %+v", got.Media)
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

// musicAlbumListOrders pins each way a client asks to see a library. The names
// and years are chosen so that no two orders agree, which is what makes the
// assertions distinguish them.
func musicAlbumListOrders(t *testing.T, s db.Repo) {
	// Written oldest-arriving first, so the added order is not the same as any
	// alphabetical one.
	catalogue(t, s,
		record{artist: "Zomby", album: "Aaron", year: 2011, genre: "Electronic"},
		record{artist: "Autechre", album: "Zeta", year: 1994, genre: "Electronic"},
		record{artist: "Móveis", album: "Meia", year: 2003, genre: "Rock"},
	)

	page := db.Page{Limit: 10}
	tests := map[db.AlbumOrder][]string{
		db.AlbumsByName:     {"Aaron", "Meia", "Zeta"},
		db.AlbumsByArtist:   {"Zeta", "Meia", "Aaron"},
		db.AlbumsByAdded:    {"Meia", "Zeta", "Aaron"},
		db.AlbumsByYear:     {"Zeta", "Meia", "Aaron"},
		db.AlbumsByYearDesc: {"Aaron", "Meia", "Zeta"},
	}
	for order, want := range tests {
		got, err := s.AlbumList(t.Context(), owner, db.AlbumFilter{Order: order, Page: page})
		if err != nil {
			t.Fatalf("AlbumList(%s): %v", order, err)
		}
		if names := albumNames(got); !equal(names, want) {
			t.Errorf("AlbumList(%s) = %v, want %v", order, names, want)
		}
	}

	// A random listing is a different set by definition, so what is pinned is
	// that it is the same albums and the limit is honoured.
	random, err := s.AlbumList(t.Context(), owner, db.AlbumFilter{Order: db.AlbumsRandom, Page: db.Page{Limit: 2}})
	if err != nil {
		t.Fatalf("AlbumList(random): %v", err)
	}
	if len(random) != 2 {
		t.Errorf("AlbumList(random) returned %d albums, want the limit", len(random))
	}
}

func musicAlbumListPages(t *testing.T, s db.Repo) {
	catalogue(t, s,
		record{artist: "A", album: "One", year: 2001, genre: "Rock"},
		record{artist: "B", album: "Two", year: 2002, genre: "Rock"},
		record{artist: "C", album: "Three", year: 2003, genre: "Rock"},
	)

	// Two pages of two, which is how a client walks a library it cannot hold.
	var seen []string
	for offset := 0; offset < 4; offset += 2 {
		f := db.AlbumFilter{Order: db.AlbumsByArtist, Page: db.Page{Limit: 2, Offset: offset}}
		got, err := s.AlbumList(t.Context(), owner, f)
		if err != nil {
			t.Fatalf("AlbumList at offset %d: %v", offset, err)
		}
		seen = append(seen, albumNames(got)...)
	}
	if want := []string{"One", "Two", "Three"}; !equal(seen, want) {
		t.Errorf("paging returned %v, want %v exactly once each", seen, want)
	}

	// A limit of zero is zero rows, the same answer PendingMedia gives. It is
	// stated because the alternative reading -- "no limit" -- is the bug.
	none, err := s.AlbumList(t.Context(), owner, db.AlbumFilter{Order: db.AlbumsByName})
	if err != nil || len(none) != 0 {
		t.Errorf("AlbumList with no limit = %+v, %v", none, err)
	}
}

// musicAlbumListFilters is the case that says why the genre and the year are
// filtered after grouping. An album with one track in a genre is in that genre,
// and it still has to report all of its tracks: filtering them out first would
// leave the album in the answer with half its songs.
func musicAlbumListFilters(t *testing.T, s db.Repo) {
	mixed := record{artist: "V", album: "Mixed", year: 1999, genre: "Rock"}
	catalogue(t, s, mixed)
	// A second track on the same album, in another genre and a later year.
	other := song("V", "V", "Mixed", "Second", 2)
	other.Genre = "Jazz"
	other.Year = 1999
	track(t, s, owner, "music/mixed-2.flac", other)
	catalogue(t, s, record{artist: "W", album: "Elsewhere", year: 1970, genre: "Folk"})

	page := db.Page{Limit: 10}
	got, err := s.AlbumList(t.Context(), owner, db.AlbumFilter{Order: db.AlbumsByName, Genre: "Jazz", Page: page})
	if err != nil {
		t.Fatalf("AlbumList: %v", err)
	}
	if len(got) != 1 || got[0].Name != "Mixed" {
		t.Fatalf("AlbumList by genre = %+v, want the album with a track in it", albumNames(got))
	}
	if got[0].SongCount != 2 {
		t.Errorf("SongCount = %d, want 2: the filter must not drop the album's other tracks", got[0].SongCount)
	}

	// A year range, inclusive at both ends.
	inRange, err := s.AlbumList(t.Context(), owner,
		db.AlbumFilter{Order: db.AlbumsByName, FromYear: 1999, ToYear: 1999, Page: page})
	if err != nil {
		t.Fatal(err)
	}
	if names := albumNames(inRange); !equal(names, []string{"Mixed"}) {
		t.Errorf("AlbumList in 1999 = %v", names)
	}
	// Unbounded above, which is a zero rather than a large number.
	from, err := s.AlbumList(t.Context(), owner,
		db.AlbumFilter{Order: db.AlbumsByName, FromYear: 1980, Page: page})
	if err != nil {
		t.Fatal(err)
	}
	if names := albumNames(from); !equal(names, []string{"Mixed"}) {
		t.Errorf("AlbumList from 1980 = %v", names)
	}
}

func musicTrackList(t *testing.T, s db.Repo) {
	first := song("A", "A", "One", "First", 1)
	first.Genre, first.Year = "Rock", 2001
	second := song("A", "A", "One", "Second", 2)
	second.Genre, second.Year = "Jazz", 2002
	track(t, s, owner, "music/b.flac", second)
	track(t, s, owner, "music/a.flac", first)

	page := db.Page{Limit: 10}
	got, err := s.TrackList(t.Context(), owner, db.TrackFilter{Order: db.TracksByPath, Page: page})
	if err != nil {
		t.Fatalf("TrackList: %v", err)
	}
	if titles := trackTitles(got); !equal(titles, []string{"First", "Second"}) {
		t.Errorf("TrackList = %v, want path order", titles)
	}

	// The year is the track's own here, not an album's aggregate.
	byGenre, err := s.TrackList(t.Context(), owner,
		db.TrackFilter{Order: db.TracksByPath, Genre: "Jazz", Page: page})
	if err != nil {
		t.Fatal(err)
	}
	if titles := trackTitles(byGenre); !equal(titles, []string{"Second"}) {
		t.Errorf("TrackList by genre = %v", titles)
	}
	byYear, err := s.TrackList(t.Context(), owner,
		db.TrackFilter{Order: db.TracksByPath, FromYear: 2002, ToYear: 2002, Page: page})
	if err != nil {
		t.Fatal(err)
	}
	if titles := trackTitles(byYear); !equal(titles, []string{"Second"}) {
		t.Errorf("TrackList by year = %v", titles)
	}

	random, err := s.TrackList(t.Context(), owner, db.TrackFilter{Order: db.TracksRandom, Page: db.Page{Limit: 1}})
	if err != nil || len(random) != 1 {
		t.Errorf("TrackList(random) = %+v, %v", trackTitles(random), err)
	}
}

// musicUnknownOrders is the answer to a caller mistake: refuse it rather than
// pick an order and return something plausible.
func musicUnknownOrders(t *testing.T, s db.Repo) {
	if _, err := s.AlbumList(t.Context(), owner, db.AlbumFilter{Order: "by vibe", Page: db.Page{Limit: 1}}); err == nil {
		t.Error("AlbumList with an unknown order returned no error")
	}
	if _, err := s.TrackList(t.Context(), owner, db.TrackFilter{Order: "by vibe", Page: db.Page{Limit: 1}}); err == nil {
		t.Error("TrackList with an unknown order returned no error")
	}
}

func musicGenres(t *testing.T, s db.Repo) {
	// Two albums in Rock, one of them with two tracks, and one track with no
	// genre tag at all.
	catalogue(t, s,
		record{artist: "A", album: "One", year: 2001, genre: "Rock"},
		record{artist: "B", album: "Two", year: 2002, genre: "Rock"},
		record{artist: "C", album: "Three", year: 2003, genre: "Jazz"},
	)
	extra := song("A", "A", "One", "Second", 2)
	extra.Genre = "Rock"
	track(t, s, owner, "music/one-2.flac", extra)
	untagged := song("D", "D", "Four", "Untagged", 1)
	untagged.Genre = ""
	track(t, s, owner, "music/four.flac", untagged)

	got, err := s.Genres(t.Context(), owner)
	if err != nil {
		t.Fatalf("Genres: %v", err)
	}
	// Name order, and the track with no genre is in none of them.
	if len(got) != 2 || got[0].Name != "Jazz" || got[1].Name != "Rock" {
		t.Fatalf("Genres = %+v, want Jazz and Rock", got)
	}
	if got[1].SongCount != 3 {
		t.Errorf("Rock SongCount = %d, want 3", got[1].SongCount)
	}
	// Two albums, not three tracks: the counts are not derivable from each
	// other, which is why the schema asks for both.
	if got[1].AlbumCount != 2 {
		t.Errorf("Rock AlbumCount = %d, want 2", got[1].AlbumCount)
	}
}

func musicSearch(t *testing.T, s db.Repo) {
	catalogue(t, s,
		record{artist: "Boards of Canada", album: "Geogaddi", year: 2002, genre: "Electronic"},
		record{artist: "Autechre", album: "Tri Repetae", year: 1995, genre: "Electronic"},
	)
	// A track whose own title is the only place the word appears.
	odd := song("Autechre", "Autechre", "Tri Repetae", "Leterel", 2)
	track(t, s, owner, "music/leterel.flac", odd)

	page := db.Page{Limit: 10}
	all := db.SearchFilter{Text: "aute", Artists: page, Albums: page, Tracks: page}
	got, err := s.Search(t.Context(), owner, all)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got.Artists) != 1 || got.Artists[0].Name != "Autechre" {
		t.Errorf("Artists = %+v, want the one whose name matches", got.Artists)
	}
	// The album's own name does not contain the term, so the album bucket is
	// empty even though its artist matched. Three questions, three answers.
	if len(got.Albums) != 0 {
		t.Errorf("Albums = %v, want none: no album name contains the term", albumNames(got.Albums))
	}
	// The tracks bucket is wider on purpose: a track matches on its title, its
	// album or its artist, because that is how a client's search box is used.
	if len(got.Tracks) != 2 {
		t.Errorf("Tracks = %v, want both of that artist's", trackTitles(got.Tracks))
	}

	// A title nothing else shares.
	one, err := s.Search(t.Context(), owner, db.SearchFilter{Text: "leterel", Tracks: page})
	if err != nil {
		t.Fatal(err)
	}
	if titles := trackTitles(one.Tracks); !equal(titles, []string{"Leterel"}) {
		t.Errorf("Search for a title = %v", titles)
	}
	// And the buckets with no page asked for come back empty rather than full.
	if len(one.Artists) != 0 || len(one.Albums) != 0 {
		t.Errorf("a zero page returned rows: %+v", one)
	}
}

// musicSearchCase is the case that fails the moment somebody moves the folding
// into SQL. SQLite's lower() folds ASCII and nothing else while PostgreSQL's
// folds Unicode, so an accented capital is where the two engines part company --
// and this suite is what says they must not.
//
// Every bucket is asserted separately and on purpose. An earlier version of
// this case only counted tracks, and a track matches on its artist as well as
// its title: it passed with lower() back in the SQL, because the one disjunct
// that still read a folded column carried it.
func musicSearchCase(t *testing.T, s db.Repo) {
	catalogue(t, s, record{artist: "BJÖRK", album: "HOMOGENIC", year: 1997, genre: "Electronic"})
	// A title of its own, so the song column is exercised by something the
	// artist and album columns cannot answer.
	m := song("BJÖRK", "BJÖRK", "HOMOGENIC", "JÓGA", 2)
	track(t, s, owner, "music/joga.flac", m)

	page := db.Page{Limit: 10}
	tests := []struct {
		term                    string
		artists, albums, tracks int
	}{
		{term: "björk", artists: 1, albums: 0, tracks: 2},
		{term: "BJÖRK", artists: 1, albums: 0, tracks: 2},
		{term: "BjÖrK", artists: 1, albums: 0, tracks: 2},
		{term: "homogenic", artists: 0, albums: 1, tracks: 2},
		{term: "HOMOGENIC", artists: 0, albums: 1, tracks: 2},
		// Only the title of one track, so nothing else can carry this one.
		{term: "jóga", artists: 0, albums: 0, tracks: 1},
		{term: "JÓGA", artists: 0, albums: 0, tracks: 1},
	}

	for _, tt := range tests {
		got, err := s.Search(t.Context(), owner,
			db.SearchFilter{Text: tt.term, Artists: page, Albums: page, Tracks: page})
		if err != nil {
			t.Fatalf("Search(%q): %v", tt.term, err)
		}
		if len(got.Artists) != tt.artists {
			t.Errorf("Search(%q) found %d artists, want %d", tt.term, len(got.Artists), tt.artists)
		}
		if len(got.Albums) != tt.albums {
			t.Errorf("Search(%q) found %d albums, want %d", tt.term, len(got.Albums), tt.albums)
		}
		if len(got.Tracks) != tt.tracks {
			t.Errorf("Search(%q) found %d tracks, want %d", tt.term, len(got.Tracks), tt.tracks)
		}
	}
}

// musicSearchWildcards is the other half of not trusting the engine: % and _
// are LIKE metacharacters, and a user typing one means the character.
func musicSearchWildcards(t *testing.T, s db.Repo) {
	catalogue(t, s,
		record{artist: "Silk", album: "100% Silk", year: 2011, genre: "Electronic"},
		record{artist: "Other", album: "Nothing Like It", year: 2011, genre: "Rock"},
	)

	page := db.Page{Limit: 10}
	got, err := s.Search(t.Context(), owner, db.SearchFilter{Text: "%", Albums: page})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	// Unescaped, the pattern would be %%% and every album would match.
	if names := albumNames(got.Albums); !equal(names, []string{"100% Silk"}) {
		t.Errorf("Search(%%) = %v, want only the album with one in its name", names)
	}
}

// musicSearchEmpty is required by the specification and by every client: an
// empty query is how one walks the whole library for offline use.
func musicSearchEmpty(t *testing.T, s db.Repo) {
	catalogue(t, s,
		record{artist: "A", album: "One", year: 2001, genre: "Rock"},
		record{artist: "B", album: "Two", year: 2002, genre: "Rock"},
	)

	page := db.Page{Limit: 10}
	got, err := s.Search(t.Context(), owner, db.SearchFilter{Artists: page, Albums: page, Tracks: page})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got.Artists) != 2 || len(got.Albums) != 2 || len(got.Tracks) != 2 {
		t.Errorf("an empty search returned %d artists, %d albums, %d tracks, want everything",
			len(got.Artists), len(got.Albums), len(got.Tracks))
	}
}

// record is one album of one track, which is all most cases need.
type record struct {
	artist, album, genre string
	year                 int
}

// catalogue writes one track per record, each in its own file, one minute apart
// -- so a listing by arrival is the reverse of the order written here.
//
// It writes the file itself rather than going through track() for exactly that
// reason: the fixture's MTime is a constant, and with every album arriving at
// the same instant the added order would be decided by the tie-break and the
// case would pass whatever the ORDER BY said.
func catalogue(t *testing.T, s db.Repo, records ...record) {
	t.Helper()
	base := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)

	for i, rec := range records {
		path := fmt.Sprintf("music/%d-%s.flac", i, rec.album)
		f := file(path)
		f.MTime = base.Add(time.Duration(i) * time.Minute)
		stored := put(t, s, f)

		m := song(rec.artist, rec.artist, rec.album, "Track", 1)
		m.Genre, m.Year = rec.genre, rec.year
		m.FileID = stored.ID
		if err := s.PutMedia(t.Context(), m); err != nil {
			t.Fatalf("PutMedia(%q): %v", path, err)
		}
	}
}

func albumNames(albums []db.Album) []string {
	out := make([]string, len(albums))
	for i, a := range albums {
		out[i] = a.Name
	}
	return out
}

func trackTitles(tracks []db.Track) []string {
	out := make([]string, len(tracks))
	for i, tr := range tracks {
		out[i] = tr.Media.Title
	}
	return out
}

func equal(got, want []string) bool {
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

// song builds a plausible audio row. Every track gets the same duration so that
// a sum is distinguishable from a single row's value.
func song(albumArtist, artist, album, title string, trackNo int) db.Media {
	return db.Media{
		Kind:        db.KindAudio,
		IndexedAt:   time.Date(2024, 7, 2, 9, 15, 0, 0, time.UTC),
		Version:     1,
		DurationMS:  254_000,
		Codec:       "flac",
		Bitrate:     2_300_000,
		SampleRate:  96_000,
		Channels:    2,
		BitDepth:    24,
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
