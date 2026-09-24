package dbtest

import (
	"errors"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// RunAnnotations executes the star and rating cases against the repository
// built by newRepo.
func RunAnnotations(t *testing.T, newRepo func(t *testing.T) db.Repo) {
	t.Helper()

	cases := []struct {
		name string
		fn   func(t *testing.T, s db.Repo)
	}{
		{"a star comes back on all three kinds", annotationsRoundTrip},
		{"starring twice keeps the first time", annotationsStarTwice},
		{"unstarring forgets, and is not an error twice", annotationsUnstar},
		{"a rating is set, replaced and taken away", annotationsRating},
		{"a rating off the scale is refused", annotationsRatingBounds},
		{"a subject no constructor builds is refused", annotationsInvalidSubject},
		{"a track's star survives a rename and an overwrite", annotationsFollowTheFile},
		{"a track's star goes with its file", annotationsCascade},
		{"a missing track cannot be starred", annotationsMissingTrack},
		{"a retagged album loses its star", annotationsRetag},
		{"owners do not see each other's stars", annotationsOwnersAreSeparate},
		{"starred lists what is still in the library, newest first", annotationsStarredOrder},
		{"the starred and highest listings filter and order", annotationsAlbumLists},
		{"reading no subjects is not a query", annotationsOfNothing},
		{"a play is counted and the latest one kept", playsCount},
		{"a play is not forgotten with a star or a rating", playsOutliveTheRest},
		{"a play goes with its file", playsCascade},
		{"a missing track cannot be played", playsMissingTrack},
		{"an album's plays are its tracks'", playsAddUpToTheAlbum},
		{"the frequent and recent listings filter and order", playsAlbumLists},
		{"owners do not see each other's plays", playsOwnersAreSeparate},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.fn(t, newRepo(t))
		})
	}
}

// starredAt is a time every driver stores to the millisecond, so a round trip
// is exact.
var starredAt = time.Date(2024, 8, 3, 18, 30, 15, 250_000_000, time.UTC)

func annotationsRoundTrip(t *testing.T, s db.Repo) {
	f := track(t, s, owner, "music/a.flac", song("Boards of Canada", "Boards of Canada", "Geogaddi", "Dandelion", 1))
	subjects := []db.Subject{
		db.TrackSubject(f.ID),
		db.AlbumSubject("Boards of Canada", "Geogaddi"),
		db.ArtistSubject("Boards of Canada"),
	}

	for i, subj := range subjects {
		if err := s.Star(t.Context(), owner, subj, starredAt.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatalf("Star(%+v): %v", subj, err)
		}
	}

	got := annotationsOf(t, s, owner, subjects...)
	for i, subj := range subjects {
		want := starredAt.Add(time.Duration(i) * time.Second)
		if a := got[subj]; !a.Starred.Equal(want) || a.Rating != 0 {
			t.Errorf("%s = %+v, want starred at %v and no rating", subj.Kind, a, want)
		}
	}
}

func annotationsStarTwice(t *testing.T, s db.Repo) {
	album := db.AlbumSubject("Autechre", "Tri Repetae")
	star(t, s, album, starredAt)
	star(t, s, album, starredAt.Add(time.Hour))

	if got := annotationsOf(t, s, owner, album)[album]; !got.Starred.Equal(starredAt) {
		t.Errorf("starred at %v after a second star, want the first time %v", got.Starred, starredAt)
	}
}

func annotationsUnstar(t *testing.T, s db.Repo) {
	f := track(t, s, owner, "music/a.flac", song("Autechre", "Autechre", "Tri Repetae", "Rotar", 1))
	subjects := []db.Subject{db.TrackSubject(f.ID), db.AlbumSubject("Autechre", "Tri Repetae")}

	for _, subj := range subjects {
		star(t, s, subj, starredAt)
		for range 2 {
			if err := s.Unstar(t.Context(), owner, subj); err != nil {
				t.Fatalf("Unstar(%+v): %v", subj, err)
			}
		}
	}
	if got := annotationsOf(t, s, owner, subjects...); len(got) != 0 {
		t.Errorf("after unstarring, AnnotationsOf = %v, want nothing", got)
	}

	// Never starred at all is the same answer.
	if err := s.Unstar(t.Context(), owner, db.ArtistSubject("Nobody")); err != nil {
		t.Errorf("Unstar of something never starred = %v, want nil", err)
	}
}

