package music_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/music"
)

// index is the library by tag, with a count of what was asked of it: the tree
// answers the same question several times per request and must not ask twice.
type index struct {
	tracks  []db.Track
	queries map[string]int
	fail    string
}

func (i *index) count(call string) error {
	if i.queries == nil {
		i.queries = map[string]int{}
	}
	i.queries[call]++
	if i.fail == call {
		return errors.New("the index is on fire")
	}
	return nil
}

func (i *index) albumArtist(t db.Track) string {
	if t.Media.AlbumArtist != "" {
		return t.Media.AlbumArtist
	}
	return t.Media.Artist
}

func (i *index) Artists(context.Context, string) ([]db.Artist, error) {
	if err := i.count("Artists"); err != nil {
		return nil, err
	}
	seen := map[string]int{}
	var out []db.Artist
	for _, t := range i.tracks {
		a := i.albumArtist(t)
		if seen[a] == 0 {
			out = append(out, db.Artist{Name: a, AlbumCount: 1})
		}
		seen[a]++
	}
	return out, nil
}

func (i *index) Albums(_ context.Context, _, artist string) ([]db.Album, error) {
	if err := i.count("Albums"); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []db.Album
	for _, t := range i.tracks {
		if i.albumArtist(t) != artist || seen[t.Media.Album] {
			continue
		}
		seen[t.Media.Album] = true
		out = append(out, db.Album{Artist: artist, Name: t.Media.Album, SongCount: 1})
	}
	return out, nil
}

func (i *index) Tracks(_ context.Context, _, artist, album string) ([]db.Track, error) {
	if err := i.count("Tracks"); err != nil {
		return nil, err
	}
	var out []db.Track
	for _, t := range i.tracks {
		if i.albumArtist(t) == artist && t.Media.Album == album {
			out = append(out, t)
		}
	}
	return out, nil
}

// track is one indexed file: an id, a path and the tags that place it.
func track(id int64, path, artist, album string) db.Track {
	return db.Track{
		File:  db.File{ID: id, Path: path, Size: 10},
		Media: db.Media{AlbumArtist: artist, Album: album, Title: path},
	}
}

func tree(t *testing.T, tracks ...db.Track) (*music.Tree, *index) {
	t.Helper()
	src := &index{tracks: tracks}
	return music.NewTree(src, "edu"), src
}

