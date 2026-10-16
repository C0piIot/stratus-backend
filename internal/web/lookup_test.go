package web_test

import (
	"bytes"
	htmlstd "html"
	"image"
	"image/png"
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

// What this file is about: what outside sources said about the music library
// (#217), where the pages show it -- the words under a track on its album
// page, and where the library stands on /status.

func TestTheAlbumPageShowsTheWords(t *testing.T) {
	t.Parallel()
	h, s, meta := browserOver(t)
	cookie := signIn(t, h)
	one, two := homework(t, s, meta)
	if err := meta.PutLyrics(t.Context(), db.Lyrics{
		FileID: one.ID, ETag: one.ETag, FetchedAt: time.Now(), Plain: "It's a long way to the top\nIf you wanna rock 'n' roll",
	}); err != nil {
		t.Fatal(err)
	}
	// The second track has only the synced form, which the page shows with
	// the times taken off: a page is read, not sung along to.
	if err := meta.PutLyrics(t.Context(), db.Lyrics{
		FileID: two.ID, ETag: two.ETag, FetchedAt: time.Now(), Synced: "[00:12.00] Riding down the highway\n[00:15.50] Going to a show\n[unterminated line",
	}); err != nil {
		t.Fatal(err)
	}

	album := htmlstd.UnescapeString(get(t, h, "/music/AC_DC/High%20Voltage/", cookie).Body.String())
	for _, want := range []string{"<details", "Lyrics</summary>", "It's a long way to the top<br>", "If you wanna rock 'n' roll<br>", "Riding down the highway<br>"} {
		if !strings.Contains(album, want) {
			t.Errorf("the album page has no %q:\n%s", want, album)
		}
	}
	if strings.Contains(album, "[00:12.00]") {
		t.Error("the page shows the times in front of the synced words")
	}
	if !strings.Contains(album, "[unterminated line<br>") {
		t.Error("a line that is not a time was cut")
	}
	if strings.Count(album, "<details") != 2 {
		t.Errorf("the page folds the words %d times, want once per track that has any", strings.Count(album, "<details"))
	}
}

func TestTheAlbumPageWithoutWordsHasNothingToFold(t *testing.T) {
	t.Parallel()
	h, s, meta := browserOver(t)
	cookie := signIn(t, h)
	homework(t, s, meta)

	album := get(t, h, "/music/AC_DC/High%20Voltage/", cookie).Body.String()
	if strings.Contains(album, "<details") {
		t.Error("the page folds words it does not have")
	}
}

func TestTheAlbumPageOverABackendThatWillNotAnswerForTheWords(t *testing.T) {
	t.Parallel()
	blobs, meta := backends(t)
	broken := dbtest.FailOn(t, meta, "LyricsOf")
	s := files.New(blobs, broken)
	creds := credentials()
	h := web.Handler(version, buildDate, creds, auth.NewSessions(creds, auth.DefaultSessionTTL), auth.NewShares(creds),
		s, media.NewThumbs(blobs, s, broken, "ffmpeg", t.TempDir()), broken,
		web.Indexing{Index: broken, Interval: time.Minute}, nil, web.Video{})
	cookie := signIn(t, h)
	homework(t, s, meta)

	if rec := get(t, h, "/music/AC_DC/High%20Voltage/", cookie); rec.Code != http.StatusInternalServerError {
		t.Errorf("the album page over a database that refuses the words = %d, want 500", rec.Code)
	}
}

// TestStatusSaysWhereTheLookupsStand: the two lines appear when the pass is
// on, say so when it is off and something was fetched before, and are not
// there at all when nothing is asked and nothing was ever fetched.
func TestStatusSaysWhereTheLookupsStand(t *testing.T) {
	t.Parallel()
	blobs, meta := backends(t)
	s := files.New(blobs, meta)
	one, _ := homework(t, s, meta)

	off := handlerIndexing(t, s, blobs, web.Indexing{Index: meta, Interval: time.Minute, Lookups: meta})
	body := get(t, off, "/status", signIn(t, off)).Body.String()
	if strings.Contains(body, "Albums identified") || strings.Contains(body, "Lyrics found") {
		t.Errorf("the page reports lookups that are off and never ran:\n%s", body)
	}

	on := handlerIndexing(t, s, blobs, web.Indexing{Index: meta, Interval: time.Minute, Lookups: meta, Lookup: true})
	body = get(t, on, "/status", signIn(t, on)).Body.String()
	for _, want := range []string{"Albums identified", "0 of 1", "0 with a fetched cover", "Lyrics found", "0 of 1", "0 asked about"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page with lookups on has no %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "STRATUS_MUSIC_LOOKUP") {
		t.Error("the page says the setting is off while it is on")
	}

	if err := meta.PutAlbumLookup(t.Context(), db.AlbumLookup{
		Owner: username, Artist: "AC/DC", Album: "High Voltage", LookedUpAt: time.Now(),
		ReleaseGroup: "4cf8ba42-5d0b-3a4d-8e8f-4c8f2f3d1a1b", CoverBlob: one.BlobKey,
	}); err != nil {
		t.Fatal(err)
	}
	body = get(t, off, "/status", signIn(t, off)).Body.String()
	for _, want := range []string{"Albums identified", "1 of 1", "1 with a fetched cover", "STRATUS_MUSIC_LOOKUP"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page with lookups off and a row has no %q:\n%s", want, body)
		}
	}
}

