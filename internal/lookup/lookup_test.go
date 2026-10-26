package lookup_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stratus-private-cloud/stratus-backend/internal/db"
	"github.com/stratus-private-cloud/stratus-backend/internal/db/dbtest"
	"github.com/stratus-private-cloud/stratus-backend/internal/files"
	"github.com/stratus-private-cloud/stratus-backend/internal/files/filestest"
	"github.com/stratus-private-cloud/stratus-backend/internal/lookup"
	"github.com/stratus-private-cloud/stratus-backend/internal/media/thumb"
)

const owner = "edu"

// The ids the fake MusicBrainz hands out, and the one the fake archive has a
// picture for.
const (
	amberGroup   = "4cf8ba42-5d0b-3a4d-8e8f-4c8f2f3d1a1b"
	taggedGroup  = "0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"
	taggedCovers = true
)

// sources is the three services, faked: what they were asked, and what they
// answer. Real HTTP, because what is under test includes the request each
// client makes -- the query, the header, the path -- and a fake client would
// only prove the fake was called.
type sources struct {
	mb, caa, lrc *httptest.Server

	mu      sync.Mutex
	queries []string
	agents  []string
	// mbCalls, caaCalls and lrcCalls count the requests each service saw.
	mbCalls, caaCalls, lrcCalls atomic.Int64
	// refuse makes every service answer 503, which is MusicBrainz's rate
	// limit and anybody's "go away".
	refuse atomic.Bool
	// notAPicture makes the archive answer a page where a cover should be.
	notAPicture atomic.Bool
}

