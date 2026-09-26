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
	tileHref = regexp.MustCompile(`href="/gallery/photos/([^"]+)"`)
	heading  = regexp.MustCompile(`<h2[^>]*>([^<]+)</h2>`)
)

func tileNames(body string) []string {
	var out []string
	for _, m := range tileHref.FindAllStringSubmatch(body, -1) {
		out = append(out, m[1])
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
	for _, target := range []string{"/gallery/photos", "/gallery/photos/a.jpg"} {
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

	rec := get(t, h, "/gallery/photos", cookie)
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
	if !strings.Contains(body, `/thumb/new.jpg?size=300&amp;v=`) {
		t.Errorf("no grid-sized thumbnail in\n%s", body)
	}
	if !strings.Contains(body, `href="/gallery/photos"`) {
		t.Error("the header does not link to the gallery")
	}
}

func TestAnEmptyGalleryExplainsItself(t *testing.T) {
	t.Parallel()
	h := newHandler(t, nil)
	body := get(t, h, "/gallery/photos", signIn(t, h)).Body.String()
	if !strings.Contains(body, "No photos yet") {
		t.Errorf("empty gallery =\n%s", body)
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

	first := get(t, h, "/gallery/photos", cookie).Body.String()
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

	rec := get(t, h, "/gallery/photos/b.jpg", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("viewer = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`href="/gallery/photos/a.jpg" rel="prev"`,
		`href="/gallery/photos/c.jpg" rel="next"`,
		`/thumb/b.jpg?size=1200&amp;v=`,
		`href="/files/b.jpg"`,
		"Taken", "2 June 2024, 09:30", "Apple iPhone 15", "4032 × 3024",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("viewer lacks %q", want)
		}
	}
	// Back to the grid page that starts with this photo.
	if !regexp.MustCompile(`href="/gallery/photos\?after=\d+\.\d+">All photos`).MatchString(body) {
		t.Error("the way back is not to this photo's page")
	}

	// At the ends there is nothing to go to.
	newest := get(t, h, "/gallery/photos/a.jpg", cookie).Body.String()
	if !strings.Contains(newest, `aria-disabled="true">Newer`) || !strings.Contains(newest, `href="/gallery/photos">All photos`) {
		t.Errorf("the newest photo offers a newer one or a way back mid-grid")
	}
	if !strings.Contains(get(t, h, "/gallery/photos/c.jpg", cookie).Body.String(), `aria-disabled="true">Older`) {
		t.Error("the oldest photo offers an older one")
	}
}

// TestAScreenshotSaysWhenItArrived: no camera, so the date is the file's and
// the page says so rather than claiming it was taken then.
func TestAScreenshotSaysWhenItArrived(t *testing.T) {
	t.Parallel()
	h, s, meta := browserOver(t)
	cookie := signIn(t, h)
	addPhoto(t, s, meta, "shot.png", time.Time{}, "")

	body := get(t, h, "/gallery/photos/shot.png", cookie).Body.String()
	if !strings.Contains(body, "Added") || strings.Contains(body, "Taken") {
		t.Errorf("a screenshot's viewer =\n%s", body)
	}
}

func TestTheViewerRefusesWhatIsNotAPhoto(t *testing.T) {
	t.Parallel()
	h, s, _ := browserOver(t)
	cookie := signIn(t, h)
	if _, err := s.Write(t.Context(), username, "notes.txt", strings.NewReader("x"), 1, "text/plain"); err != nil {
		t.Fatal(err)
	}

	for _, target := range []string{"/gallery/photos/notes.txt", "/gallery/photos/missing.jpg"} {
		if rec := get(t, h, target, cookie); rec.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", target, rec.Code)
		}
	}
	if rec := get(t, h, "/gallery/photos/", cookie); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/gallery/photos" {
		t.Errorf("/gallery/photos/ = %d %q, want the grid", rec.Code, rec.Header().Get("Location"))
	}
	for _, bad := range []string{"nonsense", "123", "123.x", "123.0"} {
		if rec := get(t, h, "/gallery/photos?after="+bad, cookie); rec.Code != http.StatusBadRequest {
			t.Errorf("after=%s = %d, want 400", bad, rec.Code)
		}
	}
}

// TestABrokenIndexIsNotAnEmptyGallery: a database that fails is an error page,
// never a gallery with nothing in it or a photo that is not there.
func TestABrokenIndexIsNotAnEmptyGallery(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ call, target string }{
		{"PhotoTimeline", "/gallery/photos"},
		{"PhotoAround", "/gallery/photos/a.jpg"},
		{"FileByPath", "/gallery/photos/a.jpg"},
	} {
		t.Run(tc.call, func(t *testing.T) {
			t.Parallel()
			blobs, meta := backends(t)
			s := files.New(blobs, meta)
			addPhoto(t, s, meta, "a.jpg", time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC), "")

			broken := dbtest.FailOn(t, meta, tc.call)
			creds := credentials()
			h := web.Handler(version, buildDate, creds, auth.NewSessions(creds, auth.DefaultSessionTTL), auth.NewShares(creds),
				files.New(blobs, broken), media.NewThumbs(blobs, s, "ffmpeg", t.TempDir()), broken, indexing(meta))
			if rec := get(t, h, tc.target, signIn(t, h)); rec.Code != http.StatusInternalServerError {
				t.Errorf("%s with %s broken = %d, want 500", tc.target, tc.call, rec.Code)
			}
		})
	}
}

func TestAPathThatIsNotOneIsRefused(t *testing.T) {
	t.Parallel()
	h := newHandler(t, nil)
	if rec := get(t, h, "/gallery/photos/%ff.jpg", signIn(t, h)); rec.Code < 400 {
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

	grid := get(t, h, "/gallery/photos", cookie).Body.String()
	if strings.Contains(grid, "/thumb/scan.tiff") || !strings.Contains(grid, ">scan.tiff</span>") {
		t.Errorf("a tile with no picture =\n%s", grid)
	}
	if viewer := get(t, h, "/gallery/photos/scan.tiff", cookie).Body.String(); !strings.Contains(viewer, "no picture of this file") {
		t.Error("the viewer does not say there is no picture")
	}
}
