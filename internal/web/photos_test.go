package web_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/auth"
	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/db/dbtest"
	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/media"
	"github.com/C0piIot/stratus-backend/internal/web"
)

// addPhoto stores an image and the row the indexer would have written for it.
func addPhoto(t *testing.T, s *files.Service, meta db.Store, name string, taken time.Time, camera string) db.File {
	t.Helper()
	f, err := s.Write(t.Context(), username, name, strings.NewReader("pixels"), 6, "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	if err := meta.PutMedia(t.Context(), db.Media{
		FileID: f.ID, Kind: db.KindImage, IndexedAt: time.Now(), Version: 1,
		TakenAt: taken, Camera: camera, Width: 4032, Height: 3024,
	}); err != nil {
		t.Fatal(err)
	}
	return f
}

var (
	// A tile links to the page about a photograph, which is its own address
	// with ?view -- the same address the WebDAV mount answers for.
	tileHref = regexp.MustCompile(`href="/photos/(\d{4}/\d{2}/[^"?]+)\?view"`)
	// A month's heading is a link into that month.
	heading = regexp.MustCompile(`<h2[^>]*><a[^>]*>([^<]+)</a></h2>`)
)

// tileNames is the filename of each tile, which is the last part of its date
// address.
func tileNames(body string) []string {
	var out []string
	for _, m := range tileHref.FindAllStringSubmatch(body, -1) {
		at := m[1]
		out = append(out, at[strings.LastIndex(at, "/")+1:])
	}
	return out
}

func headings(body string) []string {
	var out []string
	for _, m := range heading.FindAllStringSubmatch(body, -1) {
		out = append(out, m[1])
	}
	return out
}

func htmx(t *testing.T, h http.Handler, target string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	req.AddCookie(cookie)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestPhotosNeedASession(t *testing.T) {
	t.Parallel()
	h := newHandler(t, nil)
	for _, target := range []string{"/photos/", "/photos/2024/", "/photos/2024/06/a.jpg"} {
		rec := get(t, h, target)
		if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/login") {
			t.Errorf("%s with no session = %d %q, want the login form", target, rec.Code, rec.Header().Get("Location"))
		}
	}
}

// TestTheGalleryIsTheIndexNotTheTree: newest first by the camera's date,
// grouped by month, wherever each file was put -- and only images.
func TestTheGalleryIsTheIndexNotTheTree(t *testing.T) {
	t.Parallel()
	h, s, meta := browserOver(t)
	cookie := signIn(t, h)
	addPhoto(t, s, meta, "old.jpg", time.Date(2024, 5, 3, 10, 0, 0, 0, time.UTC), "")
	addPhoto(t, s, meta, "new.jpg", time.Date(2024, 6, 20, 10, 0, 0, 0, time.UTC), "")
	addPhoto(t, s, meta, "mid.jpg", time.Date(2024, 6, 2, 10, 0, 0, 0, time.UTC), "")
	song, err := s.Write(t.Context(), username, "song.flac", strings.NewReader("x"), 1, "audio/flac")
	if err != nil {
		t.Fatal(err)
	}
	if err := meta.PutMedia(t.Context(), db.Media{FileID: song.ID, Kind: db.KindAudio, IndexedAt: time.Now(), Version: 1}); err != nil {
		t.Fatal(err)
	}

	rec := get(t, h, "/photos/", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("grid = %d", rec.Code)
	}
	body := rec.Body.String()
	if got := tileNames(body); fmt.Sprint(got) != "[new.jpg mid.jpg old.jpg]" {
		t.Errorf("tiles = %v, want newest first and no track", got)
	}
	if got := headings(body); fmt.Sprint(got) != "[June 2024 May 2024]" {
		t.Errorf("headings = %v", got)
	}
	// A heading is the way into its month, which is what makes the grid and
	// the mount the same tree.
	if !strings.Contains(body, `href="/photos/2024/06/"`) {
		t.Errorf("a month's heading does not link to the month:\n%s", body)
	}
	if !strings.Contains(body, `/thumb/new.jpg?size=300&amp;v=`) {
		t.Errorf("no grid-sized thumbnail in\n%s", body)
	}
}

