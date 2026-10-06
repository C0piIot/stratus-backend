package web_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
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
		s, thumbs, meta, indexing(meta), nil, web.Video{Media: meta, Segments: segs})
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
		`<source src="/files/film.mkv?hls=index.m3u8&amp;k=`,
		`" type="application/vnd.apple.mpegurl">`,
		`<source src="/files/film.mkv?k=`, `data-hls="/files/film.mkv?hls=index.m3u8&amp;k=`,
		`/static/hls.js-1.7.3/hls.light.min.js`, `/static/stratus/play.js?v=`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the player for a Matroska film lacks %s", want)
		}
	}
	// The order is the feature: a browser takes the first source it can play,
	// so the playlist has to be offered before the film it was remuxed from.
	if strings.Index(page, `type="application/vnd.apple.mpegurl"`) > strings.Index(page, `<source src="/files/film.mkv?k=`) {
		t.Errorf("the file is offered before the playlist, so Safari plays neither:\n%s", page)
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "media-src 'self' blob:") {
		t.Errorf("Content-Security-Policy = %q, want media from here and from a MediaSource", csp)
	}

	page = get(t, c, "/files/clip.mp4?play", cookie).Body.String()
	if strings.Contains(page, "data-hls") || strings.Contains(page, "hls.light") || strings.Contains(page, "play.js") {
		t.Errorf("an MP4 a browser plays was given HLS:\n%s", page)
	}
}

// sourceSrc reads the addresses the player hands the browser, in the order
// the element offers them.
var sourceSrc = regexp.MustCompile(`<source src="([^"]+)"`)

// TestThePlayerSignsWhatAReceiverFetches: a cast or AirPlay button hands the
// television whatever the element points at, and a television carries no
// cookie -- so the film and its playlist are signed even on the owner's own
// page, while the link beside the player, which is for a person with a
// session, is not.
func TestThePlayerSignsWhatAReceiverFetches(t *testing.T) {
	t.Parallel()
	c := newCinema(t)
	c.film(t, "film.mkv", "gop.mkv", ac3Film)
	cookie := signIn(t, c)

	page := get(t, c, "/files/film.mkv?play", cookie).Body.String()
	if !strings.Contains(page, `href="/files/film.mkv">Open the file`) {
		t.Errorf("the link beside the player is signed, and it is for a person:\n%s", page)
	}
	found := sourceSrc.FindAllStringSubmatch(page, -1)
	if len(found) != 2 {
		t.Fatalf("the player offers %d sources, want the playlist and the film:\n%s", len(found), page)
	}
	// The addresses come out of an attribute, where the query's & is written
	// &amp; -- a request takes the one the browser would have made.
	unescape := func(s string) string { return strings.ReplaceAll(s, "&amp;", "&") }
	playlist, film := unescape(found[0][1]), unescape(found[1][1])
	for _, target := range []string{playlist, film} {
		if rec := get(t, c, target); rec.Code != http.StatusOK {
			t.Errorf("GET %s with no cookie = %d, which is what a receiver would get", target, rec.Code)
		}
	}
	if list := get(t, c, playlist).Body.String(); !strings.Contains(list, "&k=") {
		t.Errorf("the segments a receiver would fetch next are unsigned:\n%s", list)
	}

	// A page that arrived with a signature uses that one rather than minting a
	// second: the link somebody was sent is what the film is read with.
	token := auth.NewShares(credentials()).Issue(username, "film.mkv", false, time.Time{})
	page = get(t, c, "/files/film.mkv?play&k="+token).Body.String()
	for _, m := range sourceSrc.FindAllStringSubmatch(page, -1) {
		if !strings.Contains(unescape(m[1]), token) {
			t.Errorf("a shared player reads %s, which is not the link it was opened with", m[1])
		}
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

// fakeEncoder stands in for libx264.
type fakeEncoder struct {
	mu    sync.Mutex
	asked []media.Segment
}

func (f *fakeEncoder) Segment(_ context.Context, _ db.File, _ media.Encode, s media.Segment) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, s)
	return io.NopCloser(strings.NewReader("encoded " + media.EncodedName(s))), nil
}

func newEncodingCinema(t *testing.T) (*cinema, *fakeEncoder) {
	t.Helper()
	s, thumbs, meta := pieces(t)
	segs, enc := &fakeSegments{}, &fakeEncoder{}
	creds := credentials()
	h := web.Handler(version, buildDate, creds, auth.NewSessions(creds, auth.DefaultSessionTTL), auth.NewShares(creds),
		s, thumbs, meta, indexing(meta), nil, web.Video{Media: meta, Segments: segs, Encoded: enc})
	return &cinema{Handler: h, files: s, meta: meta, segments: segs}, enc
}

