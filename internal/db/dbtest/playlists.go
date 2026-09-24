package dbtest

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// RunPlaylists executes the playlist cases against the repository built by
// newRepo.
func RunPlaylists(t *testing.T, newRepo func(t *testing.T) db.Repo) {
	t.Helper()

	cases := []struct {
		name string
		fn   func(t *testing.T, s db.Repo)
	}{
		{"a playlist comes back as it was stored", playlistRoundTrip},
		{"tracks come back in order, a repeat included", playlistOrder},
		{"setting the tracks replaces them", playlistReplace},
		{"a deleted file leaves every playlist it was in", playlistCascade},
		{"a file that is no longer audio is not listed or counted", playlistNotAudio},
		{"a playlist is renamed, commented and made public", playlistUpdate},
		{"deleting a playlist deletes what it held", playlistDelete},
		{"a missing playlist is ErrNotFound everywhere", playlistMissing},
		{"owners do not see each other's playlists", playlistOwnersAreSeparate},
		{"playlists are listed by name", playlistListOrder},
		{"a long playlist is stored whole", playlistLong},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.fn(t, newRepo(t))
		})
	}
}

var playlistAt = time.Date(2024, 9, 1, 20, 0, 0, 125_000_000, time.UTC)

func newPlaylist(t *testing.T, s db.Repo, ownerID, name string) db.Playlist {
	t.Helper()
	p, err := s.CreatePlaylist(t.Context(), db.Playlist{
		OwnerID: ownerID, Name: name, Created: playlistAt, Changed: playlistAt,
	})
	if err != nil {
		t.Fatalf("CreatePlaylist(%q): %v", name, err)
	}
	return p
}

func setTracks(t *testing.T, s db.Repo, p db.Playlist, files ...db.File) {
	t.Helper()
	ids := make([]int64, len(files))
	for i, f := range files {
		ids[i] = f.ID
	}
	if err := s.SetPlaylistTracks(t.Context(), p.OwnerID, p.ID, ids, playlistAt.Add(time.Hour)); err != nil {
		t.Fatalf("SetPlaylistTracks: %v", err)
	}
}

func playlistTitles(t *testing.T, s db.Repo, p db.Playlist) []string {
	t.Helper()
	got, err := s.PlaylistTracks(t.Context(), p.OwnerID, p.ID)
	if err != nil {
		t.Fatalf("PlaylistTracks: %v", err)
	}
	return trackTitles(got)
}

func playlistByID(t *testing.T, s db.Repo, p db.Playlist) db.Playlist {
	t.Helper()
	got, err := s.PlaylistByID(t.Context(), p.OwnerID, p.ID)
	if err != nil {
		t.Fatalf("PlaylistByID: %v", err)
	}
	return got
}