func TestAnEmptyGalleryExplainsItself(t *testing.T) {
	t.Parallel()
	h := newHandler(t, nil)
	body := get(t, h, "/photos/", signIn(t, h)).Body.String()
	if !strings.Contains(body, "No photos yet") {
		t.Errorf("empty gallery =\n%s", body)
	}
}

// TestAYearIsItsMonthsAndAMonthIsItsPhotos: the pages between the grid and a
// photograph, at the addresses the mount lists.
func TestAYearIsItsMonthsAndAMonthIsItsPhotos(t *testing.T) {
	t.Parallel()
	h, s, meta := browserOver(t)
	cookie := signIn(t, h)
	addPhoto(t, s, meta, "june.jpg", time.Date(2024, 6, 20, 10, 0, 0, 0, time.UTC), "")
	addPhoto(t, s, meta, "may.jpg", time.Date(2024, 5, 3, 10, 0, 0, 0, time.UTC), "")
	addPhoto(t, s, meta, "old.jpg", time.Date(2019, 1, 2, 10, 0, 0, 0, time.UTC), "")

	year := get(t, h, "/photos/2024/", cookie)
	if year.Code != http.StatusOK {
		t.Fatalf("a year = %d", year.Code)
	}
	for _, want := range []string{`href="/photos/2024/06/"`, "June 2024", `href="/photos/2024/05/"`, "May 2024"} {
		if !strings.Contains(year.Body.String(), want) {
			t.Errorf("the year page lacks %q", want)
		}
	}
	if strings.Contains(year.Body.String(), "January 2019") {
		t.Error("the year page lists another year's months")
	}

	month := get(t, h, "/photos/2024/06/", cookie)
	if month.Code != http.StatusOK {
		t.Fatalf("a month = %d", month.Code)
	}
	if got := tileNames(month.Body.String()); fmt.Sprint(got) != "[june.jpg]" {
		t.Errorf("the month holds %v", got)
	}
	// The page is titled with the month, so no cell heads it again.
	if got := headings(month.Body.String()); len(got) != 0 {
		t.Errorf("a month's page heads itself twice: %v", got)
	}
	// The trail back up, which is the only way the year page is reached.
	if !strings.Contains(month.Body.String(), `href="/photos/2024/"`) {
		t.Errorf("a month does not lead back to its year:\n%s", month.Body.String())
	}

	for _, missing := range []string{"/photos/1999/", "/photos/2024/07/", "/photos/2024/6/", "/photos/notes/"} {
		if rec := get(t, h, missing, cookie); rec.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", missing, rec.Code)
		}
	}
}

// TestAMonthPagesLikeTheGrid: a month of a camera roll is thousands, so it is
// cursor-paged the same way and not listed whole.
func TestAMonthPagesLikeTheGrid(t *testing.T) {
	t.Parallel()
	h, s, meta := browserOver(t)
	cookie := signIn(t, h)
	base := time.Date(2024, 6, 30, 12, 0, 0, 0, time.UTC)
	for i := range 105 {
		addPhoto(t, s, meta, fmt.Sprintf("p%03d.jpg", i), base.Add(-time.Duration(i)*time.Minute), "")
	}

	first := get(t, h, "/photos/2024/06/", cookie).Body.String()
	if got := tileNames(first); len(got) != 100 {
		t.Fatalf("the first page of a month has %d tiles", len(got))
	}
	next := regexp.MustCompile(`<a href="(/photos/2024/06/\?after=[^"]+)"`).FindStringSubmatch(first)
	if next == nil {
		t.Fatalf("a month of 105 offers no second page:\n%s", first)
	}
	rest := get(t, h, strings.ReplaceAll(next[1], "&amp;", "&"), cookie).Body.String()
	if got := tileNames(rest); len(got) != 5 {
		t.Errorf("the rest of the month has %v", got)
	}

	if rec := get(t, h, "/photos/2024/06/?after=nonsense", cookie); rec.Code != http.StatusBadRequest {
		t.Errorf("a month with a cursor that is not one = %d, want 400", rec.Code)
	}
}

