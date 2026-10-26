package dbtest

import (
	"errors"
	"testing"
	"time"

	"github.com/stratus-private-cloud/stratus-backend/internal/db"
)

// RunLookups executes the cases for what outside sources said about the
// library (#217) against the repository built by newRepo.
//
// A db.Repo because an album is a pairing of tags over tracks, and a track is a
// file and its metadata: the cases have to write both halves before there is
// an album to ask about.
func RunLookups(t *testing.T, newRepo func(t *testing.T) db.Repo) {
	t.Helper()

	cases := []struct {
		name string
		fn   func(t *testing.T, s db.Repo)
	}{
		{"an album with no row is pending, with the ids its tags carry", lookupsAlbumPending},
		{"an album lookup survives a round trip", lookupsAlbumRoundTrip},
		{"an answer that was found settles the album", lookupsAlbumSettled},
		{"a row says when to ask again", lookupsAlbumRetry},
		{"forgetting a cover puts the album back in the queue", lookupsForgetCover},
		{"a retagged album's row is pruned and a live one is not", lookupsPrune},
		{"an album lookup that is not there is ErrNotFound", lookupsAlbumMissing},
		{"a better search asks again what found nothing", lookupsSearchVersion},
		{"lyrics are pending for a track with a title and an artist", lookupsLyricsPending},
		{"lyrics survive a round trip", lookupsLyricsRoundTrip},
		{"lyrics say when to ask again", lookupsLyricsRetry},
		{"replacing a file puts its lyrics back in the queue", lookupsLyricsOverwrite},
		{"lyrics go with the file", lookupsLyricsCascade},
		{"lyrics for a file that is not there are not found", lookupsLyricsMissingFile},
		{"reading no lyrics is not a query", lookupsLyricsOfNothing},
		{"the counts say where the library stands", lookupsCounts},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.fn(t, newRepo(t))
		})
	}
}

// release is the MusicBrainz ids a Picard-tagged track carries.
const (
	releaseGroup = "4cf8ba42-5d0b-3a4d-8e8f-4c8f2f3d1a1b"
	release      = "e1a8b6a4-9a1c-4ed2-9a7f-5b3d7c0e2f44"
)

func lookupsAlbumPending(t *testing.T, s db.Repo) {
	// Two tracks of one album, only one of which is tagged with the ids: the
	// album is one candidate and the ids come with it.
	tagged := song("Autechre", "Autechre", "Amber", "Foil", 1)
	tagged.MBReleaseGroup, tagged.MBRelease = releaseGroup, release
	track(t, s, owner, "music/amber/01.flac", tagged)
	track(t, s, owner, "music/amber/02.flac", song("Autechre", "Autechre", "Amber", "Montreal", 2))
	// An album with no album tag is not an album, and is not asked about.
	untitled := song("", "Someone", "", "Untitled", 1)
	track(t, s, owner, "music/loose.flac", untitled)

	pending, err := s.PendingAlbumLookups(t.Context(), 1, time.Now(), 10)
	if err != nil {
		t.Fatalf("PendingAlbumLookups: %v", err)
	}
	want := []db.AlbumCandidate{{Owner: owner, Artist: "Autechre", Album: "Amber", ReleaseGroup: releaseGroup, Release: release}}
	if len(pending) != 1 || pending[0] != want[0] {
		t.Errorf("PendingAlbumLookups = %+v, want %+v", pending, want)
	}
}

func lookupsAlbumRoundTrip(t *testing.T, s db.Repo) {
	track(t, s, owner, "music/amber/01.flac", song("Autechre", "Autechre", "Amber", "Foil", 1))

	want := db.AlbumLookup{
		Owner: owner, Artist: "Autechre", Album: "Amber",
		LookedUpAt:    time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC),
		RetryAt:       time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC),
		ReleaseGroup:  releaseGroup,
		CoverBlob:     "blobs/music-amber-01.flac",
		Error:         "the archive had no front cover",
		SearchVersion: 3,
	}
	if err := s.PutAlbumLookup(t.Context(), want); err != nil {
		t.Fatalf("PutAlbumLookup: %v", err)
	}
	got, err := s.AlbumLookup(t.Context(), owner, "Autechre", "Amber")
	if err != nil {
		t.Fatalf("AlbumLookup: %v", err)
	}
	if got != want {
		t.Errorf("AlbumLookup = %+v, want %+v", got, want)
	}

	// Written again, the row is replaced rather than duplicated or refused.
	want.CoverBlob, want.Error, want.RetryAt = "", "", time.Time{}
	if err = s.PutAlbumLookup(t.Context(), want); err != nil {
		t.Fatalf("PutAlbumLookup again: %v", err)
	}
	if got, err = s.AlbumLookup(t.Context(), owner, "Autechre", "Amber"); err != nil || got != want {
		t.Errorf("AlbumLookup after a second put = %+v, %v; want %+v", got, err, want)
	}
}

