package dbtest

import (
	"errors"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// RunMedia executes the metadata cases against the repository built by newRepo.
//
// It takes a db.Repo rather than a db.MediaIndex because the cases have to
// create the files the metadata hangs off: the two halves are only meaningful
// together, which is what the foreign key says too.
func RunMedia(t *testing.T, newRepo func(t *testing.T) db.Repo) {
	t.Helper()

	cases := []struct {
		name string
		fn   func(t *testing.T, s db.Repo)
	}{
		{"metadata survives a round trip", mediaRoundTrip},
		{"every column of a media row survives a round trip", mediaEveryColumn},
		{"both media times survive a round trip", mediaTimeRoundTrip},
		{"the fields another kind does not use stay empty", mediaSparse},
		{"pending skips what is indexed", mediaPending},
		{"a failed extraction is not retried", mediaFailureIsFinal},
		{"a newer extractor puts everything back in the queue", mediaVersionBump},
		{"deleting the file deletes its metadata", mediaCascade},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.fn(t, newRepo(t))
		})
	}
}

func mediaRoundTrip(t *testing.T, s db.Repo) {
	f := put(t, s, file("photos/IMG_0001.jpg"))

	want := db.Media{
		FileID:      f.ID,
		Kind:        db.KindImage,
		IndexedAt:   time.Now(),
		Version:     3,
		TakenAt:     time.Date(2024, 6, 1, 12, 30, 15, 0, time.UTC),
		Width:       4032,
		Height:      3024,
		Orientation: 6,
		GPS:         &db.GPS{Latitude: 41.3874, Longitude: 2.1686},
		Camera:      "Apple iPhone 15 Pro",
	}
	if err := s.PutMedia(t.Context(), want); err != nil {
		t.Fatalf("PutMedia: %v", err)
	}

	got, err := s.MediaByFile(t.Context(), f.ID)
	if err != nil {
		t.Fatalf("MediaByFile: %v", err)
	}
	if got.Kind != want.Kind || got.Version != want.Version || got.Camera != want.Camera {
		t.Errorf("got %+v", got)
	}
	if !got.TakenAt.Equal(want.TakenAt) {
		t.Errorf("TakenAt = %v, want %v", got.TakenAt, want.TakenAt)
	}
	if got.Width != want.Width || got.Height != want.Height || got.Orientation != want.Orientation {
		t.Errorf("dimensions = %dx%d orientation %d", got.Width, got.Height, got.Orientation)
	}
	if got.GPS == nil || got.GPS.Latitude != want.GPS.Latitude || got.GPS.Longitude != want.GPS.Longitude {
		t.Errorf("GPS = %+v, want %+v", got.GPS, want.GPS)
	}
	if !got.Indexed() {
		t.Error("a successful extraction reports itself as failed")
	}
}

// mediaEveryColumn is fileEveryColumn for the wide table, where it matters
// more: media has 21 columns, four of them were written by no case or asserted
// by no case, and disc_no was in neither camp because nothing wrote it at all.
//
// Nothing is exempt here. Unlike a file row, a media row can legally carry
// every column at once -- that is what one wide table means -- so the fixture
// carries all of them and assertEveryFieldSet keeps it that way.
func mediaEveryColumn(t *testing.T, s db.Repo) {
	f := put(t, s, file("videos/every-column.mkv"))

	// Normalize because PutMedia does: the times come back at TimePrecision.
	want := media(f.ID).Normalize()
	assertEveryFieldSet(t, want)

	if err := s.PutMedia(t.Context(), want); err != nil {
		t.Fatalf("PutMedia: %v", err)
	}

	got, err := s.MediaByFile(t.Context(), f.ID)
	if err != nil {
		t.Fatalf("MediaByFile: %v", err)
	}
	compareFields(t, got, want)
}

