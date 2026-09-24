package music_test

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/db/dbtest"
	"github.com/C0piIot/stratus-backend/internal/db/sqlite"
	"github.com/C0piIot/stratus-backend/internal/music"
)

const owner = "edu"

// open is a real SQLite store in a file, so a case can close it and open it
// again: surviving a restart is part of what is being tested.
func open(t *testing.T, path string) *sqlite.Store {
	t.Helper()
	store, err := sqlite.New(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return store
}

// library is a store with tracks named A to E, in that order.
func library(t *testing.T) (*sqlite.Store, map[string]int64, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stratus.db")
	store := open(t, path)

	ids := map[string]int64{}
	for i, title := range []string{"A", "B", "C", "D", "E"} {
		f, err := store.PutFile(t.Context(), db.File{
			OwnerID: owner, Path: fmt.Sprintf("music/%s.flac", title), BlobKey: "k" + title,
			Size: 10, MTime: time.Now(), ETag: `"e"`, MIMEType: "audio/flac",
		})
		if err != nil {
			t.Fatal(err)
		}
		err = store.PutMedia(t.Context(), db.Media{
			FileID: f.ID, Kind: db.KindAudio, IndexedAt: time.Now(), Version: 1,
			Title: title, Album: "Album", AlbumArtist: "Artist", TrackNo: i + 1, DurationMS: 1000,
		})
		if err != nil {
			t.Fatal(err)
		}
		ids[title] = f.ID
	}
	return store, ids, path
}

func of(ids map[string]int64, titles ...string) []int64 {
	out := make([]int64, len(titles))
	for i, title := range titles {
		out[i] = ids[title]
	}
	return out
}

func titles(t *testing.T, s *music.Service, id int64) []string {
	t.Helper()
	_, tracks, err := s.Playlist(t.Context(), owner, id)
	if err != nil {
		t.Fatalf("Playlist: %v", err)
	}
	out := make([]string, len(tracks))
	for i, tr := range tracks {
		out[i] = tr.Media.Title
	}
	return out
}

func TestCreateAndRead(t *testing.T) {
	t.Parallel()
	store, ids, _ := library(t)
	s := music.New(store)

	p, err := s.Create(t.Context(), owner, "Mix", of(ids, "C", "A", "C"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "Mix" || p.SongCount != 3 || p.DurationMS != 3000 {
		t.Errorf("created = %+v", p)
	}
	if got := titles(t, s, p.ID); !slices.Equal(got, []string{"C", "A", "C"}) {
		t.Errorf("tracks = %v", got)
	}

	list, err := s.Playlists(t.Context(), owner)
	if err != nil || len(list) != 1 || list[0].SongCount != 3 {
		t.Errorf("Playlists = %+v, %v", list, err)
	}
}

// TestAPlaylistSurvivesARestart is the acceptance: it is a row, not something
// held by the process.
func TestAPlaylistSurvivesARestart(t *testing.T) {
	t.Parallel()
	store, ids, path := library(t)
	p, err := music.New(store).Create(t.Context(), owner, "Mix", of(ids, "B", "A"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	if got := titles(t, music.New(open(t, path)), p.ID); !slices.Equal(got, []string{"B", "A"}) {
		t.Errorf("after a restart the playlist holds %v", got)
	}
}

// TestRemoveByIndex is the part #196 said to test hardest: an index is into the
// list the client last saw, and every shape of a list of them has to mean the
// same thing.
func TestRemoveByIndex(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		remove []int
		add    []string
		want   []string
	}{
		{"the first", []int{0}, nil, []string{"B", "C", "D", "E"}},
		{"the last", []int{4}, nil, []string{"A", "B", "C", "D"}},
		{"several, in order", []int{1, 3}, nil, []string{"A", "C", "E"}},
		// Unsorted is the trap: removing one at a time from the front shifts
		// every index after it.
		{"several, backwards", []int{3, 1}, nil, []string{"A", "C", "E"}},
		{"the same one twice", []int{2, 2}, nil, []string{"A", "B", "D", "E"}},
		{"all of them", []int{4, 0, 2, 1, 3}, nil, []string{}},
		// Removals first, then additions: index 4 is E, not what was added.
		{"and an addition", []int{4}, []string{"A"}, []string{"A", "B", "C", "D", "A"}},
		{"an addition alone", nil, []string{"E", "E"}, []string{"A", "B", "C", "D", "E", "E", "E"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store, ids, _ := library(t)
			s := music.New(store)
			p, err := s.Create(t.Context(), owner, "Mix", of(ids, "A", "B", "C", "D", "E"))
			if err != nil {
				t.Fatal(err)
			}

			if err := s.Update(t.Context(), owner, p.ID, music.Edit{Remove: tt.remove, Add: of(ids, tt.add...)}); err != nil {
				t.Fatalf("Update: %v", err)
			}
			if got := titles(t, s, p.ID); !slices.Equal(got, tt.want) {
				t.Errorf("tracks = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestARepeatIsRemovedByPosition: the same track twice is two entries, and an
// index names one of them.
func TestARepeatIsRemovedByPosition(t *testing.T) {
	t.Parallel()
	store, ids, _ := library(t)
	s := music.New(store)
	p, err := s.Create(t.Context(), owner, "Mix", of(ids, "A", "B", "A"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(t.Context(), owner, p.ID, music.Edit{Remove: []int{2}}); err != nil {
		t.Fatal(err)
	}
	if got := titles(t, s, p.ID); !slices.Equal(got, []string{"A", "B"}) {
		t.Errorf("tracks = %v, want the second A gone and the first kept", got)
	}
}

// TestAnEditIsWholeOrNothing covers every refusal: nothing it names has
// changed afterwards, not even the fields that were fine.
func TestAnEditIsWholeOrNothing(t *testing.T) {
	t.Parallel()
	renamed := "Renamed"

	tests := []struct {
		name string
		edit func(ids map[string]int64) music.Edit
		want error
	}{
		{"an index past the end", func(map[string]int64) music.Edit {
			return music.Edit{Name: &renamed, Remove: []int{0, 3}}
		}, music.ErrIndex},
		{"a negative index", func(map[string]int64) music.Edit {
			return music.Edit{Name: &renamed, Remove: []int{-1}}
		}, music.ErrIndex},
		{"a file that is not there", func(map[string]int64) music.Edit {
			return music.Edit{Name: &renamed, Remove: []int{0}, Add: []int64{424242}}
		}, db.ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store, ids, _ := library(t)
			s := music.New(store)
			p, err := s.Create(t.Context(), owner, "Mix", of(ids, "A", "B", "C"))
			if err != nil {
				t.Fatal(err)
			}

			if uerr := s.Update(t.Context(), owner, p.ID, tt.edit(ids)); !errors.Is(uerr, tt.want) {
				t.Fatalf("Update = %v, want %v", uerr, tt.want)
			}
			got, tracks, err := s.Playlist(t.Context(), owner, p.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Name != "Mix" || len(tracks) != 3 {
				t.Errorf("after a refused edit = %q with %d tracks, want it as it was", got.Name, len(tracks))
			}
		})
	}
}

func TestOnlyTracksGoIn(t *testing.T) {
	t.Parallel()
	store, ids, _ := library(t)
	s := music.New(store)

	photo, err := store.PutFile(t.Context(), db.File{
		OwnerID: owner, Path: "photo.jpg", BlobKey: "kp", Size: 1, MTime: time.Now(), ETag: `"p"`, MIMEType: "image/jpeg",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutMedia(t.Context(), db.Media{FileID: photo.ID, Kind: db.KindImage, IndexedAt: time.Now(), Version: 1}); err != nil {
		t.Fatal(err)
	}

	for name, fileIDs := range map[string][]int64{
		"a photograph":             {ids["A"], photo.ID},
		"a file that is not there": {424242},
	} {
		if _, err := s.Create(t.Context(), owner, "Mix", fileIDs); !errors.Is(err, db.ErrNotFound) {
			t.Errorf("Create with %s = %v, want ErrNotFound", name, err)
		}
	}
	if _, err := s.Create(t.Context(), "someone-else", "Theirs", of(ids, "A")); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("Create with another owner's track = %v, want ErrNotFound", err)
	}
	if list, _ := s.Playlists(t.Context(), owner); len(list) != 0 {
		t.Errorf("a refused create left %v", list)
	}
}

func TestFieldsAreEditedAlone(t *testing.T) {
	t.Parallel()
	store, ids, _ := library(t)
	s := music.New(store)
	p, err := s.Create(t.Context(), owner, "Mix", of(ids, "A"))
	if err != nil {
		t.Fatal(err)
	}

	comment, public := "side A", true
	if uerr := s.Update(t.Context(), owner, p.ID, music.Edit{Comment: &comment, Public: &public}); uerr != nil {
		t.Fatal(uerr)
	}
	got, tracks, err := s.Playlist(t.Context(), owner, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Mix" || got.Comment != "side A" || !got.Public || len(tracks) != 1 {
		t.Errorf("after editing two fields = %+v with %d tracks", got, len(tracks))
	}

	// An edit that says nothing changes nothing, and is not an error.
	if err := s.Update(t.Context(), owner, p.ID, music.Edit{}); err != nil {
		t.Errorf("an empty edit = %v", err)
	}
}

func TestReplace(t *testing.T) {
	t.Parallel()
	store, ids, _ := library(t)
	s := music.New(store)
	p, err := s.Create(t.Context(), owner, "Mix", of(ids, "A", "B"))
	if err != nil {
		t.Fatal(err)
	}

	// No name keeps the one it had.
	got, err := s.Replace(t.Context(), owner, p.ID, "", of(ids, "E"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Mix" || got.SongCount != 1 {
		t.Errorf("replaced = %+v", got)
	}
	got, err = s.Replace(t.Context(), owner, p.ID, "Other", of(ids, "C", "D"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Other" {
		t.Errorf("name = %q after a replace that gave one", got.Name)
	}
	if tracks := titles(t, s, p.ID); !slices.Equal(tracks, []string{"C", "D"}) {
		t.Errorf("tracks = %v", tracks)
	}
	if _, err := s.Replace(t.Context(), owner, 424242, "", nil); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("Replace of a missing playlist = %v", err)
	}
}

func TestDelete(t *testing.T) {
	t.Parallel()
	store, ids, _ := library(t)
	s := music.New(store)
	p, err := s.Create(t.Context(), owner, "Mix", of(ids, "A"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(t.Context(), owner, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Playlist(t.Context(), owner, p.ID); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("Playlist after delete = %v", err)
	}
	if err := s.Update(t.Context(), owner, p.ID, music.Edit{Remove: []int{0}}); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("Update after delete = %v", err)
	}
}

// TestConcurrentEditsBothLand is what the lock is for: each of these reads the
// list and writes it back, and without the lock the later write would drop the
// earlier one's addition.
func TestConcurrentEditsBothLand(t *testing.T) {
	t.Parallel()
	store, ids, _ := library(t)
	s := music.New(store)
	p, err := s.Create(t.Context(), owner, "Mix", nil)
	if err != nil {
		t.Fatal(err)
	}

	const writers = 12
	var wg sync.WaitGroup
	for range writers {
		wg.Go(func() {
			if err := s.Update(t.Context(), owner, p.ID, music.Edit{Add: of(ids, "A")}); err != nil {
				t.Errorf("Update: %v", err)
			}
		})
	}
	wg.Wait()

	if got := titles(t, s, p.ID); len(got) != writers {
		t.Errorf("%d of %d additions landed", len(got), writers)
	}
}

// TestAFailureIsReported breaks each call an edit makes, one at a time, and
// checks the error comes back. That nothing is half-written is the
// transaction's, and TestAnEditIsWholeOrNothing is where it is pinned.
func TestAFailureIsReported(t *testing.T) {
	t.Parallel()
	name := "Renamed"

	for _, call := range []string{
		"TrackByFile", "CreatePlaylist", "PlaylistByID", "LockPlaylist", "PlaylistTracks",
		"UpdatePlaylist", "SetPlaylistTracks",
	} {
		t.Run(call, func(t *testing.T) {
			t.Parallel()
			store, ids, _ := library(t)
			p, err := music.New(store).Create(t.Context(), owner, "Mix", of(ids, "A", "B"))
			if err != nil {
				t.Fatal(err)
			}
			s := music.New(dbtest.FailOn(t, store, call))

			attempts := map[string]error{
				"Create": func() error { _, err := s.Create(t.Context(), owner, "New", of(ids, "A")); return err }(),
				"Replace": func() error {
					_, err := s.Replace(t.Context(), owner, p.ID, "Other", of(ids, "A"))
					return err
				}(),
				"Update":   s.Update(t.Context(), owner, p.ID, music.Edit{Name: &name, Remove: []int{0}, Add: of(ids, "C")}),
				"Playlist": func() error { _, _, err := s.Playlist(t.Context(), owner, p.ID); return err }(),
			}
			failed := false
			for _, err := range attempts {
				if errors.Is(err, dbtest.ErrInjected) {
					failed = true
				}
			}
			if !failed {
				t.Errorf("breaking %s reached nothing: %v", call, attempts)
			}

		})
	}
}
