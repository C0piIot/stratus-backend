package dbtest

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// RunPhotos executes the gallery cases against the repository built by newRepo.
func RunPhotos(t *testing.T, newRepo func(t *testing.T) db.Repo) {
	t.Helper()

	cases := []struct {
		name string
		fn   func(t *testing.T, s db.Repo)
	}{
		{"the camera's date orders, the file's when there is none", capturesOrder},
		{"a page resumes where the last stopped, through ties", capturesPages},
		{"a month is a range of the timeline", capturesMonthBound},
		{"months are listed newest first, on the camera's clock", capturesMonths},
		{"a photo knows its neighbours", capturesAround},
		{"one kind at a time, and only indexed files", capturesOneKindAtATime},
		{"owners do not see each other's photos", capturesOwnersAreSeparate},
		{"re-indexing moves a photo to its new date", capturesReindex},
		{"a page asks for at least one row", capturesLimit},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.fn(t, newRepo(t))
		})
	}
}

var captureDay = time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)

// photo writes an image taken at taken (zero for none) whose file arrived at
// arrived.
func photo(t *testing.T, s db.Repo, ownerID, path string, taken, arrived time.Time) db.File {
	t.Helper()
	return capture(t, s, db.KindImage, ownerID, path, taken, arrived)
}

// video is the same for a recording, which the timeline carries by the same
// rules and under a different kind (#215).
func video(t *testing.T, s db.Repo, ownerID, path string, taken, arrived time.Time) db.File {
	t.Helper()
	return capture(t, s, db.KindVideo, ownerID, path, taken, arrived)
}

func capture(t *testing.T, s db.Repo, kind db.Kind, ownerID, path string, taken, arrived time.Time) db.File {
	t.Helper()
	mime := "image/jpeg"
	if kind == db.KindVideo {
		mime = "video/mp4"
	}
	f := file(path)
	f.OwnerID, f.MTime, f.MIMEType = ownerID, arrived, mime
	stored := put(t, s, f)
	if err := s.PutMedia(t.Context(), db.Media{
		FileID: stored.ID, Kind: kind, IndexedAt: captureDay, Version: 1, TakenAt: taken,
	}); err != nil {
		t.Fatalf("PutMedia(%q): %v", path, err)
	}
	return stored
}

func timeline(t *testing.T, s db.Repo, ownerID string, f db.CaptureFilter) []db.Capture {
	t.Helper()
	if f.Limit == 0 {
		f.Limit = 100
	}
	if f.Kind == "" {
		f.Kind = db.KindImage
	}
	got, err := s.Timeline(t.Context(), ownerID, f)
	if err != nil {
		t.Fatalf("Timeline(%+v): %v", f, err)
	}
	return got
}

func capturePaths(photos []db.Capture) []string {
	out := make([]string, len(photos))
	for i, p := range photos {
		out[i] = p.File.Path
	}
	return out
}

func capturesOrder(t *testing.T, s db.Repo) {
	photo(t, s, owner, "old.jpg", captureDay.AddDate(-1, 0, 0), captureDay)
	// A screenshot: no camera date, so it is listed when it arrived.
	photo(t, s, owner, "screenshot.png", time.Time{}, captureDay.AddDate(0, -1, 0))
	photo(t, s, owner, "new.jpg", captureDay, captureDay.AddDate(-5, 0, 0))

	got := timeline(t, s, owner, db.CaptureFilter{})
	if paths := capturePaths(got); !equal(paths, []string{"new.jpg", "screenshot.png", "old.jpg"}) {
		t.Errorf("timeline = %v, want newest first by the camera, then by arrival", paths)
	}
	if len(got) == 3 && !got[1].SortAt.Equal(captureDay.AddDate(0, -1, 0)) {
		t.Errorf("the screenshot is listed at %v, want its arrival", got[1].SortAt)
	}
}

