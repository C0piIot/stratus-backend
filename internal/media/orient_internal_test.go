package media

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
)

// The eight EXIF orientations, checked by where one corner ends up.
//
// A picture with four different corners is the only way to catch the failure
// that matters: a rotation with the wrong sign returns an image of exactly the
// right size with the pixels in the wrong places, which no dimension check
// notices.
func TestOrient(t *testing.T) {
	t.Parallel()

	// Three wide and two tall, so a quarter turn is visible in the bounds as
	// well, with a different colour in each corner.
	const (
		w = 3
		h = 2
	)
	topLeft := color.RGBA{R: 255, A: 255}
	topRight := color.RGBA{G: 255, A: 255}
	bottomLeft := color.RGBA{B: 255, A: 255}

	src := image.NewRGBA(image.Rect(0, 0, w, h))
	src.Set(0, 0, topLeft)
	src.Set(w-1, 0, topRight)
	src.Set(0, h-1, bottomLeft)

	tests := []struct {
		orientation int
		wantW       int
		wantH       int
		// where the pixel that was top left ends up.
		wantX int
		wantY int
	}{
		{1, w, h, 0, 0},
		{2, w, h, w - 1, 0},
		{3, w, h, w - 1, h - 1},
		{4, w, h, 0, h - 1},
		{5, h, w, 0, 0},
		{6, h, w, h - 1, 0},
		{7, h, w, h - 1, w - 1},
		{8, h, w, 0, w - 1},
	}

	for _, tt := range tests {
		t.Run(string(rune('0'+tt.orientation)), func(t *testing.T) {
			t.Parallel()
			got := orient(src, tt.orientation)

			if b := got.Bounds(); b.Dx() != tt.wantW || b.Dy() != tt.wantH {
				t.Fatalf("orientation %d: bounds = %v, want %dx%d", tt.orientation, b, tt.wantW, tt.wantH)
			}
			if r, g, b, _ := got.At(tt.wantX, tt.wantY).RGBA(); r>>8 != 255 || g != 0 || b != 0 {
				t.Errorf("orientation %d: the top-left pixel is not at (%d, %d)",
					tt.orientation, tt.wantX, tt.wantY)
			}
		})
	}

	// Nothing to do, and nothing done: the ordinary photograph says 1, and a
	// file whose tag is missing or nonsense says something outside the range.
	for _, orientation := range []int{0, 1, 9, -1} {
		if got := orient(src, orientation); got != image.Image(src) {
			t.Errorf("orientation %d rebuilt the image instead of leaving it alone", orientation)
		}
	}
}

// TestThumbnailComesOutTheRightWayUp is the whole point, through the path a
// browser uses: a photograph taken with the phone upright is stored on its side
// with a tag saying so, and the picture in the listing has to be the right way
// up like the HEIC beside it (#141).
func TestThumbnailComesOutTheRightWayUp(t *testing.T) {
	t.Parallel()

	// Stored as it comes off the sensor -- wider than it is tall -- with the
	// tag that says to turn it a quarter clockwise.
	const (
		stored = 40
		short  = 20
	)
	photo := exifJPEGWithPixels(t, 6, stored, short)

	made, err := reduceTo(bytes.NewReader(photo), "IMG_0001.jpg", thumbSmall)
	if err != nil {
		t.Fatalf("reduceTo: %v", err)
	}
	got, err := jpeg.Decode(bytes.NewReader(made))
	if err != nil {
		t.Fatalf("what came back is not a JPEG: %v", err)
	}
	if b := got.Bounds(); b.Dx() != short || b.Dy() != stored {
		t.Errorf("bounds = %v, want %dx%d: the picture was not turned", b, short, stored)
	}
}

// TestThumbnailOfAPictureThatSaysNothing: no EXIF at all is the ordinary case
// for a screenshot, and it has to come back exactly as it went in.
func TestThumbnailOfAPictureThatSaysNothing(t *testing.T) {
	t.Parallel()

	var plain bytes.Buffer
	if err := jpeg.Encode(&plain, image.NewRGBA(image.Rect(0, 0, 40, 20)), nil); err != nil {
		t.Fatal(err)
	}

	made, err := reduceTo(bytes.NewReader(plain.Bytes()), "screenshot.jpg", thumbSmall)
	if err != nil {
		t.Fatalf("reduceTo: %v", err)
	}
	got, err := jpeg.Decode(bytes.NewReader(made))
	if err != nil {
		t.Fatal(err)
	}
	if b := got.Bounds(); b.Dx() != 40 || b.Dy() != 20 {
		t.Errorf("bounds = %v, want the picture untouched", b)
	}
}
