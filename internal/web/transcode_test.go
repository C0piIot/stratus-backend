package web_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/auth"
	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/media"
	"github.com/C0piIot/stratus-backend/internal/media/stream"
	"github.com/C0piIot/stratus-backend/internal/web"
)

// fakeTranscoder stands in for ffmpeg: it records the plans it was handed and
// answers with the format's name.
type fakeTranscoder struct {
	mu    sync.Mutex
	plans []stream.Plan
	err   error
}

func (f *fakeTranscoder) Transcode(_ context.Context, _ db.File, p stream.Plan, _ time.Duration) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.plans = append(f.plans, p)
	if f.err != nil {
		return nil, f.err
	}
	return io.NopCloser(strings.NewReader("transcoded to " + p.Format)), nil
}

func (f *fakeTranscoder) started() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.plans)
}

type jukebox struct {
	http.Handler
	files      *files.Service
	meta       db.Store
	transcoder *fakeTranscoder
}

func newJukebox(t *testing.T) *jukebox {
	t.Helper()
	s, thumbs, meta := pieces(t)
	tr := &fakeTranscoder{}
	creds := credentials()
	h := web.Handler(web.Deps{
		Version:   version,
		BuildDate: buildDate,
		Verifier:  creds,
		Sessions:  auth.NewSessions(creds, auth.DefaultSessionTTL),
		Shares:    auth.NewShares(creds),
		Files:     s,
		Thumbs:    thumbs,
		Index:     meta,
		Playlists: playlistsOf(meta),
		Indexing:  indexing(meta),
		Playback:  web.Playback{Media: meta, Segments: &fakeSegments{}, Transcode: tr},
	})
	return &jukebox{Handler: h, files: s, meta: meta, transcoder: tr}
}

// track stores a file and, when m is not nil, the row the indexer would have
// written for it.
func (j *jukebox) track(t *testing.T, name string, m *db.Media) {
	t.Helper()
	write(t, j.files, name, "the original bytes")
	if m == nil {
		return
	}
	f, err := j.files.Stat(t.Context(), username, name)
	if err != nil {
		t.Fatal(err)
	}
	m.FileID, m.Kind, m.IndexedAt, m.Version = f.ID, db.KindAudio, time.Now(), 1
	if err := j.meta.PutMedia(t.Context(), *m); err != nil {
		t.Fatal(err)
	}
}

func opus() *db.Media {
	return &db.Media{Codec: "opus", Bitrate: 160_000, SampleRate: 48_000, Channels: 2, DurationMS: 60_000,
		AlbumArtist: "Artist", Artist: "Artist", Album: "Album", Title: "Song"}
}

// head is get's HEAD, which is what a client checks a URL with before it hands
// it to a screen.
func head(t *testing.T, h http.Handler, target string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodHead, target, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestATrackIsTranscodedAtItsOwnURL is #433: ?format= on the file's address,
// with a share link's signature and nothing else -- which is how a television
// asks.
func TestATrackIsTranscodedAtItsOwnURL(t *testing.T) {
	t.Parallel()
	j := newJukebox(t)
	j.track(t, "song.opus", opus())
	token := auth.NewShares(credentials()).Issue(username, "song.opus", false, time.Time{})
	target := "/files/song.opus?format=mp3&k=" + token

	checked := head(t, j, target)
	if checked.Code != http.StatusOK || checked.Header().Get("Content-Type") != "audio/mpeg" {
		t.Fatalf("HEAD = %d %q, want 200 audio/mpeg", checked.Code, checked.Header().Get("Content-Type"))
	}
	if j.transcoder.started() != 0 {
		t.Error("a HEAD started a transcode")
	}

	rec := get(t, j, target)
	if rec.Code != http.StatusOK || rec.Body.String() != "transcoded to mp3" {
		t.Fatalf("GET = %d %q", rec.Code, rec.Body)
	}
	for name, want := range map[string]string{
		"Content-Type": "audio/mpeg", "Accept-Ranges": "none", "Access-Control-Allow-Origin": "*",
	} {
		if got := rec.Header().Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

// TestATranscodeTakesStreamsParameters: the bitrate and the estimate mean what
// they mean to OpenSubsonic, because the decision is the same one.
func TestATranscodeTakesStreamsParameters(t *testing.T) {
	t.Parallel()
	j := newJukebox(t)
	j.track(t, "song.opus", opus())
	cookie := signIn(t, j)

	rec := get(t, j, "/files/song.opus?format=mp3&maxBitRate=96&estimateContentLength=true", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d", rec.Code)
	}
	if got := j.transcoder.plans[0].Bitrate; got != 96_000 {
		t.Errorf("bitrate = %d, want 96000", got)
	}
	// 96 kbps for a minute.
	if got := rec.Header().Get("Content-Length"); got != "720000" {
		t.Errorf("Content-Length = %q, want the estimate", got)
	}
}

// TestWhatCannotBeTranscodedIsTheOriginal: the cases stream.Decide sends as
// they are, and the ones it is never asked about, all answer the file with its
// ranges -- which is how a client tells it got nothing it asked for.
func TestWhatCannotBeTranscodedIsTheOriginal(t *testing.T) {
	t.Parallel()
	j := newJukebox(t)
	j.track(t, "song.opus", opus())
	j.track(t, "unread.opus", nil)
	write(t, j.files, "notes.txt", "the original bytes")
	cookie := signIn(t, j)

	for _, target := range []string{
		"/files/song.opus?format=raw",
		"/files/song.opus?format=opus",
		"/files/song.opus?format=wav",
		"/files/song.opus?format=flac",
		"/files/unread.opus?format=mp3",
		"/files/notes.txt?format=mp3",
	} {
		rec := get(t, j, target, cookie)
		if rec.Code != http.StatusOK || rec.Body.String() != "the original bytes" ||
			rec.Header().Get("Accept-Ranges") != "bytes" {
			t.Errorf("%s = %d %q, ranges %q; want the original", target, rec.Code, rec.Body,
				rec.Header().Get("Accept-Ranges"))
		}
	}
	if j.transcoder.started() != 0 {
		t.Errorf("%d transcodes started for what needed none", j.transcoder.started())
	}
}

// TestABusyTranscoderIsA503: the client asked for another format because the
// original is no use to it, so the original is not the answer.
func TestABusyTranscoderIsA503(t *testing.T) {
	t.Parallel()
	j := newJukebox(t)
	j.track(t, "song.opus", opus())
	j.transcoder.err = media.ErrBusy

	rec := get(t, j, "/files/song.opus?format=mp3", signIn(t, j))
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Errorf("busy = %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
}

// TestALinkToOneTrackTranscodesNoOther: the signature is checked against the
// path before any of this runs.
func TestALinkToOneTrackTranscodesNoOther(t *testing.T) {
	t.Parallel()
	j := newJukebox(t)
	j.track(t, "song.opus", opus())
	j.track(t, "other.opus", opus())
	token := auth.NewShares(credentials()).Issue(username, "song.opus", false, time.Time{})

	if rec := get(t, j, "/files/other.opus?format=mp3&k="+token); rec.Code != http.StatusForbidden {
		t.Errorf("another track = %d, want 403", rec.Code)
	}
}

// TestATrackInTheLibraryTranscodesToo: the address under /music/ is the same
// track and takes the same query.
func TestATrackInTheLibraryTranscodesToo(t *testing.T) {
	t.Parallel()
	j := newJukebox(t)
	j.track(t, "song.opus", opus())

	rec := get(t, j, "/music/Artist/Album/song.opus?format=mp3", signIn(t, j))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "audio/mpeg" {
		t.Errorf("GET = %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
}