// TestAPhotographsOwnAddressIsThePhotograph, like a file's is under /files/.
// This is also the GET a WebDAV client makes.
func TestAPhotographsOwnAddressIsThePhotograph(t *testing.T) {
	t.Parallel()
	h, s, meta := browserOver(t)
	cookie := signIn(t, h)
	f := addPhoto(t, s, meta, "IMG_0001.jpg", time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC), "")

	rec := get(t, h, "/photos/2024/06/IMG_0001.jpg", cookie)
	if rec.Code != http.StatusOK || rec.Body.String() != "pixels" {
		t.Fatalf("GET = %d %q, want the original", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("ETag"); got == "" || !strings.Contains(got, f.ETag) {
		t.Errorf("ETag = %q, want the file's %q", got, f.ETag)
	}
}

// TestTheGalleryPages: a hundred per page, the rest by htmx or by a plain link,
// and a month that crosses the boundary headed once.
func TestTheGalleryPages(t *testing.T) {
	t.Parallel()
	h, s, meta := browserOver(t)
	cookie := signIn(t, h)
	base := time.Date(2024, 6, 30, 12, 0, 0, 0, time.UTC)
	for i := range 105 {
		addPhoto(t, s, meta, fmt.Sprintf("p%03d.jpg", i), base.Add(-time.Duration(i)*time.Hour), "")
	}

	first := get(t, h, "/photos/", cookie).Body.String()
	if got := tileNames(first); len(got) != 100 || got[0] != "p000.jpg" {
		t.Fatalf("first page has %d tiles starting %v", len(got), got[:1])
	}
	next := regexp.MustCompile(`hx-get="([^"]+)"`).FindStringSubmatch(first)
	if next == nil {
		t.Fatal("no next page on the first")
	}
	target := strings.ReplaceAll(next[1], "&amp;", "&")
	if !strings.Contains(first, `<a href="`+next[1]+`"`) {
		t.Error("the next page is not also a plain link")
	}

	rest := htmx(t, h, target, cookie)
	body := rest.Body.String()
	if got := tileNames(body); len(got) != 5 || got[0] != "p100.jpg" {
		t.Errorf("the fragment has %v", got)
	}
	if strings.Contains(body, "<html") {
		t.Error("the fragment is a whole page")
	}
	if got := headings(body); len(got) != 0 {
		t.Errorf("the fragment heads June again: %v", got)
	}
	if strings.Contains(body, "hx-get") {
		t.Error("the last page offers another")
	}

	// The same URL without htmx is a page of its own, and a page starts with
	// its month.
	if got := headings(get(t, h, target, cookie).Body.String()); fmt.Sprint(got) != "[June 2024]" {
		t.Errorf("the second page as a page is headed %v", got)
	}
}

func TestTheViewer(t *testing.T) {
	t.Parallel()
	h, s, meta := browserOver(t)
	cookie := signIn(t, h)
	addPhoto(t, s, meta, "a.jpg", time.Date(2024, 6, 3, 9, 0, 0, 0, time.UTC), "")
	addPhoto(t, s, meta, "b.jpg", time.Date(2024, 6, 2, 9, 30, 0, 0, time.UTC), "Apple iPhone 15")
	addPhoto(t, s, meta, "c.jpg", time.Date(2024, 6, 1, 9, 0, 0, 0, time.UTC), "")

	rec := get(t, h, "/photos/2024/06/b.jpg?view", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("viewer = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`href="/photos/2024/06/a.jpg?view" rel="prev"`,
		`href="/photos/2024/06/c.jpg?view" rel="next"`,
		`/thumb/b.jpg?size=1200&amp;v=`,
		`href="/files/b.jpg"`,
		"Taken", "2 June 2024, 09:30", "Apple iPhone 15", "4032 × 3024",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("viewer lacks %q", want)
		}
	}
	// Back to the grid page that starts with this photo.
	if !regexp.MustCompile(`href="/photos/\?after=\d+\.\d+">All photos`).MatchString(body) {
		t.Error("the way back is not to this photo's page")
	}

	// At the ends there is nothing to go to.
	newest := get(t, h, "/photos/2024/06/a.jpg?view", cookie).Body.String()
	if !strings.Contains(newest, `aria-disabled="true">Newer`) || !strings.Contains(newest, `href="/photos/">All photos`) {
		t.Errorf("the newest photo offers a newer one or a way back mid-grid")
	}
	if !strings.Contains(get(t, h, "/photos/2024/06/c.jpg?view", cookie).Body.String(), `aria-disabled="true">Older`) {
		t.Error("the oldest photo offers an older one")
	}
}

// TestTheViewerIsReachableFromAPathInTheTree: the search results are files and
// have no date to build an address from, so they come through here.
func TestTheViewerIsReachableFromAPathInTheTree(t *testing.T) {
	t.Parallel()
	h, s, meta := browserOver(t)
	cookie := signIn(t, h)
	addPhoto(t, s, meta, "IMG_0001.jpg", time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC), "")

	rec := get(t, h, "/photos/?file=IMG_0001.jpg", cookie)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("by path = %d, want a redirect to the one address it has", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/photos/2024/06/IMG_0001.jpg?view" {
		t.Errorf("Location = %q", got)
	}
	if _, err := s.Write(t.Context(), username, "notes.txt", strings.NewReader("x"), 1, "text/plain"); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"/photos/?file=nothing.jpg", "/photos/?file=notes.txt"} {
		if rec := get(t, h, target, cookie); rec.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", target, rec.Code)
		}
	}
	if rec := get(t, h, "/photos/?file=%ff.jpg", cookie); rec.Code < 400 {
		t.Errorf("a path that is not UTF-8 = %d", rec.Code)
	}
}