// capturesPages walks a timeline where several photos share one instant, which
// is what a burst from a phone looks like, a page at a time.
func capturesPages(t *testing.T, s db.Repo) {
	for i := range 7 {
		// Three share an instant, so the cursor has to break the tie by id.
		at := captureDay.Add(-time.Duration(i/3) * time.Hour)
		photo(t, s, owner, fmt.Sprintf("p%d.jpg", i), at, captureDay)
	}
	want := capturePaths(timeline(t, s, owner, db.CaptureFilter{}))

	var got []string
	var after db.CaptureCursor
	for range 10 {
		page := timeline(t, s, owner, db.CaptureFilter{After: after, Limit: 2})
		if len(page) == 0 {
			break
		}
		for _, p := range page {
			got = append(got, p.File.Path)
		}
		after = page[len(page)-1].Cursor()
	}
	if !equal(got, want) || len(got) != 7 {
		t.Errorf("paged = %v, want %v whole", got, want)
	}
}

func capturesMonthBound(t *testing.T, s db.Repo) {
	photo(t, s, owner, "may.jpg", time.Date(2024, 5, 31, 23, 59, 59, 0, time.UTC), captureDay)
	photo(t, s, owner, "june1.jpg", time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC), captureDay)
	photo(t, s, owner, "june30.jpg", time.Date(2024, 6, 30, 23, 59, 59, 0, time.UTC), captureDay)
	photo(t, s, owner, "july.jpg", time.Date(2024, 7, 1, 0, 0, 0, 0, time.UTC), captureDay)

	month := db.Month{Year: 2024, Month: time.June}
	june := db.CaptureFilter{From: month.Start(), To: month.End()}
	if got := capturePaths(timeline(t, s, owner, june)); !equal(got, []string{"june30.jpg", "june1.jpg"}) {
		t.Errorf("June = %v, want its first and last second and nothing either side", got)
	}
}

func capturesMonths(t *testing.T, s db.Repo) {
	// New Year's Eve on the camera's clock stays in December.
	photo(t, s, owner, "nye.jpg", time.Date(2023, 12, 31, 23, 30, 0, 0, time.UTC), captureDay)
	photo(t, s, owner, "a.jpg", time.Date(2024, 6, 2, 0, 0, 0, 0, time.UTC), captureDay)
	photo(t, s, owner, "b.jpg", time.Date(2024, 6, 20, 0, 0, 0, 0, time.UTC), captureDay)
	photo(t, s, owner, "c.jpg", time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), captureDay)

	got, err := s.Months(t.Context(), owner, db.KindImage)
	if err != nil {
		t.Fatal(err)
	}
	want := []db.Month{
		{Year: 2024, Month: time.June},
		{Year: 2024, Month: time.January},
		{Year: 2023, Month: time.December},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("Months = %v, want %v", got, want)
	}
}

