package web_test

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"strings"
	"testing"

	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/storage/storagetest"
)

// photoJPEG is a picture big enough that a thumbnail of it is a reduction
// rather than a copy.
func photoJPEG(t *testing.T) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 400, 300))
	for y := range 300 {
		for x := range 400 {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 128, A: 255})
		}
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, nil); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// TestThumbnailOfAPhoto is what the listing's <img> asks for: the first request
// makes the picture, and what comes back is a JPEG the browser may cache
// forever, because the URL carries the ETag of the file it is of.
func TestThumbnailOfAPhoto(t *testing.T) {
	t.Parallel()
	h, s := browser(t)
	write(t, s, "holiday.jpg", photoJPEG(t))
	cookie := signIn(t, h)

	rec := get(t, h, "/thumb/holiday.jpg?size=96", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET a thumbnail = %d, want 200: %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Content-Type"); got != "image/jpeg" {
		t.Errorf("Content-Type = %q, want image/jpeg", got)
	}
	if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Errorf("Cache-Control = %q, want it cacheable forever: the URL carries the ETag", got)
	}
	if _, _, err := image.Decode(bytes.NewReader(rec.Body.Bytes())); err != nil {
		t.Errorf("what came back does not decode as an image: %v", err)
	}

	// And again, which is the path that reads the derived blob rather than
	// making it. Same bytes either way.
	again := get(t, h, "/thumb/holiday.jpg?size=96", cookie)
	if !bytes.Equal(again.Body.Bytes(), rec.Body.Bytes()) {
		t.Error("the second request answered with different bytes")
	}
}

// TestThumbnailOfWhatCannotBeOne: a text file, a folder and a file that is not
// there all answer the same way, because from a browser they are the same thing
// and telling them apart would tell a guesser what exists.
func TestThumbnailOfWhatCannotBeOne(t *testing.T) {
	t.Parallel()
	h, s := browser(t)
	write(t, s, "notes.txt", "not a picture")
	mkdir(t, s, "folder")
	cookie := signIn(t, h)

	for _, target := range []string{"/thumb/notes.txt", "/thumb/folder", "/thumb/missing.jpg"} {
		if code := get(t, h, target, cookie).Code; code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", target, code)
		}
	}
}

// TestThumbnailNeedsASession, like every other page: the pictures in a library
// are the library.
func TestThumbnailNeedsASession(t *testing.T) {
	t.Parallel()
	h, s := browser(t)
	write(t, s, "holiday.jpg", photoJPEG(t))

	rec := get(t, h, "/thumb/holiday.jpg")
	if rec.Code != http.StatusSeeOther {
		t.Errorf("GET a thumbnail with no session = %d, want 303 to the login form", rec.Code)
	}
}

// TestTheListingShowsPictures is the point of the whole thing: a row for a
// photograph carries an image and a row for anything else does not.
func TestTheListingShowsPictures(t *testing.T) {
	t.Parallel()
	h, s := browser(t)
	write(t, s, "holiday.jpg", photoJPEG(t))
	write(t, s, "notes.txt", "not a picture")
	cookie := signIn(t, h)

	body := get(t, h, "/files/", cookie).Body.String()
	if !strings.Contains(body, `src="/thumb/holiday.jpg`) {
		t.Errorf("the listing has no picture for the photograph:\n%s", excerpt(body, "<tr"))
	}
	if strings.Contains(body, "/thumb/notes.txt") {
		t.Error("the listing asks for a picture of a text file")
	}
	// Lazily, or opening a folder of five hundred photographs asks for five
	// hundred pictures at once.
	if !strings.Contains(body, `loading="lazy"`) {
		t.Error("the images are not lazy")
	}
}

// TestThumbnailWhenTheStoreRefuses: an error that is not "there is no picture"
// is a page rather than a 404, because a 404 would tell the browser to stop
// asking for something that may well be there tomorrow.
func TestThumbnailWhenTheStoreRefuses(t *testing.T) {
	t.Parallel()
	blobs, meta := backends(t)
	working := files.New(blobs, meta)
	write(t, working, "holiday.jpg", photoJPEG(t))

	h := handlerOver(t, working, storagetest.FailOn(t, blobs, "Get"), meta)
	if code := get(t, h, "/thumb/holiday.jpg", signIn(t, h)).Code; code != http.StatusInternalServerError {
		t.Errorf("a thumbnail through a store that refuses = %d, want 500", code)
	}
}

// TestThumbnailOfAnImpossiblePath: the same chokepoint every other page goes
// through, because a URL is the one thing a browser lets somebody type.
func TestThumbnailOfAnImpossiblePath(t *testing.T) {
	t.Parallel()
	h, _ := browser(t)
	cookie := signIn(t, h)

	// A size that is not a number is not an error: it falls back to the one the
	// listing asks for, because a broken query string should not cost a picture.
	if code := get(t, h, "/thumb/../escape?size=enormous", cookie).Code; code == http.StatusOK {
		t.Error("a path that climbs out of the tree was served")
	}
	// A name the tree cannot hold at all. It reaches the handler because a
	// router only cleans slashes, and it is refused by the same validation
	// every other page uses.
	if code := get(t, h, "/thumb/bad%00name.jpg", cookie).Code; code == http.StatusOK {
		t.Error("a path with a control character in it was served")
	}
}