func newSources(t *testing.T) *sources {
	t.Helper()
	s := &sources{}
	record := func(r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.agents = append(s.agents, r.Header.Get("User-Agent"))
	}

	s.mb = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mbCalls.Add(1)
		record(r)
		if s.refuse.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if r.URL.Path != "/ws/2/release-group/" || r.URL.Query().Get("fmt") != "json" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query().Get("query")
		s.mu.Lock()
		s.queries = append(s.queries, q)
		s.mu.Unlock()
		var groups []map[string]any
		switch {
		case strings.Contains(q, `artist:"Autechre, Someone Else"`):
			// A credit of several names matches nothing, as on the real
			// service; the first name alone does.
			groups = nil
		case strings.Contains(q, `releasegroup:"Amber"`):
			groups = []map[string]any{
				// A near miss first, so the score is what decides.
				{"id": "11111111-1111-4111-8111-111111111111", "score": 60, "title": "Amber Waves"},
				{"id": amberGroup, "score": 100, "title": "Amber"},
			}
		case strings.Contains(q, `releasegroup:"Nowhere"`):
			groups = nil
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"count": len(groups), "release-groups": groups})
	}))
	t.Cleanup(s.mb.Close)

	s.caa = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.caaCalls.Add(1)
		record(r)
		if s.refuse.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		switch r.URL.Path {
		case "/release-group/" + amberGroup + "/front-500", "/release-group/" + taggedGroup + "/front-500":
			// The real archive answers a redirect to archive.org, which the
			// client has to follow.
			http.Redirect(w, r, "/image.png", http.StatusTemporaryRedirect)
		case "/image.png":
			if s.notAPicture.Load() {
				w.Header().Set("Content-Type", "text/html")
				_, _ = w.Write([]byte("<html>not a picture</html>"))
				return
			}
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(pngBytes(t, 500, 500))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.caa.Close)

	s.lrc = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.lrcCalls.Add(1)
		record(r)
		if s.refuse.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if r.URL.Path != "/api/get" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		s.mu.Lock()
		s.queries = append(s.queries, "lrclib "+q.Encode())
		s.mu.Unlock()
		switch q.Get("track_name") {
		case "Foil":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"trackName": "Foil", "artistName": "Autechre", "instrumental": false,
				"plainLyrics": "first line\nsecond line", "syncedLyrics": "[00:01.00] first line\n[00:05.50] second line",
			})
		case "Montreal":
			_ = json.NewEncoder(w).Encode(map[string]any{"trackName": "Montreal", "instrumental": true})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.lrc.Close)
	return s
}

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := png.Encode(&out, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// library is the real backends the pass reads and writes: a SQLite database,
// blobs on disk, the file layer over them and the thumbnail generator that
// files a fetched cover.
type library struct {
	meta   db.Store
	files  *files.Service
	thumbs *thumb.Service
}

func newLibrary(t *testing.T) *library {
	t.Helper()
	blobs, meta := filestest.Backends(t)
	service := files.New(blobs, meta)
	thumbs := thumb.New(blobs, service, meta, "ffmpeg", t.TempDir()).WithLookups(meta)
	return &library{meta: meta, files: service, thumbs: thumbs}
}

// track writes a file and its tags.
func (l *library) track(t *testing.T, path string, m db.Media) db.File {
	t.Helper()
	if dir := db.ParentOf(path); dir != "" {
		for _, parent := range parents(dir) {
			_, _ = l.files.Mkdir(t.Context(), owner, parent)
		}
	}
	f, err := l.files.Write(t.Context(), owner, path, strings.NewReader("fLaC"), 4, "audio/flac")
	if err != nil {
		t.Fatal(err)
	}
	m.FileID, m.Kind, m.IndexedAt, m.Version = f.ID, db.KindAudio, time.Now(), 1
	if err := l.meta.PutMedia(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	return f
}

func parents(dir string) []string {
	var out []string
	for i, c := range dir {
		if c == '/' {
			out = append(out, dir[:i])
		}
	}
	return append(out, dir)
}

func song(artist, album, title string, n int) db.Media {
	return db.Media{AlbumArtist: artist, Artist: artist, Album: album, Title: title, TrackNo: n, DurationMS: 254_600}
}

// service is a Service over the fakes, with no pacing and a clock that stands
// still, so a retry time can be compared exactly.
func service(t *testing.T, l *library, s *sources, now time.Time) *lookup.Service {
	t.Helper()
	return lookup.New(l.meta, l.thumbs, "1.2.3-test",
		lookup.WithEndpoints(s.mb.URL, s.caa.URL, s.lrc.URL),
		lookup.WithPace(0),
		lookup.WithClock(func() time.Time { return now }))
}

// TestAPassIdentifiesAnAlbumAndFetchesItsCover is the whole feature on one
// album: searched on MusicBrainz, its cover fetched from the archive and
// served where the album's picture is asked for, and the words of its tracks
// looked up.
func TestAPassIdentifiesAnAlbumAndFetchesItsCover(t *testing.T) {
	t.Parallel()
	l, s := newLibrary(t), newSources(t)
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	foil := l.track(t, "music/amber/01.flac", song("Autechre", "Amber", "Foil", 1))
	l.track(t, "music/amber/02.flac", song("Autechre", "Amber", "Montreal", 2))

	done, err := service(t, l, s, now).Pass(t.Context())
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if want := (lookup.Done{Albums: 1, Covers: 1, Lyrics: 2}); done != want {
		t.Errorf("Pass = %+v, want %+v", done, want)
	}

	// The search named both tags as phrases, which is what keeps "Amber" by
	// somebody else out of the answer.
	s.mu.Lock()
	queries := append([]string(nil), s.queries...)
	s.mu.Unlock()
	if len(queries) == 0 || queries[0] != `releasegroup:"Amber" AND artist:"Autechre"` {
		t.Errorf("MusicBrainz was asked %q", queries)
	}

	row, err := l.meta.AlbumLookup(t.Context(), owner, "Autechre", "Amber")
	if err != nil {
		t.Fatal(err)
	}
	if row.ReleaseGroup != amberGroup || row.CoverBlob != foil.BlobKey || !row.RetryAt.IsZero() || row.Error != "" {
		t.Errorf("the album's row is %+v", row)
	}

	// The cover answers where getCoverArt and the album page ask for it.
	body, _, err := l.thumbs.Cover(t.Context(), owner, "music/amber", 300)
	if err != nil {
		t.Fatalf("Cover after the pass: %v", err)
	}
	_ = body.Close()

	// And the words: one with them, one without any to sing.
	words, err := l.meta.LyricsOf(t.Context(), []int64{foil.ID, foil.ID + 1})
	if err != nil {
		t.Fatal(err)
	}
	if got := words[foil.ID]; got.Plain != "first line\nsecond line" || !strings.HasPrefix(got.Synced, "[00:01.00]") || got.ETag != foil.ETag {
		t.Errorf("Foil's words are %+v", got)
	}
	if got := words[foil.ID+1]; !got.Instrumental || !got.Found() {
		t.Errorf("Montreal is not an instrumental: %+v", got)
	}
	// Both asked with what narrows the answer: the album and the length.
	if !strings.Contains(strings.Join(queries, "\n"), "album_name=Amber&artist_name=Autechre&duration=255&track_name=Foil") {
		t.Errorf("LRCLIB was asked %q", queries)
	}
}

// TestACreditOfSeveralNamesIsAskedByItsFirst: the full credit matches
// nothing on MusicBrainz, which files the record under the band, so the
// first name is asked next -- and only then.
func TestACreditOfSeveralNamesIsAskedByItsFirst(t *testing.T) {
	t.Parallel()
	l, s := newLibrary(t), newSources(t)
	l.track(t, "music/amber/01.flac", song("Autechre, Someone Else", "Amber", "Foil", 1))

	done, err := service(t, l, s, time.Now()).Pass(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if done.Albums != 1 || done.Covers != 1 {
		t.Errorf("Pass = %+v, want the album identified and its cover fetched", done)
	}
	if n := s.mbCalls.Load(); n != 2 {
		t.Errorf("MusicBrainz was asked %d times, want the full credit and then the first name", n)
	}
	row, err := l.meta.AlbumLookup(t.Context(), owner, "Autechre, Someone Else", "Amber")
	if err != nil || row.ReleaseGroup != amberGroup {
		t.Errorf("the album's row is %+v, %v", row, err)
	}
}

// TestAnEditionIsAskedWithoutIt: the search is a phrase, so a title that
// names its pressing finds nothing, and the title without it is asked next.
func TestAnEditionIsAskedWithoutIt(t *testing.T) {
	t.Parallel()
	l, s := newLibrary(t), newSources(t)
	l.track(t, "music/amber/01.flac", song("Autechre", "Amber (Deluxe Edition)", "Foil", 1))

	done, err := service(t, l, s, time.Now()).Pass(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if done.Albums != 1 || done.Covers != 1 {
		t.Errorf("Pass = %+v, want the album identified and its cover fetched", done)
	}
	var queries []string
	s.mu.Lock()
	for _, q := range s.queries {
		if strings.HasPrefix(q, "releasegroup:") {
			queries = append(queries, q)
		}
	}
	s.mu.Unlock()
	want := []string{`releasegroup:"Amber (Deluxe Edition)" AND artist:"Autechre"`, `releasegroup:"Amber" AND artist:"Autechre"`}
	if strings.Join(queries, "\n") != strings.Join(want, "\n") {
		t.Errorf("MusicBrainz was asked %q, want %q", queries, want)
	}
	row, err := l.meta.AlbumLookup(t.Context(), owner, "Autechre", "Amber (Deluxe Edition)")
	if err != nil || row.ReleaseGroup != amberGroup {
		t.Errorf("the album's row is %+v, %v", row, err)
	}
}

// TestATaggedAlbumIsNotSearched: an id in the tags is the album's own answer,
// and a search can be wrong.
func TestATaggedAlbumIsNotSearched(t *testing.T) {
	t.Parallel()
	l, s := newLibrary(t), newSources(t)
	tagged := song("Someone", "Something Obscure", "Track", 1)
	tagged.MBReleaseGroup = taggedGroup
	l.track(t, "music/obscure/01.flac", tagged)

	if _, err := service(t, l, s, time.Now()).Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := s.mbCalls.Load(); n != 0 {
		t.Errorf("MusicBrainz was asked %d times about an album the tags identify", n)
	}
	row, err := l.meta.AlbumLookup(t.Context(), owner, "Someone", "Something Obscure")
	if err != nil {
		t.Fatal(err)
	}
	if row.ReleaseGroup != taggedGroup || row.CoverBlob == "" {
		t.Errorf("the tagged album's row is %+v", row)
	}
}

// TestNothingThereIsAskedAgainInAMonth: a source that has nothing today may
// have it next month, and asking every pass would be the same question to
// the same service for ever.
func TestNothingThereIsAskedAgainInAMonth(t *testing.T) {
	t.Parallel()
	l, s := newLibrary(t), newSources(t)
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	unknown := l.track(t, "music/nowhere/01.flac", song("Nobody", "Nowhere", "Unheard", 1))

	done, err := service(t, l, s, now).Pass(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if done.Albums != 1 || done.Covers != 0 || done.Lyrics != 1 {
		t.Errorf("Pass = %+v", done)
	}
	month := now.Add(30 * 24 * time.Hour)
	row, err := l.meta.AlbumLookup(t.Context(), owner, "Nobody", "Nowhere")
	if err != nil {
		t.Fatal(err)
	}
	if row.ReleaseGroup != "" || !row.RetryAt.Equal(month) || !strings.Contains(row.Error, "MusicBrainz") {
		t.Errorf("an album nobody has is %+v, want a retry in a month", row)
	}
	words, err := l.meta.LyricsOf(t.Context(), []int64{unknown.ID})
	if err != nil {
		t.Fatal(err)
	}
	if w := words[unknown.ID]; w.Found() || !w.RetryAt.Equal(month) {
		t.Errorf("a track nobody has words for is %+v, want a retry in a month", w)
	}
	if n := s.caaCalls.Load(); n != 0 {
		t.Errorf("the archive was asked %d times for an album with no id", n)
	}
}

// TestARefusalStopsItsHalfOfThePass: a 503 from MusicBrainz is a service
// saying stop, and asking seven more times is how an agent gets blocked. What
// was refused is asked again in an hour, what was not reached stays pending
// for the next pass -- and the other half still runs, since LRCLIB is not
// MusicBrainz. The fake refuses everything, and LRCLIB's 503 is a track it is
// still fetching rather than a refusal, so both tracks are asked and deferred.
func TestARefusalStopsItsHalfOfThePass(t *testing.T) {
	t.Parallel()
	l, s := newLibrary(t), newSources(t)
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	// Two albums, in the order the queue hands them out.
	l.track(t, "music/a/01.flac", song("Aaa", "Amber", "Foil", 1))
	l.track(t, "music/b/01.flac", song("Bbb", "Amber", "Foil", 1))
	s.refuse.Store(true)

	done, err := service(t, l, s, now).Pass(t.Context())
	if err != nil {
		t.Fatalf("a refusal is not an error of the pass: %v", err)
	}
	if done.Deferred != 3 || done.Albums != 0 || done.Lyrics != 0 || done.More || !done.Refused {
		t.Errorf("Pass = %+v, want the album and both tracks deferred, and Refused said", done)
	}
	if n := s.mbCalls.Load(); n != 1 {
		t.Errorf("MusicBrainz was asked %d times after refusing", n)
	}
	if n := s.lrcCalls.Load(); n != 2 {
		t.Errorf("LRCLIB was asked %d times, want both tracks despite MusicBrainz refusing", n)
	}
	first, err := l.meta.AlbumLookup(t.Context(), owner, "Aaa", "Amber")
	if err != nil {
		t.Fatal(err)
	}
	if !first.RetryAt.Equal(now.Add(time.Hour)) || !strings.Contains(first.Error, "503") {
		t.Errorf("the refused album is %+v, want a retry in an hour", first)
	}
	if _, err := l.meta.AlbumLookup(t.Context(), owner, "Bbb", "Amber"); err == nil {
		t.Error("the album behind the refusal was written about")
	}
}

// TestTheUserAgentSaysWhoIsAsking: MusicBrainz refuses anonymous clients and
// asks for a version and a way to reach the maintainer, and the other two get
// the same courtesy.
func TestTheUserAgentSaysWhoIsAsking(t *testing.T) {
	t.Parallel()
	l, s := newLibrary(t), newSources(t)
	l.track(t, "music/amber/01.flac", song("Autechre", "Amber", "Foil", 1))

	if _, err := service(t, l, s, time.Now()).Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.agents) < 3 {
		t.Fatalf("only %d requests were made", len(s.agents))
	}
	for _, agent := range s.agents {
		if !strings.HasPrefix(agent, "Stratus/1.2.3-test ( https://github.com/") {
			t.Errorf("User-Agent = %q", agent)
		}
	}
}

// TestAPictureThatIsNotOneIsRecorded: the archive answered with a page where
// a cover should be. The identity is kept, nothing is stored, and the picture
// is asked for again in a month rather than an hour -- a page is an answer.
func TestAPictureThatIsNotOneIsRecorded(t *testing.T) {
	t.Parallel()
	l, s := newLibrary(t), newSources(t)
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	l.track(t, "music/amber/01.flac", song("Autechre", "Amber", "Foil", 1))
	s.notAPicture.Store(true)

	done, err := service(t, l, s, now).Pass(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if done.Albums != 1 || done.Covers != 0 {
		t.Errorf("Pass = %+v", done)
	}
	row, err := l.meta.AlbumLookup(t.Context(), owner, "Autechre", "Amber")
	if err != nil {
		t.Fatal(err)
	}
	if row.ReleaseGroup != amberGroup || row.CoverBlob != "" || !strings.Contains(row.Error, "not a picture") ||
		!row.RetryAt.Equal(now.Add(30*24*time.Hour)) {
		t.Errorf("the row is %+v", row)
	}
}

// TestASettledLibraryIsLeftAlone: the second pass over the same library asks
// nobody anything, which is what makes an hourly tick affordable.
func TestASettledLibraryIsLeftAlone(t *testing.T) {
	t.Parallel()
	l, s := newLibrary(t), newSources(t)
	l.track(t, "music/amber/01.flac", song("Autechre", "Amber", "Foil", 1))
	svc := service(t, l, s, time.Now())

	if _, err := svc.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	before := s.mbCalls.Load() + s.caaCalls.Load() + s.lrcCalls.Load()
	done, err := svc.Pass(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if after := s.mbCalls.Load() + s.caaCalls.Load() + s.lrcCalls.Load(); after != before || done != (lookup.Done{}) {
		t.Errorf("the second pass made %d requests and did %+v", after-before, done)
	}
}

// TestATaggedReleaseIsFetchedByItself: a tag that names the pressing and not
// the record is still an address the archive answers.
func TestATaggedReleaseIsFetchedByItself(t *testing.T) {
	t.Parallel()
	l, s := newLibrary(t), newSources(t)
	tagged := song("Someone", "A Pressing", "Track", 1)
	tagged.MBRelease = taggedGroup
	l.track(t, "music/pressing/01.flac", tagged)

	if _, err := service(t, l, s, time.Now()).Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := s.mbCalls.Load(); n != 0 {
		t.Errorf("MusicBrainz was asked %d times", n)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// The archive was asked under /release/ rather than /release-group/,
	// which the fake answers 404 to: nothing stored, and the row says so.
	row, err := l.meta.AlbumLookup(t.Context(), owner, "Someone", "A Pressing")
	if err != nil {
		t.Fatal(err)
	}
	if row.ReleaseGroup != "" || row.CoverBlob != "" || !strings.Contains(row.Error, "Cover Art Archive") {
		t.Errorf("the row is %+v", row)
	}
}

// TestWhatASourceCannotAnswerIsDeferred covers the answers that are not a
// verdict on the album: a server error, an address nothing listens on, and a
// body that is not what was asked for.
func TestWhatASourceCannotAnswerIsDeferred(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

	t.Run("a server error", func(t *testing.T) {
		t.Parallel()
		l := newLibrary(t)
		broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		t.Cleanup(broken.Close)
		l.track(t, "music/amber/01.flac", song("Autechre", "Amber", "Foil", 1))

		svc := lookup.New(l.meta, l.thumbs, "t", lookup.WithEndpoints(broken.URL, broken.URL, broken.URL),
			lookup.WithPace(0), lookup.WithClock(func() time.Time { return now }))
		done, err := svc.Pass(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		// A 500 is not a refusal, so the pass carries on to the track.
		if done.Deferred != 2 || done.More {
			t.Errorf("Pass = %+v, want both deferred", done)
		}
		row, err := l.meta.AlbumLookup(t.Context(), owner, "Autechre", "Amber")
		if err != nil || !row.RetryAt.Equal(now.Add(time.Hour)) || !strings.Contains(row.Error, "500") {
			t.Errorf("the album is %+v, %v", row, err)
		}
	})

	t.Run("nothing listening", func(t *testing.T) {
		t.Parallel()
		l := newLibrary(t)
		f := l.track(t, "music/amber/01.flac", song("Autechre", "Amber", "Foil", 1))
		nowhere := "http://127.0.0.1:1"
		svc := lookup.New(l.meta, l.thumbs, "t", lookup.WithEndpoints(nowhere, nowhere, nowhere),
			lookup.WithPace(0), lookup.WithClock(func() time.Time { return now }))
		done, err := svc.Pass(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if done.Deferred != 2 {
			t.Errorf("Pass = %+v", done)
		}
		words, err := l.meta.LyricsOf(t.Context(), []int64{f.ID})
		if err != nil || words[f.ID].Found() || !withinTheHour(words[f.ID].RetryAt, now) {
			t.Errorf("the track is %+v, %v", words[f.ID], err)
		}
	})

	t.Run("an answer that is not JSON", func(t *testing.T) {
		t.Parallel()
		l := newLibrary(t)
		page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("<html>maintenance</html>"))
		}))
		t.Cleanup(page.Close)
		l.track(t, "music/amber/01.flac", song("Autechre", "Amber", "Foil", 1))
		svc := lookup.New(l.meta, l.thumbs, "t", lookup.WithEndpoints(page.URL, page.URL, page.URL),
			lookup.WithPace(0), lookup.WithClock(func() time.Time { return now }))
		done, err := svc.Pass(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if done.Deferred != 2 {
			t.Errorf("Pass = %+v", done)
		}
	})

	t.Run("more than asked for", func(t *testing.T) {
		t.Parallel()
		l := newLibrary(t)
		huge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(bytes.Repeat([]byte("x"), 2<<20))
		}))
		t.Cleanup(huge.Close)
		l.track(t, "music/amber/01.flac", song("Autechre", "Amber", "Foil", 1))
		svc := lookup.New(l.meta, l.thumbs, "t", lookup.WithEndpoints(huge.URL, huge.URL, huge.URL),
			lookup.WithPace(0), lookup.WithClock(func() time.Time { return now }))
		done, err := svc.Pass(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		// Two megabytes is not a search answer and not the words of a song:
		// nothing there, with a month before asking again.
		if done.Albums != 1 || done.Lyrics != 1 || done.Deferred != 0 {
			t.Errorf("Pass = %+v", done)
		}
	})

	t.Run("a request the service could not read", func(t *testing.T) {
		t.Parallel()
		l := newLibrary(t)
		bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
		}))
		t.Cleanup(bad.Close)
		l.track(t, "music/amber/01.flac", song("Autechre", "Amber", "Foil", 1))
		svc := lookup.New(l.meta, l.thumbs, "t", lookup.WithEndpoints(bad.URL, bad.URL, bad.URL),
			lookup.WithPace(0), lookup.WithClock(func() time.Time { return now }))
		done, err := svc.Pass(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		// Our own query being wrong is not something an hour fixes.
		if done.Albums != 1 || done.Lyrics != 1 || done.Deferred != 0 {
			t.Errorf("Pass = %+v", done)
		}
		row, _ := l.meta.AlbumLookup(t.Context(), owner, "Autechre", "Amber")
		if !row.RetryAt.Equal(now.Add(30 * 24 * time.Hour)) {
			t.Errorf("the album is %+v, want a retry in a month", row)
		}
	})

	t.Run("a record with no words in it", func(t *testing.T) {
		t.Parallel()
		l := newLibrary(t)
		empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/get" {
				_ = json.NewEncoder(w).Encode(map[string]any{"trackName": "Foil", "instrumental": false})
				return
			}
			http.NotFound(w, r)
		}))
		t.Cleanup(empty.Close)
		f := l.track(t, "music/amber/01.flac", song("Autechre", "Amber", "Foil", 1))
		svc := lookup.New(l.meta, l.thumbs, "t", lookup.WithEndpoints(empty.URL, empty.URL, empty.URL),
			lookup.WithPace(0), lookup.WithClock(func() time.Time { return now }))
		if _, err := svc.Pass(t.Context()); err != nil {
			t.Fatal(err)
		}
		words, _ := l.meta.LyricsOf(t.Context(), []int64{f.ID})
		if words[f.ID].Found() || !words[f.ID].RetryAt.Equal(now.Add(30*24*time.Hour)) {
			t.Errorf("a record with no words is %+v, want nothing there", words[f.ID])
		}
	})
}

