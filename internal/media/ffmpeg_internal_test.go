package media

import (
	"errors"
	"image"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// stubFFmpeg is what the tests that never reach the binary are wired with: a
// script that fails. The toolchain container has no media tools, so a test that
// did reach it fails saying so rather than quietly finding a real one.
func stubFFmpeg(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ffmpeg")
	script := "#!/bin/sh\necho 'this is the stub, not ffmpeg' >&2\nexit 1\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// realFFmpeg is the binary itself, where there is one. There is not one in the
// toolchain container, which is why these skip: what runs it for real is
// scripts/smoke.sh, inside the image that carries it.
func realFFmpeg(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("no ffmpeg on the PATH")
	}
	return path
}

// TestFrameFromRaw is the arithmetic that stands in for a header rawvideo does
// not have. Getting it wrong would not fail loudly -- it would render somebody's
// photograph as diagonal noise -- so every way it can fail is refused here.
func TestFrameFromRaw(t *testing.T) {
	t.Parallel()

	t.Run("whole rows", func(t *testing.T) {
		t.Parallel()
		frame, err := frameFromRaw(make([]byte, 4*4*3), 4)
		if err != nil {
			t.Fatal(err)
		}
		if b := frame.Bounds(); b.Dx() != 4 || b.Dy() != 3 {
			t.Errorf("bounds = %v, want 4x3", b)
		}
	})

	t.Run("nothing at all", func(t *testing.T) {
		t.Parallel()
		// What ffmpeg produces when it was asked for a frame past the end of a
		// clip, which is the case the second run exists for.
		if _, err := frameFromRaw(nil, 96); err == nil {
			t.Error("no frame was read as a picture")
		}
	})

	t.Run("rows that do not divide", func(t *testing.T) {
		t.Parallel()
		if _, err := frameFromRaw(make([]byte, 4*4*3+1), 4); err == nil {
			t.Error("a frame that is not whole rows was read as a picture")
		}
	})

	t.Run("more than a thumbnail can be", func(t *testing.T) {
		t.Parallel()
		if _, err := frameFromRaw(make([]byte, maxFrameBytes+4), 1); err == nil {
			t.Error("a frame past the bound was accepted")
		}
	})
}

// TestDecodeScaledWhenFFmpegFails: a picture that cannot be made is an answer,
// not a crash, and the reason ffmpeg gave has to reach the log.
func TestDecodeScaledWhenFFmpegFails(t *testing.T) {
	t.Parallel()

	_, err := decodeScaled(t.Context(), stubFFmpeg(t), "whatever.heic", 96, false)
	if err == nil {
		t.Fatal("a failing ffmpeg produced a thumbnail")
	}
	if !strings.Contains(err.Error(), "this is the stub") {
		t.Errorf("err = %v, want what the binary said on stderr", err)
	}
}

// TestDecodeScaledAsksTwiceForAVideo: the offset is past the end of a short
// clip, so the second run without it is what answers. With a binary that always
// fails, what this pins is that both runs happen and the reason still comes
// back.
func TestDecodeScaledAsksTwiceForAVideo(t *testing.T) {
	t.Parallel()

	_, err := decodeScaled(t.Context(), stubFFmpeg(t), "clip.mp4", 96, true)
	if err == nil {
		t.Fatal("a failing ffmpeg produced a frame")
	}
	if !strings.Contains(err.Error(), "this is the stub") {
		t.Errorf("err = %v, want what the binary said", err)
	}
}

// TestThumbnailWhenTheSpoolFails: ffmpeg opens a file, so a thumbnail needs
// somewhere to put one. Nowhere to put it is a thumbnail that cannot be made,
// not a page that breaks.
func TestThumbnailWhenTheSpoolFails(t *testing.T) {
	t.Parallel()

	th, service, _ := thumbs(t)
	th.tmpDir = filepath.Join(t.TempDir(), "not", "there")
	write(t, service, "holiday.heic", readFixture(t, "tiny.heic"), "image/heic")

	if _, _, err := th.File(t.Context(), owner, "holiday.heic", 96); err == nil {
		t.Error("a thumbnail was made with nowhere to spool the file")
	}
}

// TestDecodeScaledReadsAHEIC is the format this whole path exists for: what an
// iPhone records, which no pure-Go decoder reads without cgo.
func TestDecodeScaledReadsAHEIC(t *testing.T) {
	t.Parallel()
	ffmpeg := realFFmpeg(t)

	// The fixture is 96x64, so asking for 32 gives 32x21 -- the same numbers the
	// recipe in build/ffmpeg/Dockerfile asserts while it builds the binary.
	frame, err := decodeScaled(t.Context(), ffmpeg, filepath.Join("testdata", "tiny.heic"), 32, false)
	if err != nil {
		t.Fatalf("decodeScaled: %v", err)
	}
	if b := frame.Bounds(); b.Dx() != 32 || b.Dy() != 21 {
		t.Errorf("bounds = %v, want 32x21", b)
	}
	// And it is a picture rather than a buffer of zeros: the fixture is a test
	// pattern, so something in it is not black.
	if opaqueBlack(frame) {
		t.Error("the frame is entirely black, which the fixture is not")
	}
}

// TestDecodeScaledTakesAFrameOutOfAVideo: the same path, with the extra
// question of which frame. The fixture is one second long, so the offset is
// past the end and the second run is what answers.
func TestDecodeScaledTakesAFrameOutOfAVideo(t *testing.T) {
	t.Parallel()
	ffmpeg := realFFmpeg(t)

	frame, err := decodeScaled(t.Context(), ffmpeg, filepath.Join("testdata", "moov-last.mp4"), 96, true)
	if err != nil {
		t.Fatalf("decodeScaled: %v", err)
	}
	if b := frame.Bounds(); b.Dx() != 96 || b.Dy() != 72 {
		t.Errorf("bounds = %v, want 96x72 for a 320x240 film", b)
	}
	if opaqueBlack(frame) {
		t.Error("the frame is entirely black")
	}
}

// TestThumbnailOfAHEICIsAJPEG walks the whole of what a browser asks for, with
// the real binary: a HEIC in the tree, and a JPEG of it out of the blob store.
func TestThumbnailOfAHEICIsAJPEG(t *testing.T) {
	t.Parallel()
	ffmpeg := realFFmpeg(t)

	th, service, _ := thumbs(t)
	th.ffmpeg = ffmpeg
	write(t, service, "holiday.heic", readFixture(t, "tiny.heic"), "image/heic")

	body, size, err := th.File(t.Context(), owner, "holiday.heic", 96)
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	defer func() { _ = body.Close() }()

	made, err := jpeg.Decode(body)
	if err != nil {
		t.Fatalf("what came back is not a JPEG: %v", err)
	}
	if size <= 0 {
		t.Errorf("size = %d", size)
	}
	// 96x64 asked for at 96 is served as it is: the thumbnail path never
	// enlarges, and neither does the scale filter it hands the work to.
	if b := made.Bounds(); b.Dx() != 96 || b.Dy() != 64 {
		t.Errorf("bounds = %v, want the source size", b)
	}

	// And the second ask is the stored one, which is the point of the cache:
	// breaking the binary must not break a thumbnail that already exists.
	th.ffmpeg = stubFFmpeg(t)
	again, _, err := th.File(t.Context(), owner, "holiday.heic", 96)
	if err != nil {
		t.Fatalf("the second ask made it again, and failed: %v", err)
	}
	_ = again.Close()
}

// TestThumbnailOfSomethingFFmpegRefuses: a file with a name this build claims
// and bytes it cannot read is ErrNoThumbnail, which a surface turns into a 404
// rather than a 500.
func TestThumbnailOfSomethingFFmpegRefuses(t *testing.T) {
	t.Parallel()

	th, service, _ := thumbs(t)
	write(t, service, "broken.heic", []byte("not a HEIC at all"), "image/heic")

	if _, _, err := th.File(t.Context(), owner, "broken.heic", 96); !errors.Is(err, ErrNoThumbnail) {
		t.Errorf("File = %v, want ErrNoThumbnail", err)
	}
}

// TestCanThumbnail is the question a page asks before it renders an image, and
// it has to be the same question this package answers when asked for one.
//
// It is a function of the build and of nothing else, which is what #135 decided
// and what lets a page offer a picture without asking anybody -- and what made
// this change of answer free: every HEIC already stored became a file with a
// picture the moment the path below was wired, with no byte moved and nothing
// reindexed.
func TestCanThumbnail(t *testing.T) {
	t.Parallel()

	const ordinary = 4 << 20 // a photograph, or a short clip

	for _, name := range []string{"a.jpg", "b.JPEG", "c.png", "d.heic", "e.HEIF", "f.mp4", "g.mov", "h.mkv", "i.webm", "j.avi"} {
		if !CanThumbnail(name, ordinary) {
			t.Errorf("%s is offered no picture", name)
		}
	}
	// .avif is AV1 and that decoder is deliberately out of the build; camera raw
	// needs the preview inside the file, which is a different technique; and the
	// rest is not a picture at all.
	for _, name := range []string{"k.avif", "l.dng", "m.cr2", "n.txt", "o.mp3", "p"} {
		if CanThumbnail(name, ordinary) {
			t.Errorf("%s is offered a picture this build cannot make", name)
		}
	}

	// And the rule from #145 read from the other end: a film is not copied to
	// be looked at, so the listing must not offer a picture of one. A grid of
	// broken images is what #135 promised would never happen, and the offer is
	// where that promise is kept.
	if CanThumbnail("film.mkv", maxSpool+1) {
		t.Error("a film larger than anything this will copy is offered a picture")
	}
	if CanThumbnail("enormous.jpg", maxThumbSource+1) {
		t.Error("an image larger than anything this will decode is offered a picture")
	}
	if !CanThumbnail("film.mkv", maxSpool) {
		t.Error("a file exactly at the bound is refused; the bound is inclusive")
	}
}

// opaqueBlack reports whether every pixel is black, which is what a frame taken
// from a file that decoded into nothing looks like.
func opaqueBlack(img image.Image) bool {
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if r, g, bl, _ := img.At(x, y).RGBA(); r|g|bl != 0 {
				return false
			}
		}
	}
	return true
}
