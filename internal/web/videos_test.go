package web_test

import (
	htmlstd "html"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/files"
)

// addVideo stores a recording and the row the indexer would have written for
// it. Unlike a photograph it carries a duration, which the grid shows.
func addVideo(t *testing.T, s *files.Service, meta db.Store, name string, taken time.Time, ms int64) db.File {
	t.Helper()
	f, err := s.Write(t.Context(), username, name, strings.NewReader("frames"), 6, "video/mp4")
	if err != nil {
		t.Fatal(err)
	}
	if err := meta.PutMedia(t.Context(), db.Media{
		FileID: f.ID, Kind: db.KindVideo, IndexedAt: time.Now(), Version: 1,
		TakenAt: taken, DurationMS: ms, Width: 1920, Height: 1080,
	}); err != nil {
		t.Fatal(err)
	}
	return f
}

var videoDay = time.Date(2024, 6, 15, 10, 0, 0, 0, time.UTC)

// TestTheTwoLibrariesDoNotMix is #215: every video in one place and none in
// the gallery, which is the decision the issue was open on.
func TestTheTwoLibrariesDoNotMix(t *testing.T) {
	t.Parallel()
	h, s, meta := browserOver(t)
	cookie := signIn(t, h)
	addPhoto(t, s, meta, "still.jpg", videoDay, "")
	addVideo(t, s, meta, "clip.mp4", videoDay.Add(time.Hour), 42_000)

	videos := get(t, h, "/videos/", cookie)
	if videos.Code != http.StatusOK {
		t.Fatalf("the video grid = %d", videos.Code)
	}
	if got := tileNames(videos.Body.String()); strings.Join(got, ",") != "clip.mp4" {
		t.Errorf("the videos hold %v, want the recording alone", got)
	}
	if got := tileNames(get(t, h, "/photos/", cookie).Body.String()); strings.Join(got, ",") != "still.jpg" {
		t.Errorf("the gallery holds %v, want the photograph alone", got)
	}
	// A recording is not at a photograph's address and the other way round.
	if rec := get(t, h, "/photos/2024/06/clip.mp4", cookie); rec.Code != http.StatusNotFound {
		t.Errorf("the recording answers under /photos/: %d", rec.Code)
	}
	if rec := get(t, h, "/videos/2024/06/still.jpg", cookie); rec.Code != http.StatusNotFound {
		t.Errorf("the photograph answers under /videos/: %d", rec.Code)
	}
}

// TestAVideoSaysHowLongItIs: the one thing a still cannot show, and the only
// difference between the two grids.
func TestAVideoSaysHowLongItIs(t *testing.T) {
	t.Parallel()
	h, s, meta := browserOver(t)
	cookie := signIn(t, h)
	addVideo(t, s, meta, "clip.mp4", videoDay, 42_000)

	body := get(t, h, "/videos/", cookie).Body.String()
	if !strings.Contains(body, ">0:42<") {
		t.Errorf("no duration on the tile:\n%s", body)
	}
	// The empty state is the library's own words, not the gallery's.
	if !strings.Contains(get(t, h, "/photos/", cookie).Body.String(), "No photos yet") {
		t.Error("the empty gallery does not say so")
	}
}

func TestAnEmptyVideoLibrarySaysSo(t *testing.T) {
	t.Parallel()
	h := newHandler(t, nil)
	if body := get(t, h, "/videos/", signIn(t, h)).Body.String(); !strings.Contains(body, "No videos yet") {
		t.Errorf("an empty video library =\n%s", body)
	}
}

// TestTheVideoTreeWalks: a year, a month and the bytes, at the addresses the
// WebDAV mount answers for.
func TestTheVideoTreeWalks(t *testing.T) {
	t.Parallel()
	h, s, meta := browserOver(t)
	cookie := signIn(t, h)
	f := addVideo(t, s, meta, "clip.mp4", videoDay, 42_000)

	year := get(t, h, "/videos/2024/", cookie)
	if year.Code != http.StatusOK || !strings.Contains(year.Body.String(), `href="/videos/2024/06/"`) {
		t.Fatalf("the year = %d", year.Code)
	}
	month := get(t, h, "/videos/2024/06/", cookie)
	if got := tileNames(month.Body.String()); strings.Join(got, ",") != "clip.mp4" {
		t.Errorf("the month holds %v", got)
	}
	if !strings.Contains(month.Body.String(), `href="/videos/2024/"`) {
		t.Error("a month does not lead back to its year")
	}

	bytes := get(t, h, "/videos/2024/06/clip.mp4", cookie)
	if bytes.Code != http.StatusOK || bytes.Body.String() != "frames" {
		t.Fatalf("GET = %d %q, want the original", bytes.Code, bytes.Body.String())
	}
	if got := bytes.Header().Get("ETag"); !strings.Contains(got, f.ETag) {
		t.Errorf("ETag = %q, want the file's", got)
	}
}

// TestAVideosPageIsThePlayer, at the video's own address rather than the
// file's: one URL per thing here too.
func TestAVideosPageIsThePlayer(t *testing.T) {
	t.Parallel()
	c := newCinema(t)
	cookie := signIn(t, c)
	c.film(t, "clip.mp4", "gop.mp4", db.Media{TakenAt: videoDay, DurationMS: 42_000, Codec: "h264", AudioCodec: "aac", Channels: 1})

	rec := get(t, c, "/videos/2024/06/clip.mp4?play", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("the player = %d", rec.Code)
	}
	body := htmlstd.UnescapeString(rec.Body.String())
	for _, want := range []string{
		`<video`,
		`src="/videos/2024/06/clip.mp4"`,
		`href="/videos/2024/06/"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the player lacks %q:\n%s", want, body)
		}
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "media-src 'self'") {
		t.Errorf("the player's policy blocks its own video: %q", csp)
	}
}

// TestAVideoIsReachableFromAPathInTheTree, like a photograph is.
func TestAVideoIsReachableFromAPathInTheTree(t *testing.T) {
	t.Parallel()
	h, s, meta := browserOver(t)
	cookie := signIn(t, h)
	addVideo(t, s, meta, "clip.mp4", videoDay, 1000)

	rec := get(t, h, "/videos/?file=clip.mp4", cookie)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("by path = %d", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/videos/2024/06/clip.mp4?play" {
		t.Errorf("Location = %q", got)
	}
}