func annotationsRating(t *testing.T, s db.Repo) {
	f := track(t, s, owner, "music/a.flac", song("Autechre", "Autechre", "Tri Repetae", "Rotar", 1))
	for _, subj := range []db.Subject{db.TrackSubject(f.ID), db.ArtistSubject("Autechre")} {
		rate(t, s, subj, 3)
		rate(t, s, subj, 5)
		if got := annotationsOf(t, s, owner, subj)[subj]; got.Rating != 5 || !got.Starred.IsZero() {
			t.Errorf("%s = %+v, want rated 5 and not starred", subj.Kind, got)
		}

		// A rating taken away leaves the star, and the star taken away
		// leaves nothing.
		star(t, s, subj, starredAt)
		rate(t, s, subj, 0)
		if got := annotationsOf(t, s, owner, subj)[subj]; got.Rating != 0 || !got.Starred.Equal(starredAt) {
			t.Errorf("%s = %+v after clearing the rating, want the star alone", subj.Kind, got)
		}
		if err := s.Unstar(t.Context(), owner, subj); err != nil {
			t.Fatal(err)
		}
		if got := annotationsOf(t, s, owner, subj); len(got) != 0 {
			t.Errorf("%s = %v with neither, want no row", subj.Kind, got)
		}
	}
}

func annotationsRatingBounds(t *testing.T, s db.Repo) {
	for _, rating := range []int{-1, db.MaxRating + 1} {
		err := s.SetRating(t.Context(), owner, db.ArtistSubject("Autechre"), rating)
		if !errors.Is(err, db.ErrInvalidRating) {
			t.Errorf("SetRating(%d) = %v, want ErrInvalidRating", rating, err)
		}
	}
}

func annotationsInvalidSubject(t *testing.T, s db.Repo) {
	for _, subj := range []db.Subject{
		{},
		{Kind: db.SubjectTrack},
		{Kind: db.SubjectAlbum, Artist: "Autechre"},
		{Kind: db.SubjectArtist, Artist: "Autechre", Album: "Tri Repetae"},
		{Kind: "playlist", Artist: "x"},
	} {
		if err := s.Star(t.Context(), owner, subj, starredAt); err == nil {
			t.Errorf("Star(%+v) = nil, want a refusal", subj)
		}
		if err := s.Unstar(t.Context(), owner, subj); err == nil {
			t.Errorf("Unstar(%+v) = nil, want a refusal", subj)
		}
		if err := s.SetRating(t.Context(), owner, subj, 3); err == nil {
			t.Errorf("SetRating(%+v) = nil, want a refusal", subj)
		}
	}
}

// annotationsFollowTheFile is what keying a track on its file id buys: the id
// survives both, so the star does.
func annotationsFollowTheFile(t *testing.T, s db.Repo) {
	f := track(t, s, owner, "music/a.flac", song("Autechre", "Autechre", "Tri Repetae", "Rotar", 1))
	subj := db.TrackSubject(f.ID)
	star(t, s, subj, starredAt)

	if err := s.MoveFile(t.Context(), owner, "music/a.flac", "music/b.flac"); err != nil {
		t.Fatal(err)
	}
	replaced := file("music/b.flac")
	replaced.ETag = `"other"`
	put(t, s, replaced)

	if got := annotationsOf(t, s, owner, subj)[subj]; !got.Starred.Equal(starredAt) {
		t.Errorf("after a rename and an overwrite the track is %+v, want still starred", got)
	}
}

func annotationsCascade(t *testing.T, s db.Repo) {
	f := track(t, s, owner, "music/a.flac", song("Autechre", "Autechre", "Tri Repetae", "Rotar", 1))
	subj := db.TrackSubject(f.ID)
	star(t, s, subj, starredAt)
	rate(t, s, subj, 4)

	if err := s.DeleteFile(t.Context(), owner, "music/a.flac"); err != nil {
		t.Fatal(err)
	}
	if got := annotationsOf(t, s, owner, subj); len(got) != 0 {
		t.Errorf("after the file was deleted AnnotationsOf = %v, want nothing", got)
	}
}

