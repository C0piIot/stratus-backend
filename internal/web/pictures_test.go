package web_test

import (
	"strings"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/auth"
	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/media"
	"github.com/C0piIot/stratus-backend/internal/web"
)

// What a page does with the verdict on a row (#341). Before the column there
// was nothing to read in batch -- the note beside a picture that was not made
// is one object per blob, and a listing is a hundred rows -- so every page
// offered a thumbnail for everything this build could name and let the ones
// that could not be made come back 404. The mark under them meant that was
// not a broken image; it was still a request per file per page view.

// TestAListingDoesNotOfferAPictureNothingCanMake is that, in a folder: the row
// keeps its mark and loses the request.
func TestAListingDoesNotOfferAPictureNothingCanMake(t *testing.T) {
	t.Parallel()
	h, s, meta := browserOver(t)
	cookie := signIn(t, h)
	mkdir(t, s, "holiday")
	refused := picture(t, s, meta, "holiday/IMG_0001.jpg")
	picture(t, s, meta, "holiday/IMG_0002.jpg")
	refuse(t, meta, refused, time.Now())

	listing := get(t, h, "/files/holiday/", cookie).Body.String()
	if strings.Contains(listing, "/thumb/holiday/IMG_0001.jpg") {
		t.Errorf("the listing asked for a picture nothing can make:\n%s", listing)
	}
	if !strings.Contains(listing, "/thumb/holiday/IMG_0002.jpg") {
		t.Error("the listing stopped offering the picture it can make")
	}
	// Both rows still draw the mark, which is what the square shows either
	// way: one is waiting for a picture and the other is never getting one.
	if n := strings.Count(listing, glyphBox); n != 2 {
		t.Errorf("the listing drew %d marks, want one per photograph", n)
	}
}

// TestAListingOffersAPictureAgainOnceTheRefusalIsStale is what keeps a page
// that stopped asking from being the reason nothing ever tries again: a
// verdict is good for the week the pass gives it, because what this build
// cannot draw an upgrade may.
func TestAListingOffersAPictureAgainOnceTheRefusalIsStale(t *testing.T) {
	t.Parallel()
	h, s, meta := browserOver(t)
	cookie := signIn(t, h)
	mkdir(t, s, "holiday")
	refused := picture(t, s, meta, "holiday/IMG_0001.jpg")
	refuse(t, meta, refused, time.Now().Add(-files.CacheRetention-time.Hour))

	listing := get(t, h, "/files/holiday/", cookie).Body.String()
	if !strings.Contains(listing, "/thumb/holiday/IMG_0001.jpg") {
		t.Errorf("a verdict a week old is still being believed:\n%s", listing)
	}
}

// TestAGridDoesNotOfferAPictureNothingCanMake is the same of the gallery,
// which is the grid #324 was about: forty tiles is forty requests, and the
// ones nothing can draw are the ones that cost a decode to find out.
func TestAGridDoesNotOfferAPictureNothingCanMake(t *testing.T) {
	t.Parallel()
	h, s, meta := browserOver(t)
	cookie := signIn(t, h)
	taken := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	refused := addPhoto(t, s, meta, "IMG_0001.jpg", taken, "")
	addPhoto(t, s, meta, "IMG_0002.jpg", taken.Add(time.Hour), "")
	refuse(t, meta, refused, time.Now())

	grid := get(t, h, "/photos/", cookie).Body.String()
	if strings.Contains(grid, "/thumb/IMG_0001.jpg") {
		t.Errorf("the grid asked for a picture nothing can make:\n%s", grid)
	}
	if !strings.Contains(grid, "/thumb/IMG_0002.jpg") {
		t.Error("the grid stopped offering the picture it can make")
	}
}

// TestStatusCountsThePictures is the other half of the column a person reads:
// a first pass over a library takes hours, and the number that moves is not
// the one about metadata.
func TestStatusCountsThePictures(t *testing.T) {
	t.Parallel()
	h, s, meta := browserOver(t)
	cookie := signIn(t, h)
	made := picture(t, s, meta, "IMG_0001.jpg")
	refused := picture(t, s, meta, "IMG_0002.jpg")
	picture(t, s, meta, "IMG_0003.jpg")
	drew(t, meta, made)
	refuse(t, meta, refused, time.Now())

	body := get(t, h, "/status", cookie).Body.String()
	if !strings.Contains(body, `data-count="pictures">1 of 3<`) {
		t.Errorf("the page does not say how many pictures are made:\n%s", body)
	}
	if !strings.Contains(body, `data-count="no-picture">1<`) {
		t.Error("the page does not say how many cannot have one")
	}
	if strings.Contains(body, "STRATUS_THUMBS_AHEAD") {
		t.Error("the page says the pass is off while it is on")
	}
}

// TestStatusSaysWhenPicturesAreOnlyMadeOnRequest is the other state of that
// switch, and it has to be said: with the pass off those numbers move only as
// somebody browses, which reads as a library that never finishes.
func TestStatusSaysWhenPicturesAreOnlyMadeOnRequest(t *testing.T) {
	t.Parallel()
	s, thumbs, meta := pieces(t)
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
		Indexing:  web.Indexing{Index: meta, Interval: time.Minute},
		Imports:   nil,
		Playback:  web.Playback{},
	})
	picture(t, s, meta, "IMG_0001.jpg")

	body := get(t, h, "/status", signIn(t, h)).Body.String()
	if !strings.Contains(body, "STRATUS_THUMBS_AHEAD") {
		t.Errorf("the page says nothing about the pass being off:\n%s", body)
	}
}

// picture writes a photograph and gives it the row a verdict hangs off.
func picture(t *testing.T, s *files.Service, meta db.Store, path string) db.File {
	t.Helper()
	f, err := s.Write(t.Context(), username, path, strings.NewReader("pixels"), 6, "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	row := db.Media{FileID: f.ID, Kind: db.KindImage, IndexedAt: time.Now(), Version: media.Version, ETag: f.ETag}
	if err := meta.PutMedia(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	return f
}

// refuse says this build could not draw that file, at the moment given.
func refuse(t *testing.T, meta db.Store, f db.File, at time.Time) {
	t.Helper()
	verdict := db.ThumbVerdict{
		FileID: f.ID, State: db.ThumbRefused, BlobKey: f.BlobKey,
		Generation: files.DerivedGeneration, At: at,
	}
	if err := meta.PutThumbVerdict(t.Context(), verdict); err != nil {
		t.Fatal(err)
	}
}

// drew says the pictures of that file are in the store.
func drew(t *testing.T, meta db.Store, f db.File) {
	t.Helper()
	verdict := db.ThumbVerdict{
		FileID: f.ID, State: db.ThumbMade, BlobKey: f.BlobKey,
		Generation: files.DerivedGeneration, At: time.Now(),
	}
	if err := meta.PutThumbVerdict(t.Context(), verdict); err != nil {
		t.Fatal(err)
	}
}