func TestTheTreeIsArtistsThenAlbumsThenTracks(t *testing.T) {
	t.Parallel()
	tr, _ := tree(t,
		track(1, "a/01.flac", "Autechre", "Amber"),
		track(2, "a/02.flac", "Autechre", "Amber"),
		track(3, "b/01.flac", "Autechre", "Tri Repetae"),
		track(4, "c/01.flac", "Björk", "Homogenic"))

	artists, err := tr.Children(t.Context(), music.Node{Dir: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(artists); fmt.Sprint(got) != "[Autechre Björk]" {
		t.Errorf("artists = %v", got)
	}

	albums, err := tr.Children(t.Context(), artists[0])
	if err != nil {
		t.Fatal(err)
	}
	if got := names(albums); fmt.Sprint(got) != "[Amber Tri Repetae]" {
		t.Errorf("albums = %v", got)
	}

	tracks, err := tr.Children(t.Context(), albums[0])
	if err != nil {
		t.Fatal(err)
	}
	if got := names(tracks); fmt.Sprint(got) != "[01.flac 02.flac]" {
		t.Errorf("tracks = %v", got)
	}
}

// TestATagIsOneSegmentHoweverManySlashesItHas: an artist called AC/DC is one
// step down and not two, and the name is replaced rather than escaped because
// this is also a WebDAV collection.
func TestATagIsOneSegmentHoweverManySlashesItHas(t *testing.T) {
	t.Parallel()
	tr, _ := tree(t, track(1, "x.flac", "AC/DC", "High Voltage"))

	n, err := tr.Resolve(t.Context(), "AC_DC/High Voltage/x.flac")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if n.Artist.Name != "AC/DC" {
		t.Errorf("the segment did not lead back to the tag: %q", n.Artist.Name)
	}
	if _, err := tr.Resolve(t.Context(), "AC/DC/High Voltage/x.flac"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the tag itself resolved as a path: %v", err)
	}
}

func TestResolveAnswersWhatIsThereAndNothingElse(t *testing.T) {
	t.Parallel()
	tr, _ := tree(t, track(1, "a/01.flac", "Autechre", "Amber"))

	for _, at := range []string{"", "Autechre", "Autechre/Amber", "Autechre/Amber/01.flac"} {
		if _, err := tr.Resolve(t.Context(), at); err != nil {
			t.Errorf("resolve %q: %v", at, err)
		}
	}
	for _, at := range []string{
		"Nobody", "Autechre/Nothing", "Autechre/Amber/02.flac", "Autechre/Amber/01.flac/x",
	} {
		if _, err := tr.Resolve(t.Context(), at); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("resolve %q = %v, want not-exist", at, err)
		}
	}
}

// TestNamesInAnAlbumCannotCollide: two discs both numbered from 01, and a
// case-folding client would take these for one file.
func TestNamesInAnAlbumCannotCollide(t *testing.T) {
	t.Parallel()
	tr, _ := tree(t,
		track(1, "d1/01.flac", "A", "B"),
		track(2, "d2/01.flac", "A", "B"),
		track(3, "d3/01.FLAC", "A", "B"))

	n, err := tr.Resolve(t.Context(), "A/B/01 (2).flac")
	if err != nil {
		t.Fatalf("the second 01.flac has no name of its own: %v", err)
	}
	if n.Track.File.ID != 2 {
		t.Errorf("01 (2).flac is file %d", n.Track.File.ID)
	}
	if _, err := tr.Resolve(t.Context(), "A/B/01 (3).FLAC"); err != nil {
		t.Errorf("the third: %v", err)
	}
}

// TestPathOfIsWhatTheCollectionAnswersTo, which is what keeps a link on a page
// and the mount behind it in step.
func TestPathOfIsWhatTheCollectionAnswersTo(t *testing.T) {
	t.Parallel()
	second := track(2, "d2/01.flac", "AC/DC", "High Voltage")
	tr, _ := tree(t, track(1, "d1/01.flac", "AC/DC", "High Voltage"), second)

	at, err := tr.PathOf(t.Context(), second)
	if err != nil {
		t.Fatal(err)
	}
	if at != "AC_DC/High Voltage/01 (2).flac" {
		t.Errorf("PathOf = %q", at)
	}
	n, err := tr.Resolve(t.Context(), at)
	if err != nil || n.Track.File.ID != 2 {
		t.Errorf("resolving what PathOf gave back = %d, %v", n.Track.File.ID, err)
	}

	// A track with no album artist is filed under the artist tag, which is
	// what the index does with it too.
	noAlbumArtist := db.Track{
		File:  db.File{ID: 3, Path: "d3/01.flac"},
		Media: db.Media{Artist: "Solo", Album: "Alone", Title: "x"},
	}
	solo := music.NewTree(&index{tracks: []db.Track{noAlbumArtist}}, "edu")
	if got, err := solo.PathOf(t.Context(), noAlbumArtist); err != nil || got != "Solo/Alone/01.flac" {
		t.Errorf("a track with no album artist = %q, %v", got, err)
	}

	// Right tags, a file nothing knows: an address it does not have.
	ghost := db.Track{File: db.File{ID: 99, Path: "ghost.flac"}, Media: second.Media}
	if _, err := tr.PathOf(t.Context(), ghost); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a track that is not in its album = %v", err)
	}

	if _, err := tr.PathOfArtist(t.Context(), "Nobody"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("an artist that is not there = %v", err)
	}
	if _, err := tr.PathOfAlbum(t.Context(), "AC/DC", "Nothing"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("an album that is not there = %v", err)
	}
}