func annotationsMissingTrack(t *testing.T, s db.Repo) {
	err := s.Star(t.Context(), owner, db.TrackSubject(424242), starredAt)
	if !errors.Is(err, db.ErrNotFound) {
		t.Errorf("Star of a file that is not there = %v, want ErrNotFound", err)
	}
}

// annotationsRetag pins the price written on db.Subject: an album is its tags,
// so correcting them makes another album and the star stays with the old name.
func annotationsRetag(t *testing.T, s db.Repo) {
	f := track(t, s, owner, "music/a.flac", song("Autechre", "Autechre", "Tri Repetea", "Rotar", 1))
	star(t, s, db.AlbumSubject("Autechre", "Tri Repetea"), starredAt)

	fixed := song("Autechre", "Autechre", "Tri Repetae", "Rotar", 1)
	fixed.FileID = f.ID
	if err := s.PutMedia(t.Context(), fixed); err != nil {
		t.Fatal(err)
	}

	got := starred(t, s, owner)
	if len(got.Albums) != 0 {
		t.Errorf("after a retag the starred albums are %v, want none", albumNames(got.Albums))
	}
}

func annotationsOwnersAreSeparate(t *testing.T, s db.Repo) {
	mine := track(t, s, owner, "music/a.flac", song("Autechre", "Autechre", "Tri Repetae", "Rotar", 1))
	track(t, s, "someone-else", "music/a.flac", song("Autechre", "Autechre", "Tri Repetae", "Rotar", 1))

	star(t, s, db.TrackSubject(mine.ID), starredAt)
	star(t, s, db.AlbumSubject("Autechre", "Tri Repetae"), starredAt)

	if got := annotationsOf(t, s, "someone-else", db.TrackSubject(mine.ID), db.AlbumSubject("Autechre", "Tri Repetae")); len(got) != 0 {
		t.Errorf("another owner reads %v, want nothing", got)
	}
	theirs := starred(t, s, "someone-else")
	if len(theirs.Albums)+len(theirs.Tracks)+len(theirs.Artists) != 0 {
		t.Errorf("another owner's starred = %+v, want nothing", theirs)
	}
}

func annotationsStarredOrder(t *testing.T, s db.Repo) {
	a := track(t, s, owner, "music/a.flac", song("Autechre", "Autechre", "Tri Repetae", "Rotar", 1))
	b := track(t, s, owner, "music/b.flac", song("Boards of Canada", "Boards of Canada", "Geogaddi", "Dandelion", 1))

	star(t, s, db.TrackSubject(a.ID), starredAt)
	star(t, s, db.TrackSubject(b.ID), starredAt.Add(time.Minute))
	star(t, s, db.AlbumSubject("Autechre", "Tri Repetae"), starredAt.Add(time.Minute))
	star(t, s, db.AlbumSubject("Boards of Canada", "Geogaddi"), starredAt)
	star(t, s, db.ArtistSubject("Autechre"), starredAt)
	// Starred and not in the library: waits, and is not listed.
	star(t, s, db.AlbumSubject("Nobody", "Nothing"), starredAt.Add(time.Hour))
	star(t, s, db.ArtistSubject("Nobody"), starredAt.Add(time.Hour))
	// Rated and not starred is not starred.
	rate(t, s, db.ArtistSubject("Boards of Canada"), 5)

	got := starred(t, s, owner)
	if names := trackTitles(got.Tracks); !equal(names, []string{"Dandelion", "Rotar"}) {
		t.Errorf("starred tracks = %v, want newest first", names)
	}
	if names := albumNames(got.Albums); !equal(names, []string{"Tri Repetae", "Geogaddi"}) {
		t.Errorf("starred albums = %v, want newest first", names)
	}
	if len(got.Albums) > 0 && got.Albums[0].SongCount != 1 {
		t.Errorf("a starred album counts %d songs, want 1: the join must not multiply", got.Albums[0].SongCount)
	}
	if len(got.Artists) != 1 || got.Artists[0].Name != "Autechre" || got.Artists[0].AlbumCount != 1 {
		t.Errorf("starred artists = %+v, want Autechre with one album", got.Artists)
	}
}