// TestAFilmIsOfferedBothWays: HEVC is remuxed for whoever decodes it and
// re-encoded for whoever does not, and the master playlist lets the player
// choose by the codecs each declares.
func TestAFilmIsOfferedBothWays(t *testing.T) {
	t.Parallel()
	c, enc := newEncodingCinema(t)
	hevc := db.Media{Codec: "hevc", CodecProfile: "Main 10", Level: 120, BitDepth: 10, Width: 3840, Height: 2160,
		DurationMS: 14_000, AudioCodec: "aac", Channels: 2, ColorTransfer: "arib-std-b67"}
	c.film(t, "iphone.mkv", "gop.mkv", hevc)
	cookie := signIn(t, c)

	master := get(t, c, "/files/iphone.mkv?hls=index.m3u8", cookie).Body.String()
	for _, want := range []string{`CODECS="hvc1.2.4.L120.B0,mp4a.40.2"`, "\n?hls=copy.m3u8\n", `CODECS="avc1.640028,mp4a.40.2"`, "RESOLUTION=1920x1080", "\n?hls=h264.m3u8\n"} {
		if !strings.Contains(master, want) {
			t.Errorf("the master playlist lacks %q:\n%s", want, master)
		}
	}

	encoded := get(t, c, "/files/iphone.mkv?hls=h264.m3u8", cookie).Body.String()
	if !strings.Contains(encoded, "\n?hls=h0-6000.ts\n") || !strings.Contains(encoded, "\n?hls=h12000-2000.ts\n") {
		t.Errorf("the re-encoded playlist:\n%s", encoded)
	}
	if copyList := get(t, c, "/files/iphone.mkv?hls=copy.m3u8", cookie).Body.String(); !strings.Contains(copyList, "?hls=0-6023-150-0.ts") {
		t.Errorf("the remux playlist:\n%s", copyList)
	}

	rec := get(t, c, "/files/iphone.mkv?hls=h6000-6000.ts", cookie)
	if rec.Code != http.StatusOK || rec.Body.String() != "encoded h6000-6000.ts" || rec.Header().Get("Content-Type") != "video/mp2t" {
		t.Errorf("an encoded segment = %d %q", rec.Code, rec.Body.String())
	}
	if rec := get(t, c, "/files/iphone.mkv?hls=h1-99999.ts", cookie); rec.Code != http.StatusBadRequest {
		t.Errorf("a nonsense encoded segment = %d", rec.Code)
	}
	enc.mu.Lock()
	defer enc.mu.Unlock()
	if len(enc.asked) != 1 {
		t.Errorf("the encoder was asked %d times", len(enc.asked))
	}
}

// TestAFilmOnlyReEncodingCanPlay: VP9 cannot be remuxed into a transport
// stream, so with an encoder it is the H.264 alone -- and the player offers
// it, where without one it would not.
func TestAFilmOnlyReEncodingCanPlay(t *testing.T) {
	t.Parallel()
	c, _ := newEncodingCinema(t)
	c.film(t, "vp9.mkv", "gop.mkv", db.Media{Codec: "vp9", Width: 1280, Height: 720, DurationMS: 6000})
	cookie := signIn(t, c)

	list := get(t, c, "/files/vp9.mkv?hls=index.m3u8", cookie).Body.String()
	if strings.Contains(list, "STREAM-INF") || !strings.Contains(list, "?hls=h0-6000.ts") {
		t.Errorf("a film only re-encoding plays should get that playlist alone:\n%s", list)
	}
	if rec := get(t, c, "/files/vp9.mkv?hls=copy.m3u8", cookie); rec.Code != http.StatusNotFound {
		t.Errorf("a remux of VP9 = %d, want 404", rec.Code)
	}
	if rec := get(t, c, "/files/vp9.mkv?hls=0-6000-150-0.ts", cookie); rec.Code != http.StatusNotFound {
		t.Errorf("a remuxed segment of VP9 = %d, want 404", rec.Code)
	}
	if page := get(t, c, "/files/vp9.mkv?play", cookie).Body.String(); !strings.Contains(page, "data-hls=") {
		t.Error("the player does not offer a film it could play re-encoded")
	}
}
