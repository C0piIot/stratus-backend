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
		{"the camera's date orders, the file's when there is none", photosOrder},
		{"a page resumes where the last stopped, through ties", photosPages},
		{"a month is a range of the timeline", photosMonthBound},
		{"months are listed newest first, on the camera's clock", photosMonths},
		{"a photo knows its neighbours", photosAround},
		{"only images are photos, and only indexed ones", photosOnlyImages},
		{"owners do not see each other's photos", photosOwnersAreSeparate},
		{"re-indexing moves a photo to its new date", photosReindex},
		{"a page asks for at least one row", photosLimit},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.fn(t, newRepo(t))
		})
	}
}

var photoDay = time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)

// photo writes an image taken at taken (zero for none) whose file arrived at
// arrived.
func photo(t *testing.T, s db.Repo, ownerID, path string, taken, arrived time.Time) db.File {
	t.Helper()
	f := file(path)
	f.OwnerID, f.MTime, f.MIMEType = ownerID, arrived, "image/jpeg"
	stored := put(t, s, f)
	if err := s.PutMedia(t.Context(), db.Media{
		FileID: stored.ID, Kind: db.KindImage, IndexedAt: photoDay, Version: 1, TakenAt: taken,
	}); err != nil {
		t.Fatalf("PutMedia(%q): %v", path, err)
	}
	return stored
}

func timeline(t *testing.T, s db.Repo, ownerID string, f db.PhotoFilter) []db.Photo {
	t.Helper()
	if f.Limit == 0 {
		f.Limit = 100
	}
	got, err := s.PhotoTimeline(t.Context(), ownerID, f)
	if err != nil {
		t.Fatalf("PhotoTimeline(%+v): %v", f, err)
	}
	return got
}

func photoPaths(photos []db.Photo) []string {
	out := make([]string, len(photos))
	for i, p := range photos {
		out[i] = p.File.Path
	}
	return out
}

func photosOrder(t *testing.T, s db.Repo) {
	photo(t, s, owner, "old.jpg", photoDay.AddDate(-1, 0, 0), photoDay)
	// A screenshot: no camera date, so it is listed when it arrived.
	photo(t, s, owner, "screenshot.png", time.Time{}, photoDay.AddDate(0, -1, 0))
	photo(t, s, owner, "new.jpg", photoDay, photoDay.AddDate(-5, 0, 0))

	got := timeline(t, s, owner, db.PhotoFilter{})
	if paths := photoPaths(got); !equal(paths, []string{"new.jpg", "screenshot.png", "old.jpg"}) {
		t.Errorf("timeline = %v, want newest first by the camera, then by arrival", paths)
	}
	if len(got) == 3 && !got[1].SortAt.Equal(photoDay.AddDate(0, -1, 0)) {
		t.Errorf("the screenshot is listed at %v, want its arrival", got[1].SortAt)
	}
}

// photosPages walks a timeline where several photos share one instant, which
// is what a burst from a phone looks like, a page at a time.
func photosPages(t *testing.T, s db.Repo) {
	for i := range 7 {
		// Three share an instant, so the cursor has to break the tie by id.
		at := photoDay.Add(-time.Duration(i/3) * time.Hour)
		photo(t, s, owner, fmt.Sprintf("p%d.jpg", i), at, photoDay)
	}
	want := photoPaths(timeline(t, s, owner, db.PhotoFilter{}))

	var got []string
	var after db.PhotoCursor
	for range 10 {
		page := timeline(t, s, owner, db.PhotoFilter{After: after, Limit: 2})
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

func photosMonthBound(t *testing.T, s db.Repo) {
	photo(t, s, owner, "may.jpg", time.Date(2024, 5, 31, 23, 59, 59, 0, time.UTC), photoDay)
	photo(t, s, owner, "june1.jpg", time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC), photoDay)
	photo(t, s, owner, "june30.jpg", time.Date(2024, 6, 30, 23, 59, 59, 0, time.UTC), photoDay)
	photo(t, s, owner, "july.jpg", time.Date(2024, 7, 1, 0, 0, 0, 0, time.UTC), photoDay)

	month := db.PhotoMonth{Year: 2024, Month: time.June}
	june := db.PhotoFilter{From: month.Start(), To: month.End()}
	if got := photoPaths(timeline(t, s, owner, june)); !equal(got, []string{"june30.jpg", "june1.jpg"}) {
		t.Errorf("June = %v, want its first and last second and nothing either side", got)
	}
}

func photosMonths(t *testing.T, s db.Repo) {
	// New Year's Eve on the camera's clock stays in December.
	photo(t, s, owner, "nye.jpg", time.Date(2023, 12, 31, 23, 30, 0, 0, time.UTC), photoDay)
	photo(t, s, owner, "a.jpg", time.Date(2024, 6, 2, 0, 0, 0, 0, time.UTC), photoDay)
	photo(t, s, owner, "b.jpg", time.Date(2024, 6, 20, 0, 0, 0, 0, time.UTC), photoDay)
	photo(t, s, owner, "c.jpg", time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), photoDay)

	got, err := s.PhotoMonths(t.Context(), owner)
	if err != nil {
		t.Fatal(err)
	}
	want := []db.PhotoMonth{
		{Year: 2024, Month: time.June},
		{Year: 2024, Month: time.January},
		{Year: 2023, Month: time.December},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("PhotoMonths = %v, want %v", got, want)
	}
}