func annotationsAlbumLists(t *testing.T, s db.Repo) {
	catalogue(t, s,
		record{artist: "Zomby", album: "Aaron", year: 2011, genre: "Electronic"},
		record{artist: "Autechre", album: "Zeta", year: 1994, genre: "Electronic"},
		record{artist: "Móveis", album: "Meia", year: 2003, genre: "Rock"},
		record{artist: "Burial", album: "Untrue", year: 2007, genre: "Electronic"},
	)
	star(t, s, db.AlbumSubject("Zomby", "Aaron"), starredAt)
	star(t, s, db.AlbumSubject("Móveis", "Meia"), starredAt.Add(time.Minute))
	rate(t, s, db.AlbumSubject("Autechre", "Zeta"), 2)
	rate(t, s, db.AlbumSubject("Zomby", "Aaron"), 4)
	rate(t, s, db.AlbumSubject("Burial", "Untrue"), 2)

	page := db.Page{Limit: 10}
	tests := []struct {
		f    db.AlbumFilter
		want []string
	}{
		{db.AlbumFilter{Order: db.AlbumsStarred, Page: page}, []string{"Meia", "Aaron"}},
		// Ties on the rating fall back to the artist, as every order does.
		{db.AlbumFilter{Order: db.AlbumsHighest, Page: page}, []string{"Aaron", "Zeta", "Untrue"}},
		{db.AlbumFilter{Order: db.AlbumsHighest, Page: db.Page{Limit: 2, Offset: 1}}, []string{"Zeta", "Untrue"}},
		{db.AlbumFilter{Order: db.AlbumsStarred, Genre: "Rock", Page: page}, []string{"Meia"}},
	}
	for _, tt := range tests {
		got, err := s.AlbumList(t.Context(), owner, tt.f)
		if err != nil {
			t.Fatalf("AlbumList(%+v): %v", tt.f, err)
		}
		if names := albumNames(got); !equal(names, tt.want) {
			t.Errorf("AlbumList(%+v) = %v, want %v", tt.f, names, tt.want)
		}
	}
}

func annotationsOfNothing(t *testing.T, s db.Repo) {
	got, err := s.AnnotationsOf(t.Context(), owner, nil)
	if err != nil || len(got) != 0 {
		t.Errorf("AnnotationsOf(nil) = %v, %v, want an empty map", got, err)
	}
}

func star(t *testing.T, s db.Repo, subj db.Subject, at time.Time) {
	t.Helper()
	if err := s.Star(t.Context(), owner, subj, at); err != nil {
		t.Fatalf("Star(%+v): %v", subj, err)
	}
}

func rate(t *testing.T, s db.Repo, subj db.Subject, rating int) {
	t.Helper()
	if err := s.SetRating(t.Context(), owner, subj, rating); err != nil {
		t.Fatalf("SetRating(%+v, %d): %v", subj, rating, err)
	}
}

func annotationsOf(t *testing.T, s db.Repo, ownerID string, subjects ...db.Subject) map[db.Subject]db.Annotation {
	t.Helper()
	got, err := s.AnnotationsOf(t.Context(), ownerID, subjects)
	if err != nil {
		t.Fatalf("AnnotationsOf: %v", err)
	}
	return got
}

func starred(t *testing.T, s db.Repo, ownerID string) db.StarredItems {
	t.Helper()
	got, err := s.Starred(t.Context(), ownerID)
	if err != nil {
		t.Fatalf("Starred: %v", err)
	}
	return got
}

func play(t *testing.T, s db.Repo, f db.File, at time.Time) {
	t.Helper()
	if err := s.RecordPlay(t.Context(), owner, f.ID, at); err != nil {
		t.Fatalf("RecordPlay(%d): %v", f.ID, err)
	}
}

