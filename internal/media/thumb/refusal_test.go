package thumb

import (
	"bytes"
	"errors"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"

	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/storage"
)

// What this file is about: a thumbnail that cannot be made is not made again
// (#338). It is retry_test.go's distinction on the other half of the package,
// where the work is paid per request rather than per pass -- a file nothing can
// decode used to cost a media.Spool and an ffmpeg run on every page view, in a queue
// whose places the pictures beside it were waiting for.

// TestARefusedThumbnailIsNotAttemptedAgain, proved the way the stored
// thumbnail is: by changing the bytes underneath. A second request that read
// anything at all would come back with a picture.
func TestARefusedThumbnailIsNotAttemptedAgain(t *testing.T) {
	t.Parallel()
	th, service, blobs := thumbs(t)

	f := write(t, service, "photos/broken.jpg", []byte("\xff\xd8\xff not a photograph"), "image/jpeg")
	if _, _, err := th.open(t.Context(), f, 96); !errors.Is(err, ErrNoThumbnail) {
		t.Fatalf("open = %v, want ErrNoThumbnail", err)
	}
	if _, err := blobs.Stat(t.Context(), verdictKey(f.BlobKey)); err != nil {
		t.Fatalf("the refusal was not written down: %v", err)
	}

	var picture bytes.Buffer
	if err := jpeg.Encode(&picture, gradientImage(40, 40), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := blobs.Put(t.Context(), f.BlobKey, bytes.NewReader(picture.Bytes()), -1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := th.open(t.Context(), f, 96); !errors.Is(err, ErrNoThumbnail) {
		t.Errorf("the second request decoded the file again: %v", err)
	}
}

// TestARefusalIsCollectable is the same coupling TestThumbKeyIsCollectable
// holds for a picture, plus the half that is this file's own: the note is a
// cache, so the sweep takes it a week after it was written whatever its parent
// is doing, and a build that learns the format is not refusing it for ever.
func TestARefusalIsCollectable(t *testing.T) {
	t.Parallel()

	key := verdictKey("blobs/ab/cd/efgh")
	if want := "derived/blobs/ab/cd/efgh/g1-cache-none.txt"; key != want {
		t.Fatalf("verdictKey = %q, want %q", key, want)
	}
	if want := files.DerivedKey("blobs/ab/cd/efgh", files.CachePrefix+"none.txt"); key != want {
		t.Errorf("verdictKey = %q but files.DerivedKey says %q", key, want)
	}
	if want := "derived/blobs/ab/cd/efgh/g1-cache-cover-none.txt"; coverVerdictKey("blobs/ab/cd/efgh") != want {
		t.Errorf("coverVerdictKey = %q, want %q", coverVerdictKey("blobs/ab/cd/efgh"), want)
	}
}

// TestABlobThatIsNotThereIsNotARefusal: the bytes were never read, so there is
// nothing to judge -- and a server pointed at the wrong bucket would otherwise
// write a verdict for every file in the library.
func TestABlobThatIsNotThereIsNotARefusal(t *testing.T) {
	t.Parallel()
	th, service, blobs := thumbs(t)

	f := writeImage(t, service, "photos/one.jpg", 400, 400)
	if err := blobs.Delete(t.Context(), f.BlobKey); err != nil {
		t.Fatal(err)
	}

	if _, _, err := th.open(t.Context(), f, 96); err == nil {
		t.Fatal("open of a file whose blob is gone succeeded")
	}
	if _, err := blobs.Stat(t.Context(), verdictKey(f.BlobKey)); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("a missing blob was written down as a verdict: %v", err)
	}
}

// TestAKilledFFmpegIsNotARefusal is TestAKilledProbeIsNotAVerdict's twin, and
// the reason runFFmpeg had to learn the difference: the out-of-memory killer is
// what answers a 4K recording on the smallest instance, and it says nothing
// about the recording.
func TestAKilledFFmpegIsNotARefusal(t *testing.T) {
	t.Parallel()
	_, service, blobs, meta := thumbsOver(t)

	killed := filepath.Join(t.TempDir(), "ffmpeg")
	if err := os.WriteFile(killed, []byte("#!/bin/sh\nkill -9 $$\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	th := New(blobs, service, meta, killed, t.TempDir())

	f := write(t, service, "photos/one.heic", []byte("whatever"), "image/heic")
	if _, _, err := th.open(t.Context(), f, 96); !errors.Is(err, ErrNoThumbnail) {
		t.Fatalf("open = %v, want ErrNoThumbnail", err)
	}
	if _, err := blobs.Stat(t.Context(), verdictKey(f.BlobKey)); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("a killed ffmpeg was written down as a verdict on the file: %v", err)
	}
}

// TestATrackWithNoPictureIsRememberedAsSuch is why the note carries a sentinel
// of its own: an album with no picture inside its tracks is the normal state of
// half a music library and getCoverArt answers it with a 404, while a picture
// that will not decode is a 500 somebody should see in a log. Flattening the
// two would have turned every such album into an error on the second request.
func TestATrackWithNoPictureIsRememberedAsSuch(t *testing.T) {
	t.Parallel()
	th, service, blobs := thumbs(t)

	track := write(t, service, "music/album/01.flac",
		flacFile(flacBlock(0, make([]byte, 34), true)), "audio/flac")

	for _, when := range []string{"first", "second"} {
		if _, _, err := th.Cover(t.Context(), owner, "music/album", 96); !errors.Is(err, ErrNoEmbeddedCover) {
			t.Errorf("the %s ask = %v, want ErrNoEmbeddedCover", when, err)
		}
	}
	if _, err := blobs.Stat(t.Context(), coverVerdictKey(track.BlobKey)); err != nil {
		t.Errorf("the track was read again rather than remembered: %v", err)
	}
}
