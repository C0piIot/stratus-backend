package app_test

import (
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
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

var scrobbleCall = regexp.MustCompile(`hx-get="([^"]+)"`)

// TestAPlayFromTheWebUICounts holds the two adapters together where nothing
// else can: internal/web writes OpenSubsonic's song id out by hand, because
// adapters do not import each other, so this follows the call an album page
// makes to /rest/ and checks that it counted (#212).
func TestAPlayFromTheWebUICounts(t *testing.T) {
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
	f, err := service.Write(t.Context(), "edu", "song.flac", strings.NewReader("sound"), 5, "audio/flac")
	if err != nil {
		t.Fatal(err)
	}
	if err = meta.PutMedia(t.Context(), db.Media{
		FileID: f.ID, Kind: db.KindAudio, IndexedAt: time.Now(), Version: media.Version,
		AlbumArtist: "Artist", Artist: "Artist", Album: "Album", Title: "Song", DurationMS: 1000,
	}); err != nil {
		t.Fatal(err)
	}

	const password = "an example password"
	cfg := runConfig(t, map[string]string{"STRATUS_USERNAME": "edu", "STRATUS_PASSWORD": password})
	h := app.New(cfg, "test", "2026-01-01T09:30:00Z").Handler(app.Deps{
		Storage: blobs, Database: meta, Files: service,
		Thumbs: media.NewThumbs(blobs, service, "ffmpeg", t.TempDir()),
	})

	form := url.Values{"username": {"edu"}, "password": {password}}
	login := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/login", strings.NewReader(form.Encode()))
	login.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, login)
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatalf("signing in = %d, no cookie", rec.Code)
	}

	fromThePage := func(target string) string {
		t.Helper()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
		req.AddCookie(cookies[0])
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Body.String()
	}

	match := scrobbleCall.FindStringSubmatch(fromThePage("/music/Artist/Album"))
	if match == nil {
		t.Fatal("the album page makes no call when a track ends")
	}
	if body := fromThePage(html.UnescapeString(match[1])); !strings.Contains(body, `status="ok"`) {
		t.Fatalf("the page's scrobble = %s", body)
	}
	notes, err := meta.AnnotationsOf(t.Context(), "edu", []db.Subject{db.TrackSubject(f.ID)})
	if err != nil {
		t.Fatal(err)
	}
	if got := notes[db.TrackSubject(f.ID)].PlayCount; got != 1 {
		t.Errorf("play count after the page's scrobble = %d, want 1", got)
	}
}