// TestALevelIsReadOnce: naming every track of a page asks for its album, and
// that has to cost one query rather than one each.
func TestALevelIsReadOnce(t *testing.T) {
	t.Parallel()
	var tracks []db.Track
	for i := range 30 {
		tracks = append(tracks, track(int64(i+1), fmt.Sprintf("d/%02d.flac", i), "A", "B"))
	}
	tr, src := tree(t, tracks...)

	for _, t2 := range tracks {
		if _, err := tr.PathOf(t.Context(), t2); err != nil {
			t.Fatal(err)
		}
	}
	for call, want := range map[string]int{"Artists": 1, "Albums": 1, "Tracks": 1} {
		if got := src.queries[call]; got != want {
			t.Errorf("%s was asked %d times, want %d", call, got, want)
		}
	}
}

func TestABrokenIndexIsAnError(t *testing.T) {
	t.Parallel()
	for _, call := range []string{"Artists", "Albums", "Tracks"} {
		src := &index{tracks: []db.Track{track(1, "a.flac", "A", "B")}, fail: call}
		tr := music.NewTree(src, "edu")
		if _, err := tr.Resolve(t.Context(), "A/B/a.flac"); err == nil || errors.Is(err, os.ErrNotExist) {
			t.Errorf("resolve with %s broken = %v, want a real error", call, err)
		}
	}
}

// TestAReadThatBreaksPartWayIsAnError: naming a track reads three levels, and
// a failure at any of them is an error rather than a name made up.
func TestAReadThatBreaksPartWayIsAnError(t *testing.T) {
	t.Parallel()
	only := track(1, "a.flac", "A", "B")

	for _, call := range []string{"Artists", "Albums", "Tracks"} {
		tr := music.NewTree(&index{tracks: []db.Track{only}, fail: call}, "edu")
		if _, err := tr.PathOf(t.Context(), only); err == nil || errors.Is(err, os.ErrNotExist) {
			t.Errorf("PathOf with %s broken = %v, want a real error", call, err)
		}
	}
	for _, call := range []string{"Artists", "Albums"} {
		tr := music.NewTree(&index{tracks: []db.Track{only}, fail: call}, "edu")
		if _, err := tr.PathOfAlbum(t.Context(), "A", "B"); err == nil || errors.Is(err, os.ErrNotExist) {
			t.Errorf("PathOfAlbum with %s broken = %v", call, err)
		}
	}
	tr := music.NewTree(&index{tracks: []db.Track{only}, fail: "Artists"}, "edu")
	if _, err := tr.PathOfArtist(t.Context(), "A"); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Errorf("PathOfArtist with the artists broken = %v", err)
	}
}

// TestAListingBreaksTheSameWay, at whichever level the read failed.
func TestAListingBreaksTheSameWay(t *testing.T) {
	t.Parallel()
	only := track(1, "a.flac", "A", "B")

	for _, tc := range []struct {
		call string
		node music.Node
	}{
		{"Artists", music.Node{Dir: true}},
		{"Albums", music.Node{Artist: db.Artist{Name: "A"}, ArtistName: "A", Dir: true}},
		{"Tracks", music.Node{Artist: db.Artist{Name: "A"}, ArtistName: "A", Album: db.Album{Name: "B"}, AlbumName: "B", Dir: true}},
	} {
		tr := music.NewTree(&index{tracks: []db.Track{only}, fail: tc.call}, "edu")
		if _, err := tr.Children(t.Context(), tc.node); err == nil {
			t.Errorf("listing with %s broken did not fail", tc.call)
		}
	}

	// A track holds nothing, and says so rather than listing its album again.
	tr, _ := tree(t, only)
	n, err := tr.Resolve(t.Context(), "A/B/a.flac")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Children(t.Context(), n); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("listing a track = %v, want not-exist", err)
	}
}

func TestAnEmptyTagStillHasAName(t *testing.T) {
	t.Parallel()
	if got := music.Segment("   ", "Unknown artist"); got != "Unknown artist" {
		t.Errorf("a blank tag = %q", got)
	}
	if got := music.Segment("..", "Playlist"); got != "Playlist" {
		t.Errorf("a name of dots = %q", got)
	}
	if got := music.Segment(`what? "now": <yes>|*`, "x"); strings.ContainsAny(got, `/\<>:"|?*`) {
		t.Errorf("a name Windows refuses survived: %q", got)
	}
}

func names(nodes []music.Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.Base())
	}
	return out
}
