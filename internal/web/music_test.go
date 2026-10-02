package web_test

import (
	htmlstd "html"
	"net/http"
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

// addTrack stores an audio file and the row the indexer would have written.
func addTrack(t *testing.T, s *files.Service, meta db.Store, name string, m db.Media) db.File {
	t.Helper()
	f, err := s.Write(t.Context(), username, name, strings.NewReader("sound"), 5, "audio/flac")
	if err != nil {
		t.Fatal(err)
	}
	m.FileID, m.Kind, m.IndexedAt, m.Version = f.ID, db.KindAudio, time.Now(), 1
	if err := meta.PutMedia(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	return f
}

func homework(t *testing.T, s *files.Service, meta db.Store) (db.File, db.File) {
	t.Helper()
	one := addTrack(t, s, meta, "01.flac", db.Media{
		AlbumArtist: "AC/DC", Artist: "AC/DC", Album: "High Voltage", Title: "It's a Long Way",
		TrackNo: 1, Year: 1976, Genre: "Rock", DurationMS: 301_000,
	})
	two := addTrack(t, s, meta, "02.flac", db.Media{
		AlbumArtist: "AC/DC", Artist: "AC/DC", Album: "High Voltage",
		TrackNo: 2, Year: 1976, DurationMS: 250_000,
	})
	return one, two
}

func TestMusicNeedsASession(t *testing.T) {
	t.Parallel()
	h := newHandler(t, nil)
	for _, target := range []string{"/music", "/music/a", "/music/a/b"} {
		rec := get(t, h, target)
		if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/login") {
			t.Errorf("%s with no session = %d %q, want the login form", target, rec.Code, rec.Header().Get("Location"))
		}
	}
}

// TestAnAlbumCanBeOpenedAndPlayed is #212's acceptance: from the artists to
// an album, each track an audio element over the file itself, and a play
// counted through OpenSubsonic's own scrobble.
func TestAnAlbumCanBeOpenedAndPlayed(t *testing.T) {
	t.Parallel()
	h, s, meta := browserOver(t)
	cookie := signIn(t, h)
	one, _ := homework(t, s, meta)
	if err := meta.Star(t.Context(), username, db.ArtistSubject("AC/DC"), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := meta.SetRating(t.Context(), username, db.AlbumSubject("AC/DC", "High Voltage"), 4); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := meta.RecordPlay(t.Context(), username, one.ID, time.Now()); err != nil {
			t.Fatal(err)
		}
	}

	artists := get(t, h, "/music", cookie).Body.String()
	// One segment, however many slashes the name has.
	for _, want := range []string{`href="/music/AC%2FDC"`, "1 album<", "♥"} {
		if !strings.Contains(artists, want) {
			t.Errorf("the artists page has no %q:\n%s", want, artists)
		}
	}

	artist := get(t, h, "/music/AC%2FDC", cookie).Body.String()
	for _, want := range []string{`href="/music/AC%2FDC/High%20Voltage"`, `src="/music/AC%2FDC/High%20Voltage/cover?size=300"`, "1976"} {
		if !strings.Contains(artist, want) {
			t.Errorf("the artist page has no %q:\n%s", want, artist)
		}
	}

	rec := get(t, h, "/music/AC%2FDC/High%20Voltage", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("the album = %d", rec.Code)
	}
	album := htmlstd.UnescapeString(rec.Body.String())
	for _, want := range []string{
		`src="/files/01.flac"`,
		`hx-get="/rest/scrobble?c=stratus-web&id=tr-`,
		`hx-trigger="ended"`,
		"It's a Long Way", "02.flac", "5:01", "9:11", "Rock", "★★★★☆", "3 plays",
	} {
		if !strings.Contains(album, want) {
			t.Errorf("the album has no %q:\n%s", want, album)
		}
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "media-src 'self'") {
		t.Errorf("the album's policy blocks its own audio: %q", csp)
	}
}

func TestMusicThatIsNotThereIsNotFound(t *testing.T) {
	t.Parallel()
	h, s, meta := browserOver(t)
	cookie := signIn(t, h)
	homework(t, s, meta)
	for _, target := range []string{"/music/Nobody", "/music/AC%2FDC/Nothing"} {
		if rec := get(t, h, target, cookie); rec.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", target, rec.Code)
		}
	}
}

func TestAnEmptyLibrarySaysSo(t *testing.T) {
	t.Parallel()
	h := newHandler(t, nil)
	if body := get(t, h, "/music", signIn(t, h)).Body.String(); !strings.Contains(body, "No music yet") {
		t.Errorf("an empty library =\n%s", body)
	}
}

// TestABrokenIndexIsNotAnEmptyLibrary, for the reason the gallery's is.
func TestABrokenIndexIsNotAnEmptyLibrary(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ call, target string }{
		{"Artists", "/music"},
		{"AnnotationsOf", "/music"},
		{"Albums", "/music/AC%2FDC"},
		{"Tracks", "/music/AC%2FDC/High%20Voltage"},
		{"AnnotationsOf", "/music/AC%2FDC/High%20Voltage"},
		{"Tracks", "/music/AC%2FDC/High%20Voltage/cover"},
		{"ListFiles", "/music/AC%2FDC/High%20Voltage/cover"},
	} {
		t.Run(tc.call+tc.target, func(t *testing.T) {
			t.Parallel()
			blobs, meta := backends(t)
			s := files.New(blobs, meta)
			homework(t, s, meta)

			broken := dbtest.FailOn(t, meta, tc.call)
			creds := credentials()
			h := web.Handler(version, buildDate, creds, auth.NewSessions(creds, auth.DefaultSessionTTL), auth.NewShares(creds),
				s, media.NewThumbs(blobs, files.New(blobs, broken), "ffmpeg", t.TempDir()), broken, indexing(meta), nil, web.Video{})
			if rec := get(t, h, tc.target, signIn(t, h)); rec.Code != http.StatusInternalServerError {
				t.Errorf("%s with %s broken = %d, want 500", tc.target, tc.call, rec.Code)
			}
		})
	}
}

// TestAnAlbumHasItsFoldersCover: the picture beside the first track, which is
// where getCoverArt looks too; and nothing, rather than an error, for an album
// that has none.
func TestAnAlbumHasItsFoldersCover(t *testing.T) {
	t.Parallel()
	h, s, meta := browserOver(t)
	cookie := signIn(t, h)
	homework(t, s, meta)
	addTrack(t, s, meta, "Bare.flac", db.Media{AlbumArtist: "Nobody", Album: "Bare", Title: "One"})

	if rec := get(t, h, "/music/Nobody/Bare/cover", cookie); rec.Code != http.StatusNotFound {
		t.Errorf("an album with no picture = %d, want 404", rec.Code)
	}
	if rec := get(t, h, "/music/Nobody/Nothing/cover", cookie); rec.Code != http.StatusNotFound {
		t.Errorf("no such album = %d, want 404", rec.Code)
	}

	write(t, s, "cover.jpg", photoJPEG(t))
	rec := get(t, h, "/music/AC%2FDC/High%20Voltage/cover?size=96", cookie)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/jpeg" {
		t.Errorf("the folder's cover = %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
}