// media builds a row with every column set, which is the one place a new column
// has to be written for the round trip above to cover it.
//
// It is deliberately implausible -- a camera and an album on the same row, and
// an Error beside a successful probe -- because it exercises columns and not
// meaning. The cases that care about meaning are mediaRoundTrip and mediaSparse.
func media(fileID int64) db.Media {
	return db.Media{
		FileID:      fileID,
		Kind:        db.KindVideo,
		IndexedAt:   time.Date(2024, 7, 2, 9, 15, 0, 0, time.UTC),
		Version:     7,
		Error:       "truncated at the last frame",
		TakenAt:     time.Date(2023, 5, 4, 18, 45, 30, 0, time.UTC),
		Width:       3840,
		Height:      2160,
		Orientation: 3,
		GPS:         &db.GPS{Latitude: 41.3874, Longitude: 2.1686},
		Camera:      "Sony ILCE-7M4",
		DurationMS:  742_000,
		Codec:       "hevc",
		Artist:      "Boards of Canada",
		Album:       "Music Has the Right to Children",
		Title:       "Roygbiv",
		TrackNo:     10,
		DiscNo:      2,
		Year:        1998,
		Genre:       "Electronic",
	}
}

// mediaTimeRoundTrip is timeRoundTrip for the other table, and a regression
// test with a name: TakenAt was truncated to TimePrecision by both drivers but
// IndexedAt by neither, so the same write came back with milliseconds from
// SQLite and microseconds from Postgres.
func mediaTimeRoundTrip(t *testing.T, s db.Repo) {
	f := put(t, s, file("photos/clock.jpg"))

	// Deliberately awkward in the same way: a non-UTC zone and sub-millisecond
	// precision, which is what time.Now and an EXIF reader actually carry.
	zone := time.FixedZone("CEST", 2*60*60)
	indexed := time.Date(2024, 6, 1, 12, 30, 15, 123_456_789, zone)
	taken := time.Date(2023, 9, 14, 8, 5, 1, 987_654_321, zone)

	err := s.PutMedia(t.Context(), db.Media{
		FileID:    f.ID,
		Kind:      db.KindImage,
		IndexedAt: indexed,
		Version:   1,
		TakenAt:   taken,
	})
	if err != nil {
		t.Fatalf("PutMedia: %v", err)
	}

	got, err := s.MediaByFile(t.Context(), f.ID)
	if err != nil {
		t.Fatalf("MediaByFile: %v", err)
	}
	if want := indexed.UTC().Truncate(db.TimePrecision); !got.IndexedAt.Equal(want) {
		t.Errorf("IndexedAt = %v, want %v", got.IndexedAt, want)
	}
	if want := taken.UTC().Truncate(db.TimePrecision); !got.TakenAt.Equal(want) {
		t.Errorf("TakenAt = %v, want %v", got.TakenAt, want)
	}
}

