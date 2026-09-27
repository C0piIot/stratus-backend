package web_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/auth"
	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/media"
	"github.com/C0piIot/stratus-backend/internal/web"
)

// fakeSegments stands in for ffmpeg: it records the segments asked for and
// answers with their names.
type fakeSegments struct {
	mu    sync.Mutex
	asked []media.Segment
	remux []media.Remux
	err   error
}

func (f *fakeSegments) Segment(_ context.Context, _ db.File, r media.Remux, s media.Segment) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked, f.remux = append(f.asked, s), append(f.remux, r)
	if f.err != nil {
		return nil, f.err
	}
	return io.NopCloser(strings.NewReader("segment " + s.Name())), nil
}

type cinema struct {
	http.Handler
	files    *files.Service
	meta     db.Store
	segments *fakeSegments
}

func newCinema(t *testing.T) *cinema {
	t.Helper()
	s, thumbs, meta := pieces(t)
	segs := &fakeSegments{}
	creds := credentials()
	h := web.Handler(version, buildDate, creds, auth.NewSessions(creds, auth.DefaultSessionTTL), auth.NewShares(creds),
		s, thumbs, meta, indexing(meta), web.Video{Media: meta, Segments: segs})
	return &cinema{Handler: h, files: s, meta: meta, segments: segs}
}

// film stores a fixture from internal/media's testdata under name, with the
// media row the indexer would have written.
func (c *cinema) film(t *testing.T, name, fixture string, m db.Media) db.File {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "media", "testdata", fixture))
	if err != nil {
		t.Fatal(err)
	}
	f, err := c.files.Write(t.Context(), username, name, strings.NewReader(string(body)), int64(len(body)), "")
	if err != nil {
		t.Fatal(err)
	}
	m.FileID, m.Kind, m.IndexedAt = f.ID, db.KindVideo, time.Now()
	if err := c.meta.PutMedia(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	return f
}

var ac3Film = db.Media{Codec: "h264", AudioCodec: "ac3", Channels: 6}

// TestAFilmOpensInThePlayer: a video row in the listing links to the player,
// which keeps the file one click away.
func TestAFilmOpensInThePlayer(t *testing.T) {
	t.Parallel()
	c := newCinema(t)
	c.film(t, "film.mkv", "gop.mkv", ac3Film)
	write(t, c.files, "notes.txt", "hello")
	cookie := signIn(t, c)

	listing := get(t, c, "/files/", cookie).Body.String()
	if !strings.Contains(listing, `href="/files/film.mkv?play"`) || !strings.Contains(listing, `href="/files/notes.txt"`) {
		t.Errorf("the listing does not send a film to the player and a note to itself:\n%s", listing)
	}
}

// TestThePlayerAsksForHLSOnlyWhenItMust: a Matroska film with AC-3 sound gets
// the playlist and the two scripts; an MP4 a browser plays gets neither, and
// no script at all is loaded for it.
func TestThePlayerAsksForHLSOnlyWhenItMust(t *testing.T) {
	t.Parallel()
	c := newCinema(t)
	c.film(t, "film.mkv", "gop.mkv", ac3Film)
	c.film(t, "clip.mp4", "gop.mp4", db.Media{Codec: "h264", AudioCodec: "aac", Channels: 1})
	cookie := signIn(t, c)

	rec := get(t, c, "/files/film.mkv?play", cookie)
	page := rec.Body.String()
	for _, want := range []string{
		`src="/files/film.mkv"`, `data-hls="/files/film.mkv?hls=index.m3u8"`,
		`/static/hls.js-1.7.3/hls.light.min.js`, `/static/stratus/play.js?v=`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the player for a Matroska film lacks %s", want)
		}
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "media-src 'self' blob:") {
		t.Errorf("Content-Security-Policy = %q, want media from here and from a MediaSource", csp)
	}

	page = get(t, c, "/files/clip.mp4?play", cookie).Body.String()
	if strings.Contains(page, "data-hls") || strings.Contains(page, "hls.light") || strings.Contains(page, "play.js") {
		t.Errorf("an MP4 a browser plays was given HLS:\n%s", page)
	}
}