func playlistRoundTrip(t *testing.T, s db.Repo) {
	created, err := s.CreatePlaylist(t.Context(), db.Playlist{
		OwnerID: owner, Name: "Night drive", Comment: "for the A1", Public: true,
		Created: playlistAt, Changed: playlistAt.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.ID <= 0 {
		t.Fatalf("created id = %d", created.ID)
	}

	got := playlistByID(t, s, created)
	want := db.Playlist{
		ID: created.ID, OwnerID: owner, Name: "Night drive", Comment: "for the A1", Public: true,
		Created: playlistAt, Changed: playlistAt.Add(time.Minute),
	}
	if got != want {
		t.Errorf("PlaylistByID =\n%+v\nwant\n%+v", got, want)
	}
	if created != want {
		t.Errorf("CreatePlaylist =\n%+v\nwant\n%+v", created, want)
	}
}

func playlistOrder(t *testing.T, s db.Repo) {
	a := track(t, s, owner, "music/a.flac", song("Autechre", "Autechre", "Tri Repetae", "Rotar", 1))
	b := track(t, s, owner, "music/b.flac", song("Burial", "Burial", "Untrue", "Archangel", 1))
	p := newPlaylist(t, s, owner, "Mix")

	setTracks(t, s, p, b, a, b)

	if got := playlistTitles(t, s, p); !equal(got, []string{"Archangel", "Rotar", "Archangel"}) {
		t.Errorf("tracks = %v, want the order they were set in", got)
	}
	got := playlistByID(t, s, p)
	if got.SongCount != 3 || got.DurationMS != 3*254_000 {
		t.Errorf("counts = %d songs, %d ms, want every entry counted", got.SongCount, got.DurationMS)
	}
	if !got.Changed.Equal(playlistAt.Add(time.Hour)) {
		t.Errorf("changed = %v, want the time the tracks were set", got.Changed)
	}
}

func playlistReplace(t *testing.T, s db.Repo) {
	a := track(t, s, owner, "music/a.flac", song("Autechre", "Autechre", "Tri Repetae", "Rotar", 1))
	b := track(t, s, owner, "music/b.flac", song("Burial", "Burial", "Untrue", "Archangel", 1))
	p := newPlaylist(t, s, owner, "Mix")

	setTracks(t, s, p, a, b)
	setTracks(t, s, p, b)
	if got := playlistTitles(t, s, p); !equal(got, []string{"Archangel"}) {
		t.Errorf("tracks = %v after a replace, want only the new list", got)
	}
	setTracks(t, s, p)
	if got := playlistTitles(t, s, p); len(got) != 0 {
		t.Errorf("tracks = %v after emptying, want none", got)
	}
}

func playlistCascade(t *testing.T, s db.Repo) {
	a := track(t, s, owner, "music/a.flac", song("Autechre", "Autechre", "Tri Repetae", "Rotar", 1))
	b := track(t, s, owner, "music/b.flac", song("Burial", "Burial", "Untrue", "Archangel", 1))
	one, two := newPlaylist(t, s, owner, "One"), newPlaylist(t, s, owner, "Two")
	setTracks(t, s, one, a, b, a)
	setTracks(t, s, two, a)

	if err := s.DeleteFile(t.Context(), owner, "music/a.flac"); err != nil {
		t.Fatal(err)
	}
	if got := playlistTitles(t, s, one); !equal(got, []string{"Archangel"}) {
		t.Errorf("one = %v, want the deleted track gone from both places", got)
	}
	if got := playlistTitles(t, s, two); len(got) != 0 {
		t.Errorf("two = %v, want empty", got)
	}
	if got := playlistByID(t, s, one); got.SongCount != 1 {
		t.Errorf("one counts %d songs, want 1", got.SongCount)
	}

	// A gap in the positions is not a problem for the next write.
	setTracks(t, s, one, b, b)
	if got := playlistTitles(t, s, one); !equal(got, []string{"Archangel", "Archangel"}) {
		t.Errorf("after the gap, one = %v", got)
	}
}

func playlistNotAudio(t *testing.T, s db.Repo) {
	a := track(t, s, owner, "music/a.flac", song("Autechre", "Autechre", "Tri Repetae", "Rotar", 1))
	b := track(t, s, owner, "music/b.flac", song("Burial", "Burial", "Untrue", "Archangel", 1))
	p := newPlaylist(t, s, owner, "Mix")
	setTracks(t, s, p, a, b)

	photo := song("", "", "", "", 0)
	photo.Kind, photo.FileID = db.KindImage, b.ID
	if err := s.PutMedia(t.Context(), photo); err != nil {
		t.Fatal(err)
	}

	if got := playlistTitles(t, s, p); !equal(got, []string{"Rotar"}) {
		t.Errorf("tracks = %v, want what is still audio", got)
	}
	if got := playlistByID(t, s, p); got.SongCount != 1 {
		t.Errorf("song count = %d, want the listing's", got.SongCount)
	}
}

func playlistUpdate(t *testing.T, s db.Repo) {
	p := newPlaylist(t, s, owner, "Mix")
	p.Name, p.Comment, p.Public, p.Changed = "Mixtape", "side A", true, playlistAt.Add(time.Hour)
	if err := s.UpdatePlaylist(t.Context(), p); err != nil {
		t.Fatal(err)
	}

	got := playlistByID(t, s, p)
	if got.Name != "Mixtape" || got.Comment != "side A" || !got.Public || !got.Changed.Equal(p.Changed) {
		t.Errorf("after an update = %+v", got)
	}
	if !got.Created.Equal(playlistAt) {
		t.Errorf("created = %v, want it untouched", got.Created)
	}
}

func playlistDelete(t *testing.T, s db.Repo) {
	a := track(t, s, owner, "music/a.flac", song("Autechre", "Autechre", "Tri Repetae", "Rotar", 1))
	p := newPlaylist(t, s, owner, "Mix")
	setTracks(t, s, p, a)

	if err := s.DeletePlaylist(t.Context(), owner, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PlaylistByID(t.Context(), owner, p.ID); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("PlaylistByID after delete = %v, want ErrNotFound", err)
	}
	// The file is untouched: an entry is a reference, not the track.
	if _, err := s.TrackByFile(t.Context(), owner, a.ID); err != nil {
		t.Errorf("the track went with the playlist: %v", err)
	}
	if err := s.DeletePlaylist(t.Context(), owner, p.ID); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("deleting twice = %v, want ErrNotFound", err)
	}
}

func playlistMissing(t *testing.T, s db.Repo) {
	ctx := t.Context()
	const id = 424242
	calls := map[string]error{
		"PlaylistByID": func() error { _, err := s.PlaylistByID(ctx, owner, id); return err }(),
		"LockPlaylist": s.LockPlaylist(ctx, owner, id),
		"UpdatePlaylist": s.UpdatePlaylist(ctx, db.Playlist{
			ID: id, OwnerID: owner, Name: "x", Created: playlistAt, Changed: playlistAt,
		}),
		"SetPlaylistTracks": s.SetPlaylistTracks(ctx, owner, id, nil, playlistAt),
		"DeletePlaylist":    s.DeletePlaylist(ctx, owner, id),
	}
	for name, err := range calls {
		if !errors.Is(err, db.ErrNotFound) {
			t.Errorf("%s of a missing playlist = %v, want ErrNotFound", name, err)
		}
	}
	if got, err := s.PlaylistTracks(ctx, owner, id); err != nil || len(got) != 0 {
		t.Errorf("PlaylistTracks of a missing playlist = %v, %v, want nothing", got, err)
	}
	if err := s.LockPlaylist(ctx, owner, newPlaylist(t, s, owner, "Here").ID); err != nil {
		t.Errorf("LockPlaylist of one that is there = %v", err)
	}
}

func playlistOwnersAreSeparate(t *testing.T, s db.Repo) {
	a := track(t, s, owner, "music/a.flac", song("Autechre", "Autechre", "Tri Repetae", "Rotar", 1))
	mine := newPlaylist(t, s, owner, "Mine")
	setTracks(t, s, mine, a)

	const other = "someone-else"
	if list, err := s.Playlists(t.Context(), other); err != nil || len(list) != 0 {
		t.Errorf("another owner lists %v, %v", list, err)
	}
	if _, err := s.PlaylistByID(t.Context(), other, mine.ID); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("another owner reads it: %v", err)
	}
	if got, _ := s.PlaylistTracks(t.Context(), other, mine.ID); len(got) != 0 {
		t.Errorf("another owner reads its tracks: %v", trackTitles(got))
	}
	if err := s.SetPlaylistTracks(t.Context(), other, mine.ID, nil, playlistAt); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("another owner empties it: %v", err)
	}
	if err := s.DeletePlaylist(t.Context(), other, mine.ID); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("another owner deletes it: %v", err)
	}
	if got := playlistTitles(t, s, mine); !equal(got, []string{"Rotar"}) {
		t.Errorf("after all that mine holds %v", got)
	}
}