func capturesAround(t *testing.T, s db.Repo) {
	a := photo(t, s, owner, "a.jpg", captureDay.Add(2*time.Hour), captureDay)
	b := photo(t, s, owner, "b.jpg", captureDay.Add(time.Hour), captureDay)
	// c and d share an instant: the tie still has an order.
	c := photo(t, s, owner, "c.jpg", captureDay, captureDay)
	d := photo(t, s, owner, "d.jpg", captureDay, captureDay)

	order := capturePaths(timeline(t, s, owner, db.CaptureFilter{}))
	for i, path := range order {
		id := map[string]int64{"a.jpg": a.ID, "b.jpg": b.ID, "c.jpg": c.ID, "d.jpg": d.ID}[path]
		got, err := s.Around(t.Context(), owner, db.KindImage, id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Capture.File.Path != path {
			t.Errorf("Around(%s) is %s", path, got.Capture.File.Path)
		}
		wantNewer, wantOlder := "", ""
		if i > 0 {
			wantNewer = order[i-1]
		}
		if i < len(order)-1 {
			wantOlder = order[i+1]
		}
		if name(got.Newer) != wantNewer || name(got.Older) != wantOlder {
			t.Errorf("around %s = newer %q, older %q, want %q and %q", path, name(got.Newer), name(got.Older), wantNewer, wantOlder)
		}
	}
}

func name(p *db.Capture) string {
	if p == nil {
		return ""
	}
	return p.File.Path
}

// capturesOneKindAtATime: the gallery and the video library are the same queries
// over the same column, told apart by the kind the caller asks for (#215).
// Neither carries the other's files, and neither carries a track or a row the
// indexer has not written.
func capturesOneKindAtATime(t *testing.T, s db.Repo) {
	pic := photo(t, s, owner, "pic.jpg", captureDay, captureDay)
	vid := video(t, s, owner, "clip.mp4", captureDay.Add(time.Hour), captureDay)
	tr := track(t, s, owner, "song.flac", song("A", "A", "B", "C", 1))
	put(t, s, file("unindexed.jpg"))

	if got := capturePaths(timeline(t, s, owner, db.CaptureFilter{Kind: db.KindImage})); !equal(got, []string{"pic.jpg"}) {
		t.Errorf("the images = %v, want the image alone", got)
	}
	if got := capturePaths(timeline(t, s, owner, db.CaptureFilter{Kind: db.KindVideo})); !equal(got, []string{"clip.mp4"}) {
		t.Errorf("the videos = %v, want the recording alone", got)
	}

	// The months are per kind too, or a year that holds only video would show
	// in the gallery with nothing in it.
	months, err := s.Months(t.Context(), owner, db.KindVideo)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(months) != fmt.Sprint([]db.Month{db.MonthOf(captureDay)}) {
		t.Errorf("the video months = %v", months)
	}

	// And a file is only found among its own: asking the wrong library for it
	// is the same answer as asking for a track.
	for _, c := range []struct {
		kind db.Kind
		id   int64
		what string
	}{
		{db.KindImage, vid.ID, "a recording in the gallery"},
		{db.KindVideo, pic.ID, "an image in the videos"},
		{db.KindImage, tr.ID, "a track"},
	} {
		if _, err := s.Around(t.Context(), owner, c.kind, c.id); !errors.Is(err, db.ErrNotFound) {
			t.Errorf("Around of %s = %v, want ErrNotFound", c.what, err)
		}
	}
}

func capturesOwnersAreSeparate(t *testing.T, s db.Repo) {
	mine := photo(t, s, owner, "mine.jpg", captureDay, captureDay)
	photo(t, s, "someone-else", "theirs.jpg", captureDay, captureDay)

	if got := capturePaths(timeline(t, s, owner, db.CaptureFilter{})); !equal(got, []string{"mine.jpg"}) {
		t.Errorf("timeline = %v", got)
	}
	if _, err := s.Around(t.Context(), "someone-else", db.KindImage, mine.ID); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("another owner opens mine: %v", err)
	}
	around, err := s.Around(t.Context(), owner, db.KindImage, mine.ID)
	if err != nil || around.Newer != nil || around.Older != nil {
		t.Errorf("mine has neighbours %v and %v, want none: %v", around.Newer, around.Older, err)
	}
	if months, _ := s.Months(t.Context(), "nobody", db.KindImage); len(months) != 0 {
		t.Errorf("an owner with nothing has months %v", months)
	}
}

// capturesReindex is PutMedia keeping sort_at in step: an extractor that finds a
// date on a second reading moves the photo.
func capturesReindex(t *testing.T, s db.Repo) {
	f := photo(t, s, owner, "a.jpg", time.Time{}, captureDay)
	if err := s.PutMedia(t.Context(), db.Media{
		FileID: f.ID, Kind: db.KindImage, IndexedAt: captureDay, Version: 2, TakenAt: captureDay.AddDate(-3, 0, 0),
	}); err != nil {
		t.Fatal(err)
	}
	got := timeline(t, s, owner, db.CaptureFilter{})
	if len(got) != 1 || !got[0].SortAt.Equal(captureDay.AddDate(-3, 0, 0)) {
		t.Errorf("after a re-index the photo is at %v, want the camera's date", got)
	}
}

func capturesLimit(t *testing.T, s db.Repo) {
	if _, err := s.Timeline(t.Context(), owner, db.CaptureFilter{}); err == nil {
		t.Error("a page of nothing = nil, want a refusal")
	}
}