// TestThePlaylist is built from the film's own keyframes, and each segment's
// address carries the signature the playlist was fetched with.
func TestThePlaylist(t *testing.T) {
	t.Parallel()
	c := newCinema(t)
	c.film(t, "film.mkv", "gop.mkv", ac3Film)

	rec := get(t, c, "/files/film.mkv?hls=index.m3u8", signIn(t, c))
	body := rec.Body.String()
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/vnd.apple.mpegurl" {
		t.Fatalf("playlist = %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if !strings.HasPrefix(body, "#EXTM3U") || !strings.Contains(body, "\n?hls=0-6023-150-0.ts\n") || !strings.HasSuffix(body, "#EXT-X-ENDLIST\n") {
		t.Errorf("playlist:\n%s", body)
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Error("a Chromecast's receiver cannot read a playlist without CORS")
	}

	token := auth.NewShares(credentials()).Issue(username, "film.mkv", false, time.Time{})
	shared := get(t, c, "/files/film.mkv?hls=index.m3u8&k="+token).Body.String()
	if !strings.Contains(shared, "?hls=0-6023-150-0.ts&k=") {
		t.Errorf("a shared playlist's segments do not carry the signature:\n%s", shared)
	}
}

// TestASegment reaches the transcoder as its name says, and a film that
// cannot be remuxed, has not been read or asks for nonsense is refused with a
// status a player reads.
func TestASegment(t *testing.T) {
	t.Parallel()
	c := newCinema(t)
	c.film(t, "film.mkv", "gop.mkv", ac3Film)
	c.film(t, "vp9.webm", "opus.webm", db.Media{Codec: "vp9", AudioCodec: "opus"})
	cookie := signIn(t, c)

	rec := get(t, c, "/files/film.mkv?hls=6023-3977-100-6173.ts", cookie)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "video/mp2t" || rec.Body.String() != "segment 6023-3977-100-6173.ts" {
		t.Fatalf("segment = %d %q %q", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
	c.segments.mu.Lock()
	remux := c.segments.remux[0]
	c.segments.mu.Unlock()
	if remux.CopyAudio {
		t.Error("AC-3 sound was to be copied into a stream a television will not play")
	}

	write(t, c.files, "unread.mkv", "not indexed")
	for target, want := range map[string]int{
		"/files/film.mkv?hls=nonsense.ts":      http.StatusBadRequest,
		"/files/vp9.webm?hls=index.m3u8":       http.StatusNotFound,
		"/files/unread.mkv?hls=index.m3u8":     http.StatusNotFound,
		"/files/film.mkv?hls=0-1000-9999-0.ts": http.StatusBadRequest,
	} {
		if rec := get(t, c, target, cookie); rec.Code != want {
			t.Errorf("%s = %d, want %d", target, rec.Code, want)
		}
	}

	c.segments.err = media.ErrBusy
	if rec := get(t, c, "/files/film.mkv?hls=0-6023-150-0.ts", cookie); rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Errorf("a full house = %d, want 503 with Retry-After", rec.Code)
	}
	c.segments.err = errors.New("ffmpeg: no")
	if rec := get(t, c, "/files/film.mkv?hls=0-6023-150-0.ts", cookie); rec.Code != http.StatusInternalServerError {
		t.Errorf("a failing ffmpeg = %d, want 500", rec.Code)
	}
}

// TestHLSIsForFilmsOnly: a query on anything else is the file, as it was.
func TestHLSIsForFilmsOnly(t *testing.T) {
	t.Parallel()
	c := newCinema(t)
	write(t, c.files, "notes.txt", "hello")
	if rec := get(t, c, "/files/notes.txt?hls=index.m3u8", signIn(t, c)); rec.Body.String() != "hello" {
		t.Errorf("a note asked for as HLS = %q, want the note", rec.Body.String())
	}
}

// TestAFilmWithoutAQueryIsTheFile, and a film whose container keeps no index
// has no playlist to give.
func TestAFilmWithoutAQueryIsTheFile(t *testing.T) {
	t.Parallel()
	c := newCinema(t)
	f := c.film(t, "film.mkv", "gop.mkv", ac3Film)
	cookie := signIn(t, c)
	if rec := get(t, c, "/files/film.mkv", cookie); rec.Body.Len() != int(f.Size) {
		t.Errorf("the film itself = %d bytes, want %d", rec.Body.Len(), f.Size)
	}

	write(t, c.files, "broken.mkv", "not a film at all")
	stored, err := c.files.Stat(t.Context(), username, "broken.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.meta.PutMedia(t.Context(), db.Media{FileID: stored.ID, Kind: db.KindVideo, Codec: "h264", IndexedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if rec := get(t, c, "/files/broken.mkv?hls=index.m3u8", cookie); rec.Code != http.StatusNotFound {
		t.Errorf("a playlist for a file with no keyframe index = %d, want 404", rec.Code)
	}
}