func photosAround(t *testing.T, s db.Repo) {
	a := photo(t, s, owner, "a.jpg", photoDay.Add(2*time.Hour), photoDay)
	b := photo(t, s, owner, "b.jpg", photoDay.Add(time.Hour), photoDay)
	// c and d share an instant: the tie still has an order.
	c := photo(t, s, owner, "c.jpg", photoDay, photoDay)
	d := photo(t, s, owner, "d.jpg", photoDay, photoDay)

	order := photoPaths(timeline(t, s, owner, db.PhotoFilter{}))
	for i, path := range order {
		id := map[string]int64{"a.jpg": a.ID, "b.jpg": b.ID, "c.jpg": c.ID, "d.jpg": d.ID}[path]
		got, err := s.PhotoAround(t.Context(), owner, id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Photo.File.Path != path {
			t.Errorf("PhotoAround(%s) is %s", path, got.Photo.File.Path)
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

func name(p *db.Photo) string {
	if p == nil {
		return ""
	}
	return p.File.Path
}

func photosOnlyImages(t *testing.T, s db.Repo) {
	photo(t, s, owner, "pic.jpg", photoDay, photoDay)
	tr := track(t, s, owner, "song.flac", song("A", "A", "B", "C", 1))
	put(t, s, file("unindexed.jpg"))

	if got := photoPaths(timeline(t, s, owner, db.PhotoFilter{})); !equal(got, []string{"pic.jpg"}) {
		t.Errorf("timeline = %v, want the image alone", got)
	}
	if _, err := s.PhotoAround(t.Context(), owner, tr.ID); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("PhotoAround of a track = %v, want ErrNotFound", err)
	}
}

func photosOwnersAreSeparate(t *testing.T, s db.Repo) {
	mine := photo(t, s, owner, "mine.jpg", photoDay, photoDay)
	photo(t, s, "someone-else", "theirs.jpg", photoDay, photoDay)

	if got := photoPaths(timeline(t, s, owner, db.PhotoFilter{})); !equal(got, []string{"mine.jpg"}) {
		t.Errorf("timeline = %v", got)
	}
	if _, err := s.PhotoAround(t.Context(), "someone-else", mine.ID); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("another owner opens mine: %v", err)
	}
	around, err := s.PhotoAround(t.Context(), owner, mine.ID)
	if err != nil || around.Newer != nil || around.Older != nil {
		t.Errorf("mine has neighbours %v and %v, want none: %v", around.Newer, around.Older, err)
	}
	if months, _ := s.PhotoMonths(t.Context(), "nobody"); len(months) != 0 {
		t.Errorf("an owner with nothing has months %v", months)
	}
}

// photosReindex is PutMedia keeping sort_at in step: an extractor that finds a
// date on a second reading moves the photo.
func photosReindex(t *testing.T, s db.Repo) {
	f := photo(t, s, owner, "a.jpg", time.Time{}, photoDay)
	if err := s.PutMedia(t.Context(), db.Media{
		FileID: f.ID, Kind: db.KindImage, IndexedAt: photoDay, Version: 2, TakenAt: photoDay.AddDate(-3, 0, 0),
	}); err != nil {
		t.Fatal(err)
	}
	got := timeline(t, s, owner, db.PhotoFilter{})
	if len(got) != 1 || !got[0].SortAt.Equal(photoDay.AddDate(-3, 0, 0)) {
		t.Errorf("after a re-index the photo is at %v, want the camera's date", got)
	}
}

func photosLimit(t *testing.T, s db.Repo) {
	if _, err := s.PhotoTimeline(t.Context(), owner, db.PhotoFilter{}); err == nil {
		t.Error("a page of nothing = nil, want a refusal")
	}
}