// TestAScreenshotSaysWhenItArrived: no camera, so the date is the file's and
// the page says so rather than claiming it was taken then.
func TestAScreenshotSaysWhenItArrived(t *testing.T) {
	t.Parallel()
	h, s, meta := browserOver(t)
	cookie := signIn(t, h)
	f := addPhoto(t, s, meta, "shot.png", time.Time{}, "")

	at := f.MTime.UTC()
	body := get(t, h, fmt.Sprintf("/photos/%04d/%02d/shot.png?view", at.Year(), at.Month()), cookie).Body.String()
	if !strings.Contains(body, "Added") || strings.Contains(body, "Taken") {
		t.Errorf("a screenshot's viewer =\n%s", body)
	}
}

func TestWhatIsNotAPhotographIsNotFound(t *testing.T) {
	t.Parallel()
	h, s, _ := browserOver(t)
	cookie := signIn(t, h)
	if _, err := s.Write(t.Context(), username, "notes.txt", strings.NewReader("x"), 1, "text/plain"); err != nil {
		t.Fatal(err)
	}

	for _, target := range []string{"/photos/notes.txt", "/photos/2024/06/missing.jpg", "/photos/2024/06/a.jpg/x"} {
		if rec := get(t, h, target, cookie); rec.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", target, rec.Code)
		}
	}
	for _, bad := range []string{"nonsense", "123", "123.x", "123.0"} {
		if rec := get(t, h, "/photos/?after="+bad, cookie); rec.Code != http.StatusBadRequest {
			t.Errorf("after=%s = %d, want 400", bad, rec.Code)
		}
	}
}

// TestABrokenIndexIsNotAnEmptyGallery: a database that fails is an error page,
// never a gallery with nothing in it or a photo that is not there.
func TestABrokenIndexIsNotAnEmptyGallery(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ call, target string }{
		{"PhotoTimeline", "/photos/"},
		{"PhotoMonths", "/photos/2024/"},
		{"PhotoAround", "/photos/2024/06/a.jpg?view"},
		{"FileByPath", "/photos/?file=a.jpg"},
		// The same address again, broken one layer further in: the file is
		// found and the month it would be named in is not.
		{"PhotoTimeline", "/photos/?file=a.jpg"},
	} {
		t.Run(tc.call, func(t *testing.T) {
			t.Parallel()
			blobs, meta := backends(t)
			s := files.New(blobs, meta)
			addPhoto(t, s, meta, "a.jpg", time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC), "")

			broken := dbtest.FailOn(t, meta, tc.call)
			creds := credentials()
			h := web.Handler(version, buildDate, creds, auth.NewSessions(creds, auth.DefaultSessionTTL), auth.NewShares(creds),
				files.New(blobs, broken), media.NewThumbs(blobs, s, "ffmpeg", t.TempDir()), broken, indexing(meta), nil, web.Video{})
			if rec := get(t, h, tc.target, signIn(t, h)); rec.Code != http.StatusInternalServerError {
				t.Errorf("%s with %s broken = %d, want 500", tc.target, tc.call, rec.Code)
			}
		})
	}
}

