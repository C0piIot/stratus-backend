package dbtest

import (
	"errors"
	"testing"
	"time"

	"github.com/stratus-private-cloud/stratus-backend/internal/db"
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
		{"all three media times survive a round trip", mediaTimeRoundTrip},
		{"the fields another kind does not use stay empty", mediaSparse},
		{"pending skips what is indexed", mediaPending},
		{"a failed extraction is not retried", mediaFailureIsFinal},
		{"a failure with a time on it is retried when it arrives", mediaRetry},
		{"a newer extractor puts everything back in the queue", mediaVersionBump},
		{"overwriting a file puts it back in the queue", mediaOverwrite},
		{"the counts say how much of the library is done", mediaCounts},
		{"the counts say how much of it has a picture", thumbsCounted},
		{"states come back for the files asked about", mediaStates},
		{"pictures are pending until something makes them", thumbsPending},
		{"the newest pictures are offered first", thumbsNewestFirst},
		{"a picture's verdict survives a re-index", thumbsSurviveReindex},
		{"a newer generator puts the pictures back in the queue", thumbsGenerationBump},
		{"overwriting a file puts its pictures back in the queue", thumbsOverwrite},
		{"a refused picture comes back when the verdict goes stale", thumbsRefusalGoesStale},
		{"a verdict about a file with no metadata is not found", thumbsVerdictNeedsARow},
		{"deleting the file deletes its metadata", mediaCascade},
		{"metadata for a file that is gone is not found", mediaOrphan},
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
// more: media has 22 columns, four of them were written by no case or asserted
// by no case, and disc_no was in neither camp because nothing wrote it at all.
//
// Nothing is exempt here. Unlike a file row, a media row can legally carry
// every column at once -- that is what one wide table means -- so the fixture
// carries all of them and assertEveryFieldSet keeps it that way.
func mediaEveryColumn(t *testing.T, s db.Repo) {
	f := put(t, s, file("videos/every-column.mkv"))

	// Normalize because PutMedia does: the times come back at TimePrecision.
	want := media(f).Normalize()
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
func media(f db.File) db.Media {
	return db.Media{
		FileID:      f.ID,
		Kind:        db.KindVideo,
		IndexedAt:   time.Date(2024, 7, 2, 9, 15, 0, 0, time.UTC),
		Version:     7,
		ETag:        f.ETag,
		Error:       "truncated at the last frame",
		RetryAt:     time.Date(2024, 7, 2, 10, 15, 0, 0, time.UTC),
		TakenAt:     time.Date(2023, 5, 4, 18, 45, 30, 0, time.UTC),
		Width:       3840,
		Height:      2160,
		Orientation: 3,
		GPS:         &db.GPS{Latitude: 41.3874, Longitude: 2.1686},
		Camera:      "Sony ILCE-7M4",
		DurationMS:  742_000,
		Codec:       "hevc",
		// Distinct values, so a driver that wrote one column into another
		// comes back wrong rather than coincidentally right.
		Bitrate:        320_000,
		SampleRate:     48_000,
		Channels:       6,
		BitDepth:       24,
		CodecProfile:   "HE-AAC",
		Level:          41,
		FrameRate:      29_970,
		AudioCodec:     "ac3",
		ColorPrimaries: "bt2020",
		ColorTransfer:  "arib-std-b67",
		ColorSpace:     "bt2020nc",
		DoViProfile:    8,
		Artist:         "Boards of Canada",
		AlbumArtist:    "Various Artists",
		Album:          "Music Has the Right to Children",
		Title:          "Roygbiv",
		TrackNo:        10,
		DiscNo:         2,
		Year:           1998,
		Genre:          "Electronic",
		MBReleaseGroup: "4cf8ba42-5d0b-3a4d-8e8f-4c8f2f3d1a1b",
		MBRelease:      "e1a8b6a4-9a1c-4ed2-9a7f-5b3d7c0e2f44",
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
	retry := time.Date(2024, 6, 1, 13, 30, 15, 123_456_789, zone)

	err := s.PutMedia(t.Context(), db.Media{
		FileID:    f.ID,
		Kind:      db.KindImage,
		IndexedAt: indexed,
		Version:   1,
		TakenAt:   taken,
		RetryAt:   retry,
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
	if want := retry.UTC().Truncate(db.TimePrecision); !got.RetryAt.Equal(want) {
		t.Errorf("RetryAt = %v, want %v", got.RetryAt, want)
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

	pending, err := s.PendingMedia(t.Context(), 1, time.Now(), 10)
	if err != nil {
		t.Fatalf("PendingMedia: %v", err)
	}
	// A directory has nothing to extract, so it must never be queued.
	if len(pending) != 2 {
		t.Fatalf("PendingMedia returned %d files, want the two that are not directories", len(pending))
	}

	if perr := s.PutMedia(t.Context(), indexed(one, 1)); perr != nil {
		t.Fatal(perr)
	}
	pending, err = s.PendingMedia(t.Context(), 1, time.Now(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != two.ID {
		t.Errorf("PendingMedia = %+v, want only the file that is still unindexed", pending)
	}

	// The limit is what keeps a first run over a large library from loading it
	// all into memory at once.
	if limited, err := s.PendingMedia(t.Context(), 1, time.Now(), 0); err != nil || len(limited) != 0 {
		t.Errorf("PendingMedia with a limit of zero = %+v, %v", limited, err)
	}
}

// mediaFailureIsFinal is the reason the row is written even when extraction
// fails: without it, one corrupt file is re-read on every pass forever.
func mediaFailureIsFinal(t *testing.T, s db.Repo) {
	f := put(t, s, file("broken.jpg"))

	failed := indexed(f, 1)
	failed.Error = "no exif segment"
	if err := s.PutMedia(t.Context(), failed); err != nil {
		t.Fatal(err)
	}

	pending, err := s.PendingMedia(t.Context(), 1, time.Now(), 10)
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

// mediaRetry is the other half of the rule above: a failure that was about
// reaching the bytes rather than about the bytes carries a time, and the queue
// hands the file back when that time arrives and not before (#157).
//
// The row is written either way. A deferred file with no row would sit at the
// head of the queue -- ordered by id and limited -- and nothing behind it would
// ever be read.
func mediaRetry(t *testing.T, s db.Repo) {
	f := put(t, s, file("unreachable.jpg"))

	deferred := indexed(f, 1)
	deferred.Error = "the store timed out"
	deferred.RetryAt = time.Now().Add(time.Hour)
	if err := s.PutMedia(t.Context(), deferred); err != nil {
		t.Fatal(err)
	}

	if pending, err := s.PendingMedia(t.Context(), 1, time.Now(), 10); err != nil || len(pending) != 0 {
		t.Fatalf("PendingMedia before the time = %+v, %v", pending, err)
	}
	pending, err := s.PendingMedia(t.Context(), 1, time.Now().Add(2*time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != f.ID {
		t.Errorf("PendingMedia after the time = %+v, want the file back", pending)
	}

	// And while it waits it is pending rather than unreadable, on both surfaces
	// that report: nobody is going to go looking at a file that is fine.
	counts, err := s.MediaCounts(t.Context(), 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Failed != 0 || counts.Pending() != 1 {
		t.Errorf("counts = %+v (pending %d), want it waiting rather than failed", counts, counts.Pending())
	}
	states, err := s.MediaStates(t.Context(), []int64{f.ID})
	if err != nil {
		t.Fatal(err)
	}
	if states[f.ID].Failed {
		t.Error("a file waiting to be tried again is marked as failed in a listing")
	}
}

func mediaVersionBump(t *testing.T, s db.Repo) {
	f := put(t, s, file("photo.jpg"))
	if err := s.PutMedia(t.Context(), indexed(f, 1)); err != nil {
		t.Fatal(err)
	}

	if pending, err := s.PendingMedia(t.Context(), 1, time.Now(), 10); err != nil || len(pending) != 0 {
		t.Fatalf("PendingMedia at the same version = %+v, %v", pending, err)
	}
	// A better extractor ships, the version goes up, and everything it already
	// looked at comes back without a migration or a script.
	pending, perr := s.PendingMedia(t.Context(), 2, time.Now(), 10)
	if perr != nil {
		t.Fatal(perr)
	}
	if len(pending) != 1 || pending[0].ID != f.ID {
		t.Errorf("PendingMedia at a newer version = %+v, want the file back", pending)
	}
}

// thumbsCounted is the other half of the same walk: how much of what a grid
// draws has a picture, which is what somebody watches while a pass works
// through a library (#341).
func thumbsCounted(t *testing.T, s db.Repo) {
	made := put(t, s, file("made.jpg"))
	refused := put(t, s, file("refused.mp4"))
	waiting := put(t, s, file("waiting.jpg"))
	// A track has a cover rather than a thumbnail, and these numbers are
	// about the grid.
	track := put(t, s, file("song.mp3"))

	for _, m := range []db.Media{
		indexed(made, 1), indexedAs(refused, db.KindVideo, 1),
		indexed(waiting, 1), indexedAs(track, db.KindAudio, 1),
	} {
		if err := s.PutMedia(t.Context(), m); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.PutThumbVerdict(t.Context(), verdict(made, db.ThumbMade, 1)); err != nil {
		t.Fatal(err)
	}
	if err := s.PutThumbVerdict(t.Context(), verdict(refused, db.ThumbRefused, 1)); err != nil {
		t.Fatal(err)
	}

	got, err := s.MediaCounts(t.Context(), 1, 1)
	if err != nil {
		t.Fatalf("MediaCounts: %v", err)
	}
	want := db.ThumbCounts{Files: 3, Made: 1, Refused: 1}
	if got.Pictures != want || got.Pictures.Waiting() != 1 {
		t.Errorf("pictures = %+v (waiting %d), want %+v with one waiting",
			got.Pictures, got.Pictures.Waiting(), want)
	}

	// A newer generator puts them all back: what was made is no longer what
	// would be served, which is the number somebody is watching when they
	// raise it.
	got, err = s.MediaCounts(t.Context(), 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got.Pictures.Made != 0 || got.Pictures.Refused != 0 || got.Pictures.Waiting() != 3 {
		t.Errorf("pictures at a newer generation = %+v, want all three waiting", got.Pictures)
	}
}

// thumbsPending is the second queue, and the three things it promises (#341):
// only what could have a picture, only after the extractor has said what the
// file is, and never a directory.
func thumbsPending(t *testing.T, s db.Repo) {
	if _, err := s.CreateDir(t.Context(), owner, "album"); err != nil {
		t.Fatal(err)
	}
	photo := put(t, s, file("album/one.jpg"))
	film := put(t, s, file("album/holiday.mp4"))
	track := put(t, s, file("album/song.mp3"))
	// Indexed by nobody yet, so it has no kind: the extractor's half goes
	// first and this one gets it on the pass after.
	put(t, s, file("album/two.jpg"))

	for _, m := range []db.Media{
		indexed(photo, 1),
		indexedAs(film, db.KindVideo, 1),
		indexedAs(track, db.KindAudio, 1),
	} {
		if err := s.PutMedia(t.Context(), m); err != nil {
			t.Fatal(err)
		}
	}

	// One kind per call, so the track and the file nothing has indexed are
	// absent from both answers rather than filtered out of one.
	pending, err := s.PendingThumbs(t.Context(), db.KindImage, 1, time.Now(), 10)
	if err != nil {
		t.Fatalf("PendingThumbs: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != photo.ID {
		t.Fatalf("PendingThumbs for pictures = %+v, want the photograph alone", pending)
	}
	films, err := s.PendingThumbs(t.Context(), db.KindVideo, 1, time.Now(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(films) != 1 || films[0].ID != film.ID {
		t.Fatalf("PendingThumbs for films = %+v, want the film alone", films)
	}

	if verr := s.PutThumbVerdict(t.Context(), verdict(photo, db.ThumbMade, 1)); verr != nil {
		t.Fatal(verr)
	}
	if pending, err = s.PendingThumbs(t.Context(), db.KindImage, 1, time.Now(), 10); err != nil || len(pending) != 0 {
		t.Errorf("PendingThumbs after the picture was made = %+v, %v", pending, err)
	}

	// The limit is what paces the pass, the way it bounds a first index.
	if limited, err := s.PendingThumbs(t.Context(), db.KindVideo, 1, time.Now(), 0); err != nil || len(limited) != 0 {
		t.Errorf("PendingThumbs with a limit of zero = %+v, %v", limited, err)
	}
}

// thumbsNewestFirst is why the order is the gallery's own: what somebody opens
// is the roll that just arrived, so that is what the pass makes first.
func thumbsNewestFirst(t *testing.T, s db.Repo) {
	old := put(t, s, file("old.jpg"))
	recent := put(t, s, file("recent.jpg"))

	older := indexed(old, 1)
	older.TakenAt = time.Date(2019, 3, 1, 9, 0, 0, 0, time.UTC)
	newer := indexed(recent, 1)
	newer.TakenAt = time.Date(2024, 8, 1, 9, 0, 0, 0, time.UTC)
	for _, m := range []db.Media{older, newer} {
		if err := s.PutMedia(t.Context(), m); err != nil {
			t.Fatal(err)
		}
	}

	pending, err := s.PendingThumbs(t.Context(), db.KindImage, 1, time.Now(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 || pending[0].ID != recent.ID || pending[1].ID != old.ID {
		t.Errorf("PendingThumbs = %+v, want the newest photograph first", pending)
	}
}

// thumbsSurviveReindex is the property the four columns are separate for: a
// better extractor puts the library back in the metadata queue, and it must
// not put it back in this one -- the pictures are of bytes nothing moved.
func thumbsSurviveReindex(t *testing.T, s db.Repo) {
	photo := put(t, s, file("photo.jpg"))
	if err := s.PutMedia(t.Context(), indexed(photo, 1)); err != nil {
		t.Fatal(err)
	}
	if err := s.PutThumbVerdict(t.Context(), verdict(photo, db.ThumbMade, 1)); err != nil {
		t.Fatal(err)
	}

	if err := s.PutMedia(t.Context(), indexed(photo, 2)); err != nil {
		t.Fatal(err)
	}
	if pending, err := s.PendingThumbs(t.Context(), db.KindImage, 1, time.Now(), 10); err != nil || len(pending) != 0 {
		t.Fatalf("PendingThumbs after a re-index = %+v, %v, want the verdict kept", pending, err)
	}
	states, err := s.MediaStates(t.Context(), []int64{photo.ID})
	if err != nil {
		t.Fatal(err)
	}
	if states[photo.ID].Thumb != db.ThumbMade {
		t.Errorf("state after a re-index = %+v, want the picture still made", states[photo.ID])
	}
}

// thumbsGenerationBump is files.DerivedGeneration's half of the same question:
// raising it changes the key every picture is kept under, so what was made is
// no longer what would be served.
func thumbsGenerationBump(t *testing.T, s db.Repo) {
	photo := put(t, s, file("photo.jpg"))
	if err := s.PutMedia(t.Context(), indexed(photo, 1)); err != nil {
		t.Fatal(err)
	}
	if err := s.PutThumbVerdict(t.Context(), verdict(photo, db.ThumbMade, 1)); err != nil {
		t.Fatal(err)
	}

	if pending, err := s.PendingThumbs(t.Context(), db.KindImage, 1, time.Now(), 10); err != nil || len(pending) != 0 {
		t.Fatalf("PendingThumbs at the same generation = %+v, %v", pending, err)
	}
	pending, err := s.PendingThumbs(t.Context(), db.KindImage, 2, time.Now(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != photo.ID {
		t.Errorf("PendingThumbs at a newer generation = %+v, want the file back", pending)
	}
}

// thumbsOverwrite is the blob key's reason for being on the row. A replaced
// file keeps its row and its id while its bytes change, and the pictures of
// the bytes that are gone are not the pictures of the bytes that are there.
func thumbsOverwrite(t *testing.T, s db.Repo) {
	first := put(t, s, file("holiday.jpg"))
	if err := s.PutMedia(t.Context(), indexed(first, 1)); err != nil {
		t.Fatal(err)
	}
	if err := s.PutThumbVerdict(t.Context(), verdict(first, db.ThumbMade, 1)); err != nil {
		t.Fatal(err)
	}

	replacement := file("holiday.jpg")
	replacement.ETag = `"another photograph"`
	replacement.BlobKey = "blobs/holiday-2"
	second := put(t, s, replacement)
	if second.ID != first.ID {
		t.Fatalf("the overwrite made a new row (%d, was %d); this case is about the one that does not",
			second.ID, first.ID)
	}

	pending, err := s.PendingThumbs(t.Context(), db.KindImage, 1, time.Now(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != first.ID {
		t.Errorf("PendingThumbs = %+v, want the overwritten file back", pending)
	}
}

// thumbsRefusalGoesStale is #338's week, kept here instead of by the sweep: a
// verdict cannot know whether the build that reached it is still this build,
// so an ffmpeg recipe that gains a decoder un-refuses what needed it by
// itself.
func thumbsRefusalGoesStale(t *testing.T, s db.Repo) {
	film := put(t, s, file("av1.mp4"))
	if err := s.PutMedia(t.Context(), indexedAs(film, db.KindVideo, 1)); err != nil {
		t.Fatal(err)
	}
	refused := verdict(film, db.ThumbRefused, 1)
	if err := s.PutThumbVerdict(t.Context(), refused); err != nil {
		t.Fatal(err)
	}

	if pending, err := s.PendingThumbs(t.Context(), db.KindVideo, 1, refused.At.Add(-time.Hour), 10); err != nil || len(pending) != 0 {
		t.Fatalf("PendingThumbs while the verdict stands = %+v, %v", pending, err)
	}
	pending, err := s.PendingThumbs(t.Context(), db.KindVideo, 1, refused.At.Add(time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != film.ID {
		t.Errorf("PendingThumbs after the verdict went stale = %+v, want the file back", pending)
	}

	// And a listing can read the same fact in one go, which is what the column
	// is for: a page stops offering a picture nothing can draw.
	states, err := s.MediaStates(t.Context(), []int64{film.ID})
	if err != nil {
		t.Fatal(err)
	}
	got := states[film.ID]
	if got.Thumb != db.ThumbRefused {
		t.Errorf("state = %+v, want the refusal", got)
	}
	if got.ThumbAt.Sub(refused.At).Abs() > time.Second {
		t.Errorf("the verdict is timed %v, want about %v", got.ThumbAt, refused.At)
	}
}

// thumbsVerdictNeedsARow is what a pass racing a delete looks like: the file
// went while its picture was being made, and there is nothing to record.
func thumbsVerdictNeedsARow(t *testing.T, s db.Repo) {
	photo := put(t, s, file("photo.jpg"))

	if err := s.PutThumbVerdict(t.Context(), verdict(photo, db.ThumbMade, 1)); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("a verdict about a file nothing has indexed = %v, want ErrNotFound", err)
	}
	if err := s.PutThumbVerdict(t.Context(), db.ThumbVerdict{FileID: 99999, At: time.Now()}); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("a verdict about a file that does not exist = %v, want ErrNotFound", err)
	}
}

// indexedAs is indexed for the cases that care which kind the row carries,
// since what may have a picture is a property of the kind.
func indexedAs(f db.File, kind db.Kind, version int) db.Media {
	m := indexed(f, version)
	m.Kind = kind
	return m
}

// verdict is what a generator writes down about one file's pictures.
func verdict(f db.File, state db.ThumbState, generation int) db.ThumbVerdict {
	return db.ThumbVerdict{
		FileID:     f.ID,
		State:      state,
		BlobKey:    f.BlobKey,
		Generation: generation,
		At:         time.Now(),
	}
}

// mediaCascade is what the foreign key is for: deleting a file takes its
// metadata with it, in the same statement, without files knowing media exists.
// indexed is a row saying "this file, these bytes, this extractor", which is
// what every queue case needs and none of them care about the detail of.
func indexed(f db.File, version int) db.Media {
	return db.Media{
		FileID:    f.ID,
		Kind:      db.KindImage,
		IndexedAt: time.Now(),
		Version:   version,
		ETag:      f.ETag,
	}
}

// mediaOverwrite is the bug the etag column exists for. Replacing a file keeps
// its row and therefore its id, so metadata written from the bytes that are
// gone would otherwise describe the bytes that are there until somebody raised
// the extractor version.
func mediaOverwrite(t *testing.T, s db.Repo) {
	first := put(t, s, file("holiday.mp4"))
	if err := s.PutMedia(t.Context(), indexed(first, 1)); err != nil {
		t.Fatal(err)
	}
	if pending, err := s.PendingMedia(t.Context(), 1, time.Now(), 10); err != nil || len(pending) != 0 {
		t.Fatalf("PendingMedia after indexing = %+v, %v", pending, err)
	}

	replacement := file("holiday.mp4")
	replacement.ETag = `"a different film"`
	replacement.BlobKey = "blobs/holiday-2"
	second := put(t, s, replacement)
	if second.ID != first.ID {
		t.Fatalf("the overwrite made a new row (%d, was %d); this case is about the one that does not",
			second.ID, first.ID)
	}

	pending, err := s.PendingMedia(t.Context(), 1, time.Now(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != first.ID {
		t.Errorf("PendingMedia = %+v, want the overwritten file back", pending)
	}
}

func mediaCounts(t *testing.T, s db.Repo) {
	if _, err := s.CreateDir(t.Context(), owner, "album"); err != nil {
		t.Fatal(err)
	}
	done := put(t, s, file("album/done.jpg"))
	broken := put(t, s, file("album/broken.jpg"))
	put(t, s, file("album/waiting.jpg"))

	if err := s.PutMedia(t.Context(), indexed(done, 3)); err != nil {
		t.Fatal(err)
	}
	failed := indexed(broken, 3)
	failed.Error = "no exif segment"
	if err := s.PutMedia(t.Context(), failed); err != nil {
		t.Fatal(err)
	}

	got, err := s.MediaCounts(t.Context(), 3, 1)
	if err != nil {
		t.Fatalf("MediaCounts: %v", err)
	}
	// The directory is not a file with metadata, and must not be counted as one
	// waiting for some.
	if got.Files != 3 || got.Indexed != 1 || got.Failed != 1 || got.Pending() != 1 {
		t.Errorf("counts = %+v (pending %d), want 3 files, 1 indexed, 1 failed, 1 pending",
			got, got.Pending())
	}

	// And what is pending by these numbers is what the queue would hand out.
	pending, err := s.PendingMedia(t.Context(), 3, time.Now(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(pending)) != got.Pending() {
		t.Errorf("the queue holds %d and the counts say %d", len(pending), got.Pending())
	}
}

func mediaStates(t *testing.T, s db.Repo) {
	done := put(t, s, file("done.jpg"))
	waiting := put(t, s, file("waiting.jpg"))
	broken := put(t, s, file("broken.jpg"))

	if err := s.PutMedia(t.Context(), indexed(done, 3)); err != nil {
		t.Fatal(err)
	}
	failed := indexed(broken, 3)
	failed.Error = "no exif segment"
	if err := s.PutMedia(t.Context(), failed); err != nil {
		t.Fatal(err)
	}

	// An id that has no row and an id that is not a file at all: a listing asks
	// about the page it is rendering, not about what it knows has metadata.
	states, err := s.MediaStates(t.Context(), []int64{done.ID, waiting.ID, broken.ID, 99999})
	if err != nil {
		t.Fatalf("MediaStates: %v", err)
	}
	if len(states) != 2 {
		t.Fatalf("states = %+v, want only the two files that have a row", states)
	}
	if got := states[done.ID]; got.Version != 3 || got.ETag != done.ETag || got.Failed {
		t.Errorf("the indexed file = %+v", got)
	}
	if got := states[broken.ID]; !got.Failed {
		t.Errorf("the failed file = %+v, want it marked", got)
	}
	if _, ok := states[waiting.ID]; ok {
		t.Error("a file with no row came back with a state")
	}

	empty, err := s.MediaStates(t.Context(), nil)
	if err != nil || len(empty) != 0 {
		t.Errorf("MediaStates(nil) = %+v, %v", empty, err)
	}
}

// mediaOrphan is the race a write-driven indexer can lose: a file uploaded and
// deleted again leaves an extractor holding metadata with nowhere to put it
// (#158). Every driver has its own name for a foreign key with no parent, so
// the port's name for it is the one the caller can act on.
func mediaOrphan(t *testing.T, s db.Repo) {
	err := s.PutMedia(t.Context(), db.Media{
		FileID: 987654, Kind: db.KindImage, IndexedAt: time.Now(), Version: 1,
	})
	if !errors.Is(err, db.ErrNotFound) {
		t.Errorf("PutMedia for a file that does not exist = %v, want ErrNotFound", err)
	}
}

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