func TestStatusWhenTheLookupCountsWillNotAnswer(t *testing.T) {
	t.Parallel()
	blobs, meta := backends(t)
	broken := dbtest.FailOn(t, meta, "LookupCounts")
	s := files.New(blobs, broken)
	h := handlerIndexing(t, s, blobs, web.Indexing{Index: broken, Interval: time.Minute, Lookups: broken, Lookup: true})

	// One line on a page about something else: logged, and the page goes out
	// without it.
	rec := get(t, h, "/status", signIn(t, h))
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "Albums identified") {
		t.Errorf("GET /status = %d with the lookup counts refusing; want 200 without the lines", rec.Code)
	}
}

// TestAnAlbumsCoverInAFlatFolderIsItsOwn is #217's flat-library bug on the
// album page: the cover is asked for through the album's own first track, so
// an album with none does not get its neighbour's.
func TestAnAlbumsCoverInAFlatFolderIsItsOwn(t *testing.T) {
	t.Parallel()
	blobs, meta := backends(t)
	s := files.New(blobs, meta)
	creds := credentials()
	thumbs := media.NewThumbs(blobs, s, meta, "ffmpeg", t.TempDir()).WithLookups(meta)
	h := web.Handler(version, buildDate, creds, auth.NewSessions(creds, auth.DefaultSessionTTL), auth.NewShares(creds),
		s, thumbs, meta, indexing(meta), nil, web.Video{})
	cookie := signIn(t, h)

	addTrack(t, s, meta, "80KIDZ - This Is My Shit.flac", db.Media{
		AlbumArtist: "80KIDZ", Artist: "80KIDZ", Album: "This Is My Shit", Title: "This Is My Shit", TrackNo: 1,
	})
	obel := addTrack(t, s, meta, "Agnes Obel - Familiar.flac", db.Media{
		AlbumArtist: "Agnes Obel", Artist: "Agnes Obel", Album: "Aventine", Title: "Familiar", TrackNo: 1,
	})
	var picture bytes.Buffer
	if err := png.Encode(&picture, image.NewRGBA(image.Rect(0, 0, 300, 300))); err != nil {
		t.Fatal(err)
	}
	if err := thumbs.StoreArt(t.Context(), obel, picture.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := meta.PutAlbumLookup(t.Context(), db.AlbumLookup{
		Owner: username, Artist: "Agnes Obel", Album: "Aventine", LookedUpAt: time.Now(),
		ReleaseGroup: "4cf8ba42-5d0b-3a4d-8e8f-4c8f2f3d1a1b", CoverBlob: obel.BlobKey,
	}); err != nil {
		t.Fatal(err)
	}

	if rec := get(t, h, "/music/Agnes%20Obel/Aventine/?cover=96", cookie); rec.Code != http.StatusOK {
		t.Errorf("the album with a fetched cover = %d", rec.Code)
	}
	if rec := get(t, h, "/music/80KIDZ/This%20Is%20My%20Shit/?cover=96", cookie); rec.Code != http.StatusNotFound {
		t.Errorf("the album with no cover of its own = %d, want 404 rather than its neighbour's picture", rec.Code)
	}
}
