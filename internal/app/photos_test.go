package app_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/app"
	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/db/sqlite"
	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/media"
	"github.com/C0piIot/stratus-backend/internal/storage/disk"
)

// TestMusicIsOneAddressAndTwoProtocols is the music half of #279, and like
// the photographs' it can only be asserted here: the split is made in this
// package, so neither adapter's own tests can see that the browser's GET and
// the client's PROPFIND land on the same generated name.
func TestMusicIsOneAddressAndTwoProtocols(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	blobs, err := disk.New(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blobs.Close() })
	meta, err := sqlite.New(t.Context(), filepath.Join(dir, "stratus.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = meta.Close() })
	if err = meta.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	service := files.New(blobs, meta)
	f, err := service.Write(t.Context(), "edu", "01.flac", strings.NewReader("sound"), 5, "audio/flac")
	if err != nil {
		t.Fatal(err)
	}
	// A slash in the tag, because that is the case the two halves have to
	// agree about: a collection cannot hold one and a link must not escape it.
	if err = meta.PutMedia(t.Context(), db.Media{
		FileID: f.ID, Kind: db.KindAudio, IndexedAt: time.Now(), Version: media.Version,
		AlbumArtist: "AC/DC", Artist: "AC/DC", Album: "High Voltage", Title: "It's a Long Way",
		TrackNo: 1, DurationMS: 301_000,
	}); err != nil {
		t.Fatal(err)
	}

	const password = "an example password"
	cfg := runConfig(t, map[string]string{"STRATUS_USERNAME": "edu", "STRATUS_PASSWORD": password})
	h := app.New(cfg, "test", "2026-01-01T09:30:00Z").Handler(app.Deps{
		Storage: blobs, Database: meta, Files: service,
		Thumbs: media.NewThumbs(blobs, service, "ffmpeg", t.TempDir()),
	})

	const at = "/music/AC_DC/High%20Voltage/01.flac"

	client := func(method, target string, headers ...string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequestWithContext(t.Context(), method, target, nil)
		req.SetBasicAuth("edu", password)
		for i := 0; i+1 < len(headers); i += 2 {
			req.Header.Set(headers[i], headers[i+1])
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// The root collection leads here, which is the point of the whole issue.
	root := client("PROPFIND", "/", "Depth", "1")
	if !strings.Contains(root.Body.String(), "/music/") {
		t.Errorf("the root does not list the music:\n%s", root.Body.String())
	}

	album := client("PROPFIND", "/music/AC_DC/High%20Voltage/", "Depth", "1")
	if album.Code != http.StatusMultiStatus {
		t.Fatalf("PROPFIND of an album = %d, want 207", album.Code)
	}
	if !strings.Contains(album.Body.String(), at) {
		t.Errorf("the album does not list %s:\n%s", at, album.Body.String())
	}

	bytes := client(http.MethodGet, at)
	if bytes.Code != http.StatusOK || bytes.Body.String() != "sound" {
		t.Fatalf("GET %s = %d %q, want the original", at, bytes.Code, bytes.Body.String())
	}

	form := url.Values{"username": {"edu"}, "password": {password}}
	login := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/login", strings.NewReader(form.Encode()))
	login.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, login)
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatalf("signing in = %d, no cookie", rec.Code)
	}
	page := func(target string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
		req.AddCookie(cookies[0])
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	shown := page("/music/AC_DC/High%20Voltage/")
	if shown.Code != http.StatusOK || !strings.Contains(shown.Body.String(), "<html") {
		t.Fatalf("the album as a page = %d", shown.Code)
	}
	// The same name the multistatus gave, which is what sharing
	// internal/music between the two halves is for.
	if !strings.Contains(shown.Body.String(), at) {
		t.Errorf("the page does not play %s:\n%s", at, shown.Body.String())
	}
}

// TestPhotosAreOneAddressAndTwoProtocols is the whole of step 2 of #279, and
// it can only be asserted here: the split is made in this package, so neither
// adapter's own tests can see that the browser's GET and the client's PROPFIND
// land on the same name.
func TestPhotosAreOneAddressAndTwoProtocols(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	blobs, err := disk.New(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blobs.Close() })
	meta, err := sqlite.New(t.Context(), filepath.Join(dir, "stratus.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = meta.Close() })
	if err = meta.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	service := files.New(blobs, meta)
	f, err := service.Write(t.Context(), "edu", "IMG_0001.jpg", strings.NewReader("pixels"), 6, "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	if err = meta.PutMedia(t.Context(), db.Media{
		FileID: f.ID, Kind: db.KindImage, IndexedAt: time.Now(), Version: media.Version,
		TakenAt: time.Date(2024, 6, 15, 10, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}

	const password = "an example password"
	cfg := runConfig(t, map[string]string{"STRATUS_USERNAME": "edu", "STRATUS_PASSWORD": password})
	h := app.New(cfg, "test", "2026-01-01T09:30:00Z").Handler(app.Deps{
		Storage: blobs, Database: meta, Files: service,
		Thumbs: media.NewThumbs(blobs, service, "ffmpeg", t.TempDir()),
	})

	const at = "/photos/2024/06/IMG_0001.jpg"

	// A WebDAV client: Basic, and no Sec-Fetch header, which is what tells the
	// server it is not a browser.
	client := func(method, target string, headers ...string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequestWithContext(t.Context(), method, target, nil)
		req.SetBasicAuth("edu", password)
		for i := 0; i+1 < len(headers); i += 2 {
			req.Header.Set(headers[i], headers[i+1])
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	listing := client("PROPFIND", "/photos/2024/06/", "Depth", "1")
	if listing.Code != http.StatusMultiStatus {
		t.Fatalf("PROPFIND of a month = %d, want 207", listing.Code)
	}
	if !strings.Contains(listing.Body.String(), at) {
		t.Errorf("the month does not list %s:\n%s", at, listing.Body.String())
	}

	bytes := client(http.MethodGet, at)
	if bytes.Code != http.StatusOK || bytes.Body.String() != "pixels" {
		t.Fatalf("GET %s = %d %q, want the original", at, bytes.Code, bytes.Body.String())
	}

	// And a browser, at the same two addresses: the month is a page and the
	// photograph's own URL with ?view is the page about it.
	form := url.Values{"username": {"edu"}, "password": {password}}
	login := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/login", strings.NewReader(form.Encode()))
	login.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, login)
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatalf("signing in = %d, no cookie", rec.Code)
	}

	page := func(target string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
		req.AddCookie(cookies[0])
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	month := page("/photos/2024/06/")
	if month.Code != http.StatusOK || !strings.Contains(month.Body.String(), "<html") {
		t.Fatalf("the month as a page = %d", month.Code)
	}
	// The same name the multistatus gave, which is the point of sharing
	// internal/photos between the two.
	if !strings.Contains(month.Body.String(), at+"?view") {
		t.Errorf("the page does not link to %s:\n%s", at, month.Body.String())
	}
	if viewer := page(at + "?view"); viewer.Code != http.StatusOK || !strings.Contains(viewer.Body.String(), "IMG_0001.jpg") {
		t.Errorf("the viewer = %d", viewer.Code)
	}
}
