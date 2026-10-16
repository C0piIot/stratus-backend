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

// TestTheStatusPageKnowsWhetherLookupsAreOn is the wiring and nothing else
// (#217): the page reads the rows and the setting through what the
// composition root hands it, and a root that forgot either would show a
// library nobody is asking about as one with nothing to say.
func TestTheStatusPageKnowsWhetherLookupsAreOn(t *testing.T) {
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
	for _, tc := range []struct {
		name  string
		vars  map[string]string
		shown bool
	}{
		{"off", map[string]string{}, false},
		{"on", map[string]string{"STRATUS_MUSIC_LOOKUP": "on"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			vars := map[string]string{"STRATUS_USERNAME": "edu", "STRATUS_PASSWORD": password}
			for k, v := range tc.vars {
				vars[k] = v
			}
			cfg := runConfig(t, vars)
			h := app.New(cfg, "test", "2026-01-01T09:30:00Z").Handler(app.Deps{
				Storage: blobs, Database: meta, Files: service,
				Thumbs: media.NewThumbs(blobs, service, meta, "ffmpeg", t.TempDir()),
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

			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/status", nil)
			req.AddCookie(cookies[0])
			req.Header.Set("Sec-Fetch-Mode", "navigate")
			rec = httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET /status = %d", rec.Code)
			}
			if got := strings.Contains(rec.Body.String(), "Albums identified"); got != tc.shown {
				t.Errorf("the status page reports lookups: %v, want %v with the setting %s", got, tc.shown, tc.name)
			}
		})
	}
}