func playsCount(t *testing.T, s db.Repo) {
	f := track(t, s, owner, "music/a.flac", song("Autechre", "Autechre", "Tri Repetae", "Rotar", 1))
	subj := db.TrackSubject(f.ID)

	play(t, s, f, starredAt)
	play(t, s, f, starredAt.Add(time.Hour))
	// Reported late, as a client syncing an offline session does: it counts,
	// and the track is no less recent for it.
	play(t, s, f, starredAt.Add(-time.Hour))

	got := annotationsOf(t, s, owner, subj)[subj]
	if got.PlayCount != 3 || !got.Played.Equal(starredAt.Add(time.Hour)) {
		t.Errorf("after three plays = %+v, want 3 and the latest time", got)
	}
	if !got.Starred.IsZero() || got.Rating != 0 {
		t.Errorf("a play starred or rated the track: %+v", got)
	}
}

// playsOutliveTheRest is the prune: a row that loses its star and its rating
// still holds its plays, and must not be deleted for having neither.
func playsOutliveTheRest(t *testing.T, s db.Repo) {
	f := track(t, s, owner, "music/a.flac", song("Autechre", "Autechre", "Tri Repetae", "Rotar", 1))
	subj := db.TrackSubject(f.ID)

	star(t, s, subj, starredAt)
	rate(t, s, subj, 4)
	play(t, s, f, starredAt)
	if err := s.Unstar(t.Context(), owner, subj); err != nil {
		t.Fatal(err)
	}
	rate(t, s, subj, 0)

	if got := annotationsOf(t, s, owner, subj)[subj]; got.PlayCount != 1 {
		t.Errorf("after the star and the rating went = %+v, want the play kept", got)
	}
}

func playsCascade(t *testing.T, s db.Repo) {
	f := track(t, s, owner, "music/a.flac", song("Autechre", "Autechre", "Tri Repetae", "Rotar", 1))
	play(t, s, f, starredAt)

	if err := s.DeleteFile(t.Context(), owner, "music/a.flac"); err != nil {
		t.Fatal(err)
	}
	if got := annotationsOf(t, s, owner, db.TrackSubject(f.ID)); len(got) != 0 {
		t.Errorf("after the file was deleted AnnotationsOf = %v, want nothing", got)
	}
}

func playsMissingTrack(t *testing.T, s db.Repo) {
	if err := s.RecordPlay(t.Context(), owner, 424242, starredAt); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("RecordPlay of a file that is not there = %v, want ErrNotFound", err)
	}
	if err := s.RecordPlay(t.Context(), owner, 0, starredAt); err == nil {
		t.Error("RecordPlay(0) = nil, want a refusal")
	}
}

func playsAddUpToTheAlbum(t *testing.T, s db.Repo) {
	a := track(t, s, owner, "music/a.flac", song("Autechre", "Autechre", "Tri Repetae", "Rotar", 1))
	b := track(t, s, owner, "music/b.flac", song("Autechre", "Autechre", "Tri Repetae", "Stud", 2))
	track(t, s, owner, "music/c.flac", song("Autechre", "Autechre", "Tri Repetae", "Eutow", 3))
	album := db.AlbumSubject("Autechre", "Tri Repetae")
	star(t, s, album, starredAt)

	play(t, s, a, starredAt)
	play(t, s, a, starredAt.Add(time.Minute))
	play(t, s, b, starredAt.Add(time.Hour))

	got := annotationsOf(t, s, owner, album, db.ArtistSubject("Autechre"))
	if a := got[album]; a.PlayCount != 3 || !a.Played.Equal(starredAt.Add(time.Hour)) || !a.Starred.Equal(starredAt) {
		t.Errorf("the album = %+v, want its star, three plays and the latest time", a)
	}
	if a, ok := got[db.ArtistSubject("Autechre")]; ok {
		t.Errorf("the artist = %+v, want nothing: an artist has no plays", a)
	}

	// Played and never starred is still answered.
	other := track(t, s, owner, "music/d.flac", song("Burial", "Burial", "Untrue", "Archangel", 1))
	play(t, s, other, starredAt)
	untrue := db.AlbumSubject("Burial", "Untrue")
	if got := annotationsOf(t, s, owner, untrue)[untrue]; got.PlayCount != 1 {
		t.Errorf("an album played and not starred = %+v, want one play", got)
	}
}