// TestARefusedLyricsLookupStopsItsHalf is the refusal on the other service:
// a 429 from LRCLIB ends the words' half of the pass and leaves the albums'
// alone.
func TestARefusedLyricsLookupStopsItsHalf(t *testing.T) {
	t.Parallel()
	l, s := newLibrary(t), newSources(t)
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	tired := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(tired.Close)
	l.track(t, "music/amber/01.flac", song("Autechre", "Amber", "Foil", 1))
	l.track(t, "music/amber/02.flac", song("Autechre", "Amber", "Montreal", 2))

	svc := lookup.New(l.meta, l.thumbs, "t", lookup.WithEndpoints(s.mb.URL, s.caa.URL, tired.URL),
		lookup.WithPace(0), lookup.WithClock(func() time.Time { return now }))
	done, err := svc.Pass(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if done.Albums != 1 || done.Lyrics != 0 || done.Deferred != 1 || !done.Refused {
		t.Errorf("Pass = %+v, want the album done, one track deferred and Refused said", done)
	}
}

// TestLRCLIBBusyIsAnHour: LRCLIB answers 503 for a track it is still fetching
// from outside, which is about the track and not about this client, and is
// usually answered an hour later. It is asked about again then, not in a
// month, and the pass goes on to the next one.
func TestLRCLIBBusyIsAnHour(t *testing.T) {
	t.Parallel()
	l, s := newLibrary(t), newSources(t)
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	busy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("track_name") == "Unheard" {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"message":"The server is busy, please retry in a moment","name":"ServerOverloaded","statusCode":503}`))
			return
		}
		s.lrc.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(busy.Close)
	unheard := l.track(t, "music/amber/01.flac", song("Autechre", "Amber", "Unheard", 1))
	foil := l.track(t, "music/amber/02.flac", song("Autechre", "Amber", "Foil", 2))

	svc := lookup.New(l.meta, l.thumbs, "t", lookup.WithEndpoints(s.mb.URL, s.caa.URL, busy.URL),
		lookup.WithPace(0), lookup.WithClock(func() time.Time { return now }))
	done, err := svc.Pass(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if done.Lyrics != 1 || done.Deferred != 1 || done.Refused {
		t.Errorf("Pass = %+v, want one track answered and the busy one deferred", done)
	}
	words, err := l.meta.LyricsOf(t.Context(), []int64{unheard.ID, foil.ID})
	if err != nil {
		t.Fatal(err)
	}
	if w := words[unheard.ID]; w.Found() || !withinTheHour(w.RetryAt, now) || !strings.Contains(w.Error, "503") {
		t.Errorf("the busy track is %+v, want a retry in an hour or so with the reason", w)
	}
	if !words[foil.ID].Found() {
		t.Errorf("the track behind the busy one was not asked about: %+v", words[foil.ID])
	}
}

// withinTheHour reports whether a deferred track's retry falls where the jitter
// puts it: an hour after now, and less than half an hour past that.
func withinTheHour(at, now time.Time) bool {
	return !at.Before(now.Add(time.Hour)) && at.Before(now.Add(90*time.Minute))
}

// TestAPassStopsWithItsContext: a shutdown in the middle of a batch is a
// return, not a batch finished first.
func TestAPassStopsWithItsContext(t *testing.T) {
	t.Parallel()
	l, s := newLibrary(t), newSources(t)
	l.track(t, "music/amber/01.flac", song("Autechre", "Amber", "Foil", 1))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := service(t, l, s, time.Now()).Pass(ctx); err == nil {
		t.Error("a pass ran to the end on a context that had ended")
	}
}

// TestADatabaseThatRefusesFailsThePass: unlike a source, the index is this
// server's own, and a pass that cannot read it has nothing to do.
func TestADatabaseThatRefusesFailsThePass(t *testing.T) {
	t.Parallel()
	for _, method := range []string{"PruneAlbumLookups", "PendingAlbumLookups", "PendingLyrics", "PutAlbumLookup", "PutLyrics", "Tracks"} {
		t.Run(method, func(t *testing.T) {
			t.Parallel()
			l, s := newLibrary(t), newSources(t)
			l.track(t, "music/amber/01.flac", song("Autechre", "Amber", "Foil", 1))
			broken := dbtest.FailOn(t, l.meta, method)
			svc := lookup.New(broken, l.thumbs, "t", lookup.WithEndpoints(s.mb.URL, s.caa.URL, s.lrc.URL), lookup.WithPace(0))
			if _, err := svc.Pass(t.Context()); !errors.Is(err, dbtest.ErrInjected) {
				t.Errorf("Pass with %s refusing = %v, want the injected failure", method, err)
			}
		})
	}
}