// TestAnIndexThatBreaksWhileNamingIsNotAHalfPage: a page reads the timeline and
// then the month each photograph is in, to name it the way the mount does. The
// second read failing is a page that cannot be built, not one built with links
// that go nowhere.
func TestAnIndexThatBreaksWhileNamingIsNotAHalfPage(t *testing.T) {
	t.Parallel()
	for _, target := range []string{"/photos/", "/photos/2024/06/"} {
		t.Run(target, func(t *testing.T) {
			t.Parallel()
			blobs, meta := backends(t)
			s := files.New(blobs, meta)
			addPhoto(t, s, meta, "a.jpg", time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC), "")

			broken := dbtest.FailAfter(t, meta, "PhotoTimeline", 1)
			creds := credentials()
			h := web.Handler(version, buildDate, creds, auth.NewSessions(creds, auth.DefaultSessionTTL), auth.NewShares(creds),
				files.New(blobs, broken), media.NewThumbs(blobs, s, "ffmpeg", t.TempDir()), broken, indexing(meta), nil, web.Video{})
			if rec := get(t, h, target, signIn(t, h)); rec.Code != http.StatusInternalServerError {
				t.Errorf("%s with the month read broken = %d, want 500", target, rec.Code)
			}
		})
	}
}

// TestAViewerThatCannotNameItsNeighbourIsAnError: the photographs either side
// are in other months, so naming them is another read -- and a page offering a
// link it could not build would be worse than no page.
func TestAViewerThatCannotNameItsNeighbourIsAnError(t *testing.T) {
	t.Parallel()
	blobs, meta := backends(t)
	s := files.New(blobs, meta)
	addPhoto(t, s, meta, "july.jpg", time.Date(2024, 7, 1, 0, 0, 0, 0, time.UTC), "")
	addPhoto(t, s, meta, "june.jpg", time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC), "")
	addPhoto(t, s, meta, "may.jpg", time.Date(2024, 5, 1, 0, 0, 0, 0, time.UTC), "")

	// One read names the month the viewer is in; the neighbours' months are
	// the ones after it.
	broken := dbtest.FailAfter(t, meta, "PhotoTimeline", 1)
	creds := credentials()
	h := web.Handler(version, buildDate, creds, auth.NewSessions(creds, auth.DefaultSessionTTL), auth.NewShares(creds),
		files.New(blobs, broken), media.NewThumbs(blobs, s, "ffmpeg", t.TempDir()), broken, indexing(meta), nil, web.Video{})
	if rec := get(t, h, "/photos/2024/06/june.jpg?view", signIn(t, h)); rec.Code != http.StatusInternalServerError {
		t.Errorf("the viewer with the neighbours' month broken = %d, want 500", rec.Code)
	}
}

func TestAPathThatIsNotOneIsRefused(t *testing.T) {
	t.Parallel()
	h := newHandler(t, nil)
	if rec := get(t, h, "/photos/%ff.jpg", signIn(t, h)); rec.Code < 400 {
		t.Errorf("a path that is not UTF-8 = %d", rec.Code)
	}
}

// TestAPhotoWithNoPictureStillHasATile: a file the thumbnailer cannot read
// gets its name in the cell rather than a broken image.
func TestAPhotoWithNoPictureStillHasATile(t *testing.T) {
	t.Parallel()
	h, s, meta := browserOver(t)
	cookie := signIn(t, h)
	addPhoto(t, s, meta, "scan.tiff", time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC), "")

	grid := get(t, h, "/photos/", cookie).Body.String()
	if strings.Contains(grid, "/thumb/scan.tiff") || !strings.Contains(grid, ">scan.tiff</span>") {
		t.Errorf("a tile with no picture =\n%s", grid)
	}
	if viewer := get(t, h, "/photos/2024/06/scan.tiff?view", cookie).Body.String(); !strings.Contains(viewer, "no picture of this file") {
		t.Error("the viewer does not say there is no picture")
	}
}