func playsAlbumLists(t *testing.T, s db.Repo) {
	catalogue(t, s,
		record{artist: "Zomby", album: "Aaron", year: 2011, genre: "Electronic"},
		record{artist: "Autechre", album: "Zeta", year: 1994, genre: "Electronic"},
		record{artist: "Móveis", album: "Meia", year: 2003, genre: "Rock"},
		record{artist: "Burial", album: "Untrue", year: 2007, genre: "Electronic"},
	)
	// A second track on Aaron nobody plays, which must still be in its count.
	track(t, s, owner, "music/aaron-2.flac", song("Zomby", "Zomby", "Aaron", "Second", 2))

	tracks := func(album string) db.File {
		t.Helper()
		got, err := s.AlbumList(t.Context(), owner, db.AlbumFilter{Order: db.AlbumsByName, Page: db.Page{Limit: 10}})
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range got {
			if a.Name == album {
				list, err := s.Tracks(t.Context(), owner, a.Artist, a.Name)
				if err != nil {
					t.Fatal(err)
				}
				return list[0].File
			}
		}
		t.Fatalf("no album %q", album)
		return db.File{}
	}
	aaron, zeta, meia := tracks("Aaron"), tracks("Zeta"), tracks("Meia")
	play(t, s, aaron, starredAt)
	play(t, s, zeta, starredAt.Add(time.Hour))
	play(t, s, zeta, starredAt.Add(2*time.Hour))
	play(t, s, meia, starredAt.Add(3*time.Hour))
	play(t, s, meia, starredAt.Add(-time.Hour))

	page := db.Page{Limit: 10}
	tests := []struct {
		f    db.AlbumFilter
		want []string
	}{
		// Ties on the count fall back to the artist, as every order does.
		{db.AlbumFilter{Order: db.AlbumsFrequent, Page: page}, []string{"Zeta", "Meia", "Aaron"}},
		{db.AlbumFilter{Order: db.AlbumsRecent, Page: page}, []string{"Meia", "Zeta", "Aaron"}},
		{db.AlbumFilter{Order: db.AlbumsRecent, Page: db.Page{Limit: 1, Offset: 1}}, []string{"Zeta"}},
		{db.AlbumFilter{Order: db.AlbumsFrequent, Genre: "Rock", Page: page}, []string{"Meia"}},
	}
	for _, tt := range tests {
		got, err := s.AlbumList(t.Context(), owner, tt.f)
		if err != nil {
			t.Fatalf("AlbumList(%+v): %v", tt.f, err)
		}
		if names := albumNames(got); !equal(names, tt.want) {
			t.Errorf("AlbumList(%+v) = %v, want %v", tt.f, names, tt.want)
		}
		for _, a := range got {
			if a.Name == "Aaron" && a.SongCount != 2 {
				t.Errorf("Aaron counts %d songs in %s, want 2: the unplayed track is still on it", a.SongCount, tt.f.Order)
			}
		}
	}
}

func playsOwnersAreSeparate(t *testing.T, s db.Repo) {
	f := track(t, s, owner, "music/a.flac", song("Autechre", "Autechre", "Tri Repetae", "Rotar", 1))
	play(t, s, f, starredAt)

	got := annotationsOf(t, s, "someone-else", db.TrackSubject(f.ID), db.AlbumSubject("Autechre", "Tri Repetae"))
	if len(got) != 0 {
		t.Errorf("another owner reads %v, want nothing", got)
	}
	recent, err := s.AlbumList(t.Context(), "someone-else", db.AlbumFilter{Order: db.AlbumsRecent, Page: db.Page{Limit: 10}})
	if err != nil || len(recent) != 0 {
		t.Errorf("another owner's recent = %v, %v, want nothing", albumNames(recent), err)
	}
}