// mediaSparse is the price of one wide table, and the assertion that keeps it
// honest: a track leaves every photo field alone, and an unknown position is
// nil rather than a point in the Atlantic.
func mediaSparse(t *testing.T, s db.Repo) {
	f := put(t, s, file("music/01 Hunter.flac"))

	track := db.Media{
		FileID:     f.ID,
		Kind:       db.KindAudio,
		IndexedAt:  time.Now(),
		DurationMS: 254_000,
		Codec:      "flac",
		Artist:     "Björk",
		Album:      "Homogenic",
		Title:      "Hunter",
		TrackNo:    1,
		Year:       1997,
		Genre:      "Electronic",
	}
	if err := s.PutMedia(t.Context(), track); err != nil {
		t.Fatal(err)
	}

	got, err := s.MediaByFile(t.Context(), f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Artist != "Björk" || got.Album != "Homogenic" || got.TrackNo != 1 || got.Year != 1997 {
		t.Errorf("got %+v", got)
	}
	if got.DurationMS != 254_000 {
		t.Errorf("DurationMS = %d", got.DurationMS)
	}
	if got.GPS != nil {
		t.Errorf("GPS = %+v, want nil for a file that has none", got.GPS)
	}
	if !got.TakenAt.IsZero() || got.Width != 0 || got.Camera != "" {
		t.Errorf("photo fields are set on a track: %+v", got)
	}
}

func mediaPending(t *testing.T, s db.Repo) {
	if _, err := s.CreateDir(t.Context(), owner, "album"); err != nil {
		t.Fatal(err)
	}
	one := put(t, s, file("album/one.jpg"))
	two := put(t, s, file("two.mp3"))

	pending, err := s.PendingMedia(t.Context(), 1, 10)
	if err != nil {
		t.Fatalf("PendingMedia: %v", err)
	}
	// A directory has nothing to extract, so it must never be queued.
	if len(pending) != 2 {
		t.Fatalf("PendingMedia returned %d files, want the two that are not directories", len(pending))
	}

	if perr := s.PutMedia(t.Context(), db.Media{FileID: one.ID, Kind: db.KindImage, IndexedAt: time.Now(), Version: 1}); perr != nil {
		t.Fatal(perr)
	}
	pending, err = s.PendingMedia(t.Context(), 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != two.ID {
		t.Errorf("PendingMedia = %+v, want only the file that is still unindexed", pending)
	}

	// The limit is what keeps a first run over a large library from loading it
	// all into memory at once.
	if limited, err := s.PendingMedia(t.Context(), 1, 0); err != nil || len(limited) != 0 {
		t.Errorf("PendingMedia with a limit of zero = %+v, %v", limited, err)
	}
}

// mediaFailureIsFinal is the reason the row is written even when extraction
// fails: without it, one corrupt file is re-read on every pass forever.
func mediaFailureIsFinal(t *testing.T, s db.Repo) {
	f := put(t, s, file("broken.jpg"))

	failed := db.Media{
		FileID:    f.ID,
		Kind:      db.KindImage,
		IndexedAt: time.Now(),
		Version:   1,
		Error:     "no exif segment",
	}
	if err := s.PutMedia(t.Context(), failed); err != nil {
		t.Fatal(err)
	}

	pending, err := s.PendingMedia(t.Context(), 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Errorf("a file whose extraction failed is queued again: %+v", pending)
	}

	got, err := s.MediaByFile(t.Context(), f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Indexed() || got.Error != "no exif segment" {
		t.Errorf("got %+v, want the failure recorded", got)
	}
}

func mediaVersionBump(t *testing.T, s db.Repo) {
	f := put(t, s, file("photo.jpg"))
	if err := s.PutMedia(t.Context(), db.Media{FileID: f.ID, Kind: db.KindImage, IndexedAt: time.Now(), Version: 1}); err != nil {
		t.Fatal(err)
	}

	if pending, err := s.PendingMedia(t.Context(), 1, 10); err != nil || len(pending) != 0 {
		t.Fatalf("PendingMedia at the same version = %+v, %v", pending, err)
	}
	// A better extractor ships, the version goes up, and everything it already
	// looked at comes back without a migration or a script.
	pending, perr := s.PendingMedia(t.Context(), 2, 10)
	if perr != nil {
		t.Fatal(perr)
	}
	if len(pending) != 1 || pending[0].ID != f.ID {
		t.Errorf("PendingMedia at a newer version = %+v, want the file back", pending)
	}
}

// mediaCascade is what the foreign key is for: deleting a file takes its
// metadata with it, in the same statement, without files knowing media exists.
func mediaCascade(t *testing.T, s db.Repo) {
	f := put(t, s, file("doomed.jpg"))
	if err := s.PutMedia(t.Context(), db.Media{FileID: f.ID, Kind: db.KindImage, IndexedAt: time.Now(), Version: 1}); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteFile(t.Context(), owner, "doomed.jpg"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MediaByFile(t.Context(), f.ID); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("MediaByFile after deleting the file = %v, want ErrNotFound", err)
	}
}