func playlistListOrder(t *testing.T, s db.Repo) {
	for _, name := range []string{"Zeta", "Alpha", "Mid"} {
		newPlaylist(t, s, owner, name)
	}
	list, err := s.Playlists(t.Context(), owner)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(list))
	for i, p := range list {
		names[i] = p.Name
	}
	if !slices.Equal(names, []string{"Alpha", "Mid", "Zeta"}) {
		t.Errorf("Playlists = %v, want name order", names)
	}
}

// playlistLong crosses the batch a driver inserts in, where an off-by-one
// would drop or repeat the entries at the seam.
func playlistLong(t *testing.T, s db.Repo) {
	a := track(t, s, owner, "music/a.flac", song("Autechre", "Autechre", "Tri Repetae", "Rotar", 1))
	b := track(t, s, owner, "music/b.flac", song("Burial", "Burial", "Untrue", "Archangel", 1))
	p := newPlaylist(t, s, owner, "Long")

	const n = 2503
	ids := make([]int64, n)
	for i := range ids {
		ids[i] = a.ID
		if i%1000 == 999 {
			ids[i] = b.ID
		}
	}
	if err := s.SetPlaylistTracks(t.Context(), owner, p.ID, ids, playlistAt); err != nil {
		t.Fatal(err)
	}

	got, err := s.PlaylistTracks(t.Context(), owner, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != n {
		t.Fatalf("%d tracks, want %d", len(got), n)
	}
	for i, tr := range got {
		if tr.File.ID != ids[i] {
			t.Fatalf("entry %d is file %d, want %d", i, tr.File.ID, ids[i])
		}
	}
}
