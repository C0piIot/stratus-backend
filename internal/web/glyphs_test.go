package web_test

import (
	"strings"
	"testing"
	"time"
)

// What this file is about: the square that has no picture in it. A grid of
// those used to be the browser's broken-image icon repeated, and what makes
// the mark show instead is two things a change to the markup can undo without
// failing anything else -- the glyph under the picture, and the empty alt that
// keeps a picture which never arrives from drawing over it (#324).

// The drawings themselves, as a piece of each path: which mark a page shows is
// the point, so a test that only counted them would pass with the wrong one.
const (
	photoMark = "M14 73l21-24"
	filmMark  = "M26 14l54 34-54 34z"
	coverMark = "rotate(-18 28 70)"
)

func TestAPhotoWithNoPictureShowsTheMarkInstead(t *testing.T) {
	t.Parallel()
	h, s, meta := browserOver(t)
	cookie := signIn(t, h)
	addPhoto(t, s, meta, "scan.tiff", time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC), "")

	grid := get(t, h, "/photos/", cookie).Body.String()
	if strings.Contains(grid, "/thumb/scan.tiff") {
		t.Error("a thumbnail was offered for a file this build cannot decode")
	}
	if !strings.Contains(grid, photoMark) {
		t.Errorf("the tile is empty rather than a mark:\n%s", grid)
	}
	// And the name is still what the link is called, where a reader and a
	// pointer can both reach it.
	if !strings.Contains(grid, `class="visually-hidden">scan.tiff</span>`) {
		t.Error("the tile does not name the file to a screen reader")
	}
}

// TestAFilmWithNoPictureShowsTheFilmMark: the same square on the other
// library, and a different drawing -- which is the library's kind and not
// whether a duration has been read, since a recording nothing has looked at
// is still a film.
func TestAFilmWithNoPictureShowsTheFilmMark(t *testing.T) {
	t.Parallel()
	h, s, meta := browserOver(t)
	cookie := signIn(t, h)
	addVideo(t, s, meta, "holiday.avi", time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC), 0)

	grid := get(t, h, "/videos/", cookie).Body.String()
	if !strings.Contains(grid, filmMark) || strings.Contains(grid, photoMark) {
		t.Errorf("a film with no picture is not drawn as one:\n%s", grid)
	}
}

// TestThePictureOverTheMarkIsDecorative is the half that is easy to undo by
// improving it: an <img> that fails with alt text draws the text and a
// broken-image icon over the mark, and one with alt="" draws nothing at all.
// So the name belongs in the link and the picture carries no alt, which is
// what makes a refused thumbnail look like a file with no picture rather than
// like a page that broke.
func TestThePictureOverTheMarkIsDecorative(t *testing.T) {
	t.Parallel()
	h, s, meta := browserOver(t)
	cookie := signIn(t, h)
	addPhoto(t, s, meta, "IMG_0001.jpg", time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC), "")

	grid := get(t, h, "/photos/", cookie).Body.String()
	if !strings.Contains(grid, "/thumb/IMG_0001.jpg") {
		t.Fatalf("no thumbnail was offered at all:\n%s", grid)
	}
	if !strings.Contains(grid, `alt=""`) || strings.Contains(grid, `alt="IMG_0001.jpg"`) {
		t.Errorf("the picture is not decorative:\n%s", grid)
	}
	// The mark is under it either way, which is also what a grid shows while
	// it is still fetching.
	if !strings.Contains(grid, photoMark) {
		t.Error("the picture was drawn with nothing under it")
	}
}

// TestAnAlbumWithNoCoverShowsTheCoverMark: the same square again, on the pages
// where an empty one is the normal answer for half a library.
func TestAnAlbumWithNoCoverShowsTheCoverMark(t *testing.T) {
	t.Parallel()
	h, s, meta := browserOver(t)
	cookie := signIn(t, h)
	homework(t, s, meta)

	for _, at := range []string{"/music/AC_DC/", "/music/AC_DC/High%20Voltage/"} {
		if page := get(t, h, at, cookie).Body.String(); !strings.Contains(page, coverMark) {
			t.Errorf("%s draws an empty square rather than a mark:\n%s", at, page)
		}
	}
}