func lookupsAlbumSettled(t *testing.T, s db.Repo) {
	track(t, s, owner, "music/amber/01.flac", song("Autechre", "Autechre", "Amber", "Foil", 1))
	if err := s.PutAlbumLookup(t.Context(), db.AlbumLookup{
		Owner: owner, Artist: "Autechre", Album: "Amber", LookedUpAt: time.Now(), ReleaseGroup: releaseGroup,
	}); err != nil {
		t.Fatal(err)
	}
	pending, err := s.PendingAlbumLookups(t.Context(), 1, time.Now().Add(365*24*time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Errorf("a settled album is still pending: %+v", pending)
	}
}

func lookupsAlbumRetry(t *testing.T, s db.Repo) {
	track(t, s, owner, "music/amber/01.flac", song("Autechre", "Autechre", "Amber", "Foil", 1))
	now := time.Now()
	// Written by this search, so only the time decides.
	if err := s.PutAlbumLookup(t.Context(), db.AlbumLookup{
		Owner: owner, Artist: "Autechre", Album: "Amber", LookedUpAt: now, RetryAt: now.Add(time.Hour), SearchVersion: 1,
	}); err != nil {
		t.Fatal(err)
	}

	before, err := s.PendingAlbumLookups(t.Context(), 1, now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 0 {
		t.Errorf("pending before its time: %+v", before)
	}
	after, err := s.PendingAlbumLookups(t.Context(), 1, now.Add(2*time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 {
		t.Errorf("not pending when its time came: %+v", after)
	}
}

func lookupsForgetCover(t *testing.T, s db.Repo) {
	track(t, s, owner, "music/amber/01.flac", song("Autechre", "Autechre", "Amber", "Foil", 1))
	now := time.Now()
	if err := s.PutAlbumLookup(t.Context(), db.AlbumLookup{
		Owner: owner, Artist: "Autechre", Album: "Amber", LookedUpAt: now,
		ReleaseGroup: releaseGroup, CoverBlob: "blobs/gone",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.ForgetCover(t.Context(), owner, "Autechre", "Amber", now); err != nil {
		t.Fatalf("ForgetCover: %v", err)
	}

	row, err := s.AlbumLookup(t.Context(), owner, "Autechre", "Amber")
	if err != nil {
		t.Fatal(err)
	}
	// The identity is kept: it was the picture that went, not the answer.
	if row.CoverBlob != "" || row.ReleaseGroup != releaseGroup {
		t.Errorf("after ForgetCover the row is %+v", row)
	}
	pending, err := s.PendingAlbumLookups(t.Context(), 1, now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Errorf("the album is not pending again: %+v", pending)
	}
	// And for a row that is not there, nothing happens.
	if err := s.ForgetCover(t.Context(), owner, "Nobody", "Nothing", now); err != nil {
		t.Errorf("ForgetCover of a missing row: %v", err)
	}
}

func lookupsPrune(t *testing.T, s db.Repo) {
	track(t, s, owner, "music/amber/01.flac", song("Autechre", "Autechre", "Amber", "Foil", 1))
	for _, album := range []string{"Amber", "Incunabula"} {
		if err := s.PutAlbumLookup(t.Context(), db.AlbumLookup{
			Owner: owner, Artist: "Autechre", Album: album, LookedUpAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}

	n, err := s.PruneAlbumLookups(t.Context())
	if err != nil {
		t.Fatalf("PruneAlbumLookups: %v", err)
	}
	if n != 1 {
		t.Errorf("pruned %d rows, want the one album the library does not have", n)
	}
	if _, err := s.AlbumLookup(t.Context(), owner, "Autechre", "Amber"); err != nil {
		t.Errorf("the live album's row went too: %v", err)
	}
	if _, err := s.AlbumLookup(t.Context(), owner, "Autechre", "Incunabula"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("the dead album's row is still there: %v", err)
	}
}

func lookupsAlbumMissing(t *testing.T, s db.Repo) {
	if _, err := s.AlbumLookup(t.Context(), owner, "Autechre", "Amber"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("AlbumLookup of nothing = %v, want ErrNotFound", err)
	}
}

func lookupsLyricsPending(t *testing.T, s db.Repo) {
	sung := track(t, s, owner, "music/amber/01.flac", song("Autechre", "Autechre", "Amber", "Foil", 1))
	// No title: nothing to ask a source for.
	track(t, s, owner, "music/untitled.flac", song("Autechre", "Autechre", "Amber", "", 2))
	// Not audio at all.
	put(t, s, file("photos/IMG_0001.jpg"))

	pending, err := s.PendingLyrics(t.Context(), time.Now(), 10)
	if err != nil {
		t.Fatalf("PendingLyrics: %v", err)
	}
	if len(pending) != 1 || pending[0].File.ID != sung.ID || pending[0].Media.Title != "Foil" {
		t.Errorf("PendingLyrics = %+v, want the one track with a title", pending)
	}
}

func lookupsLyricsRoundTrip(t *testing.T, s db.Repo) {
	f := track(t, s, owner, "music/amber/01.flac", song("Autechre", "Autechre", "Amber", "Foil", 1))

	want := db.Lyrics{
		FileID:    f.ID,
		ETag:      f.ETag,
		FetchedAt: time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC),
		RetryAt:   time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC),
		Plain:     "a line\nanother line",
		Synced:    "[00:01.00] a line\n[00:05.50] another line",
		Error:     "nothing yet",
	}
	if err := s.PutLyrics(t.Context(), want); err != nil {
		t.Fatalf("PutLyrics: %v", err)
	}
	got, err := s.LyricsOf(t.Context(), []int64{f.ID, f.ID + 1000})
	if err != nil {
		t.Fatalf("LyricsOf: %v", err)
	}
	if len(got) != 1 || got[f.ID] != want {
		t.Errorf("LyricsOf = %+v, want %+v", got, want)
	}
	if !got[f.ID].Found() {
		t.Error("words were stored and Found says no")
	}

	// Replaced on a second write, and an instrumental is an answer too.
	want = db.Lyrics{FileID: f.ID, ETag: f.ETag, FetchedAt: want.FetchedAt, Instrumental: true}
	if err = s.PutLyrics(t.Context(), want); err != nil {
		t.Fatalf("PutLyrics again: %v", err)
	}
	got, err = s.LyricsOf(t.Context(), []int64{f.ID})
	if err != nil || got[f.ID] != want {
		t.Errorf("LyricsOf after a second put = %+v, %v; want %+v", got, err, want)
	}
	if !got[f.ID].Found() {
		t.Error("an instrumental is an answer, and Found says no")
	}
	pending, err := s.PendingLyrics(t.Context(), time.Now(), 10)
	if err != nil || len(pending) != 0 {
		t.Errorf("an answered track is still pending: %+v, %v", pending, err)
	}
}

func lookupsLyricsRetry(t *testing.T, s db.Repo) {
	f := track(t, s, owner, "music/amber/01.flac", song("Autechre", "Autechre", "Amber", "Foil", 1))
	now := time.Now()
	if err := s.PutLyrics(t.Context(), db.Lyrics{FileID: f.ID, ETag: f.ETag, FetchedAt: now, RetryAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	before, err := s.PendingLyrics(t.Context(), now, 10)
	if err != nil || len(before) != 0 {
		t.Errorf("pending before its time: %+v, %v", before, err)
	}
	after, err := s.PendingLyrics(t.Context(), now.Add(2*time.Hour), 10)
	if err != nil || len(after) != 1 {
		t.Errorf("not pending when its time came: %+v, %v", after, err)
	}
}

func lookupsLyricsOverwrite(t *testing.T, s db.Repo) {
	first := track(t, s, owner, "music/amber/01.flac", song("Autechre", "Autechre", "Amber", "Foil", 1))
	if err := s.PutLyrics(t.Context(), db.Lyrics{FileID: first.ID, ETag: first.ETag, FetchedAt: time.Now(), Plain: "words"}); err != nil {
		t.Fatal(err)
	}

	replacement := file("music/amber/01.flac")
	replacement.ETag = `"a different recording"`
	replacement.BlobKey = "blobs/amber-01-2"
	second := put(t, s, replacement)
	if second.ID != first.ID {
		t.Fatalf("the overwrite made a new row (%d, was %d)", second.ID, first.ID)
	}

	pending, err := s.PendingLyrics(t.Context(), time.Now(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].File.ID != first.ID {
		t.Errorf("PendingLyrics = %+v, want the overwritten file back", pending)
	}
}

func lookupsLyricsCascade(t *testing.T, s db.Repo) {
	f := track(t, s, owner, "music/amber/01.flac", song("Autechre", "Autechre", "Amber", "Foil", 1))
	if err := s.PutLyrics(t.Context(), db.Lyrics{FileID: f.ID, ETag: f.ETag, FetchedAt: time.Now(), Plain: "words"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteFile(t.Context(), owner, f.Path); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	got, err := s.LyricsOf(t.Context(), []int64{f.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("the lyrics outlived their file: %+v", got)
	}
}

func lookupsLyricsMissingFile(t *testing.T, s db.Repo) {
	err := s.PutLyrics(t.Context(), db.Lyrics{FileID: 424242, FetchedAt: time.Now(), Plain: "words"})
	if !errors.Is(err, db.ErrNotFound) {
		t.Errorf("PutLyrics for a missing file = %v, want ErrNotFound", err)
	}
}

func lookupsLyricsOfNothing(t *testing.T, s db.Repo) {
	got, err := s.LyricsOf(t.Context(), nil)
	if err != nil || len(got) != 0 {
		t.Errorf("LyricsOf(nil) = %+v, %v", got, err)
	}
}

func lookupsCounts(t *testing.T, s db.Repo) {
	foil := track(t, s, owner, "music/amber/01.flac", song("Autechre", "Autechre", "Amber", "Foil", 1))
	track(t, s, owner, "music/amber/02.flac", song("Autechre", "Autechre", "Amber", "Montreal", 2))
	track(t, s, owner, "music/geogaddi/01.flac", song("Boards of Canada", "Boards of Canada", "Geogaddi", "Dandelion", 1))
	// A track with no title is not counted: nothing could be asked about it.
	track(t, s, owner, "music/untitled.flac", song("Autechre", "Autechre", "Amber", "", 3))
	now := time.Now()

	if err := s.PutAlbumLookup(t.Context(), db.AlbumLookup{
		Owner: owner, Artist: "Autechre", Album: "Amber", LookedUpAt: now, ReleaseGroup: releaseGroup, CoverBlob: foil.BlobKey,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutLyrics(t.Context(), db.Lyrics{FileID: foil.ID, ETag: foil.ETag, FetchedAt: now, Plain: "words"}); err != nil {
		t.Fatal(err)
	}

	got, err := s.LookupCounts(t.Context())
	if err != nil {
		t.Fatalf("LookupCounts: %v", err)
	}
	want := db.LookupCounts{Albums: 2, AlbumsTried: 1, Identified: 1, Covers: 1, Tracks: 3, TracksTried: 1, Lyrics: 1}
	if got != want {
		t.Errorf("LookupCounts = %+v, want %+v", got, want)
	}
}

func lookupsSearchVersion(t *testing.T, s db.Repo) {
	track(t, s, owner, "music/amber/01.flac", song("Autechre", "Autechre", "Amber", "Foil", 1))
	track(t, s, owner, "music/geogaddi/01.flac", song("Boards of Canada", "Boards of Canada", "Geogaddi", "Dandelion", 1))
	now := time.Now()
	// One album found nothing under search 1, the other found its id.
	for _, row := range []db.AlbumLookup{
		{Owner: owner, Artist: "Autechre", Album: "Amber", LookedUpAt: now, RetryAt: now.Add(720 * time.Hour), SearchVersion: 1},
		{Owner: owner, Artist: "Boards of Canada", Album: "Geogaddi", LookedUpAt: now, ReleaseGroup: releaseGroup, SearchVersion: 1},
	} {
		if err := s.PutAlbumLookup(t.Context(), row); err != nil {
			t.Fatal(err)
		}
	}

	same, err := s.PendingAlbumLookups(t.Context(), 1, now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(same) != 0 {
		t.Errorf("pending under the same search: %+v", same)
	}
	better, err := s.PendingAlbumLookups(t.Context(), 2, now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(better) != 1 || better[0].Album != "Amber" {
		t.Errorf("pending under a better search = %+v, want only the album that found nothing", better)
	}
}
