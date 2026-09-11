package media

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"path/filepath"
	"testing"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/db/sqlite"
	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/storage"
	"github.com/C0piIot/stratus-backend/internal/storage/disk"
)

// thumbs wires the real backends, like the indexer's fixture: a thumbnail is a
// decode, a resize and an object in a blob store, and a fake store would only
// prove the fake keeps what it is given.
func thumbs(t *testing.T) (*Thumbs, *files.Service, storage.Storage) {
	t.Helper()
	dir := t.TempDir()

	blobs, err := disk.New(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blobs.Close() })

	meta, err := sqlite.New(t.Context(), filepath.Join(dir, "stratus.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = meta.Close() })
	if merr := meta.Migrate(t.Context()); merr != nil {
		t.Fatal(merr)
	}

	service := files.New(blobs, meta)
	return NewThumbs(blobs, service), service, blobs
}

func TestSnapSize(t *testing.T) {
	t.Parallel()

	// Rounded up, because a thumbnail bigger than asked for still displays and
	// a smaller one does not. Anything past the ladder gets the largest: this
	// serves thumbnails, and the original is what WebDAV is for.
	tests := map[int]thumbSize{
		0: thumbSmall, 1: thumbSmall, 96: thumbSmall,
		97: thumbMedium, 300: thumbMedium,
		301: thumbLarge, 1200: thumbLarge, 100_000: thumbLarge,
	}
	for asked, want := range tests {
		if got := snapSize(asked); got != want {
			t.Errorf("snapSize(%d) = %d, want %d", asked, got, want)
		}
	}
}

// TestReduce is the arithmetic, tested where it is rather than through a JPEG
// encoder: the ratio has to survive and nothing may come out zero-sized.
func TestReduce(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		w, h, px     int
		wantW, wantH int
	}{
		{name: "a square", w: 800, h: 800, px: 300, wantW: 300, wantH: 300},
		{name: "wider than tall", w: 1200, h: 600, px: 300, wantW: 300, wantH: 150},
		{name: "taller than wide", w: 600, h: 1200, px: 300, wantW: 150, wantH: 300},
		// Already smaller: served as it is, because enlarging makes it blurrier
		// than the file on disk and no larger in any useful sense.
		{name: "smaller than the box", w: 200, h: 100, px: 300, wantW: 200, wantH: 100},
		// A panorama whose short side rounds to nothing. An image of zero
		// height encodes to a file nothing can open.
		{name: "a panorama", w: 10_000, h: 20, px: 96, wantW: 96, wantH: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := reduce(image.NewRGBA(image.Rect(0, 0, tt.w, tt.h)), tt.px).Bounds()
			if got.Dx() != tt.wantW || got.Dy() != tt.wantH {
				t.Errorf("reduce(%dx%d, %d) = %dx%d, want %dx%d",
					tt.w, tt.h, tt.px, got.Dx(), got.Dy(), tt.wantW, tt.wantH)
			}
		})
	}
}

// TestThumbKeyIsCollectable is the coupling that keeps thumbnails from leaking:
// the key has to be the one files.Collect knows how to read the parent out of.
func TestThumbKeyIsCollectable(t *testing.T) {
	t.Parallel()

	key := thumbKey("blobs/ab/cd/efgh", thumbMedium)
	if want := "derived/blobs/ab/cd/efgh/300.jpg"; key != want {
		t.Fatalf("thumbKey = %q, want %q", key, want)
	}
	// And the port agrees, which is the half a test can check without the
	// sweep: the shape is defined once, in internal/files.
	if want := files.DerivedKey("blobs/ab/cd/efgh", "300.jpg"); key != want {
		t.Errorf("thumbKey = %q but files.DerivedKey says %q", key, want)
	}
}

func TestDecodableInGo(t *testing.T) {
	t.Parallel()

	for _, p := range []string{"a.jpg", "A.JPG", "a.jpeg", "a.png", "dir/b.PNG"} {
		if !decodableInGo(p) {
			t.Errorf("%q should be decodable without ffmpeg", p)
		}
	}
	// HEIC and video wait for the ffmpeg path; camera raw wants the embedded
	// preview, which is a different technique; and a track is not a picture.
	for _, p := range []string{"a.heic", "a.mp4", "a.dng", "a.webp", "a.tiff", "a.flac", "a"} {
		if decodableInGo(p) {
			t.Errorf("%q is claimed as decodable and is not", p)
		}
	}
}

// TestOpenMakesAndKeeps is the whole path: decode, reduce, encode, store, and
// answer the stored bytes the second time.
func TestOpenMakesAndKeeps(t *testing.T) {
	t.Parallel()
	th, service, blobs := thumbs(t)

	f := writeImage(t, service, "photos/one.jpg", 800, 400)

	first := read(t, th, f, 300)
	cfg, format, err := image.DecodeConfig(bytes.NewReader(first))
	if err != nil {
		t.Fatalf("the thumbnail does not decode: %v", err)
	}
	if format != "jpeg" || cfg.Width != 300 || cfg.Height != 150 {
		t.Errorf("the thumbnail is %s %dx%d, want jpeg 300x150", format, cfg.Width, cfg.Height)
	}

	// Kept where the sweep can find it.
	if _, err := blobs.Stat(t.Context(), thumbKey(f.BlobKey, thumbMedium)); err != nil {
		t.Fatalf("the thumbnail was not stored: %v", err)
	}
	// And the second answer is the stored one. Proved by changing it: a read
	// that regenerated would come back as a picture again.
	stored := thumbKey(f.BlobKey, thumbMedium)
	if _, err := blobs.Put(t.Context(), stored, bytes.NewReader([]byte("not a picture")), -1); err != nil {
		t.Fatal(err)
	}
	if got := read(t, th, f, 300); string(got) != "not a picture" {
		t.Error("the second request generated the thumbnail again instead of reading it")
	}
}

// TestOpenSnapsTheSize: a request for 301 pixels must not leave an object
// behind that nothing will ever ask for twice.
func TestOpenSnapsTheSize(t *testing.T) {
	t.Parallel()
	th, service, blobs := thumbs(t)

	f := writeImage(t, service, "photos/one.jpg", 2000, 2000)
	read(t, th, f, 301)

	if _, err := blobs.Stat(t.Context(), thumbKey(f.BlobKey, thumbLarge)); err != nil {
		t.Errorf("301 pixels did not snap to the large size: %v", err)
	}
	for _, size := range []thumbSize{thumbSmall, thumbMedium} {
		if _, err := blobs.Stat(t.Context(), thumbKey(f.BlobKey, size)); !errors.Is(err, storage.ErrNotFound) {
			t.Errorf("a size nobody asked for was written: %d", size)
		}
	}
}

func TestOpenRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()
	th, service, _ := thumbs(t)

	// A format that needs ffmpeg, refused on its name before anything is read.
	heic := write(t, service, "photos/one.heic", []byte("whatever"), "image/heic")
	if _, _, err := th.open(t.Context(), heic, 300); !errors.Is(err, ErrNoThumbnail) {
		t.Errorf("Open on a HEIC = %v, want ErrNoThumbnail", err)
	}

	// And a name that promises a JPEG over bytes that are not one.
	broken := write(t, service, "photos/two.jpg", []byte("this is not a JPEG"), "image/jpeg")
	if _, _, err := th.open(t.Context(), broken, 300); !errors.Is(err, ErrNoThumbnail) {
		t.Errorf("Open on a broken JPEG = %v, want ErrNoThumbnail", err)
	}
}

// TestFolderCover is where a library keeps its artwork, and the order is the
// point: a folder often holds a back cover and a booklet scan too.
func TestFolderCover(t *testing.T) {
	t.Parallel()
	_, service, _ := thumbs(t)

	writeImage(t, service, "music/album/thumb.png", 100, 100)
	writeImage(t, service, "music/album/front.jpg", 100, 100)
	want := writeImage(t, service, "music/album/cover.jpg", 100, 100)
	// A track, so the directory is not only pictures.
	write(t, service, "music/album/01.flac", []byte("audio"), "audio/flac")

	entries, err := service.List(t.Context(), owner, "music/album")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := folderCover(entries)
	if !ok {
		t.Fatal("folderCover found nothing")
	}
	if got.Path != want.Path {
		t.Errorf("folderCover = %q, want the conventional name", got.Path)
	}

}

// TestFolderCoverIgnoresCase, because a folder written by another tool has
// COVER.JPG in it and that is the same cover.
func TestFolderCoverIgnoresCase(t *testing.T) {
	t.Parallel()
	_, service, _ := thumbs(t)

	want := writeImage(t, service, "music/album/COVER.JPG", 100, 100)

	entries, err := service.List(t.Context(), owner, "music/album")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := folderCover(entries)
	if !ok {
		t.Fatal("folderCover found nothing")
	}
	if got.Path != want.Path {
		t.Errorf("folderCover = %q", got.Path)
	}
}

func TestFolderCoverWithNoPicture(t *testing.T) {
	t.Parallel()
	th, service, _ := thumbs(t)

	write(t, service, "music/album/01.flac", []byte("audio"), "audio/flac")
	// A subdirectory is not a cover, however it is named.
	if _, err := service.Mkdir(t.Context(), owner, "music/album/cover.jpg"); err != nil {
		t.Fatal(err)
	}

	entries, err := service.List(t.Context(), owner, "music/album")
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := folderCover(entries); ok {
		t.Errorf("folderCover found %q in a directory with no picture", got.Path)
	}
	// And a directory that is not there has no cover either, which is not a
	// failure worth its own error: a listing of nothing is a listing.
	if _, _, err := th.Cover(t.Context(), owner, "music/nowhere", 96); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Cover of a missing directory = %v, want ErrNotFound", err)
	}
}

func read(t *testing.T, th *Thumbs, f db.File, px int) []byte {
	t.Helper()
	body, size, err := th.open(t.Context(), f, px)
	if err != nil {
		t.Fatalf("Open(%q, %d): %v", f.Path, px, err)
	}
	defer func() { _ = body.Close() }()

	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(got)) != size {
		t.Errorf("Open reported %d bytes and returned %d", size, len(got))
	}
	return got
}

// writeImage stores a real picture of the given size, encoded from its
// extension. Generated rather than committed so the case that asserts the
// dimensions carries them.
func writeImage(t *testing.T, s *files.Service, path string, w, h int) db.File {
	t.Helper()

	var body bytes.Buffer
	var err error
	mime := "image/jpeg"
	if filepath.Ext(path) == ".png" {
		mime = "image/png"
		err = png.Encode(&body, gradientImage(w, h))
	} else {
		err = jpeg.Encode(&body, gradientImage(w, h), nil)
	}
	if err != nil {
		t.Fatalf("encoding %q: %v", path, err)
	}
	return write(t, s, path, body.Bytes(), mime)
}

// gradientImage rather than one colour: a flat image compresses to almost
// nothing and would hide a resize doing nothing at all.
func gradientImage(w, h int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 128, A: 255})
		}
	}
	return img
}

// TestCoverFindsTheFolderPicture is the common case and the cheap one: a
// listing this request already did answers it.
func TestCoverFindsTheFolderPicture(t *testing.T) {
	t.Parallel()
	th, service, blobs := thumbs(t)

	write(t, service, "music/album/01.flac", []byte("not really audio"), "audio/flac")
	beside := writeImage(t, service, "music/album/cover.jpg", 500, 500)

	body, _, err := th.Cover(t.Context(), owner, "music/album", 96)
	if err != nil {
		t.Fatalf("Cover: %v", err)
	}
	_ = body.Close()

	if _, err := blobs.Stat(t.Context(), thumbKey(beside.BlobKey, thumbSmall)); err != nil {
		t.Errorf("the folder picture was not the one used: %v", err)
	}
}

// TestCoverFindsThePictureInsideTheTrack is the other place it lives, and for a
// library bought as downloads it is the only one.
func TestCoverFindsThePictureInsideTheTrack(t *testing.T) {
	t.Parallel()
	th, service, blobs := thumbs(t)

	track := write(t, service, "music/album/01.flac", flacWithCover(t, 400, 400), "audio/flac")

	got := coverBytes(t, th, "music/album", 96)
	cfg, format, err := image.DecodeConfig(bytes.NewReader(got))
	if err != nil {
		t.Fatalf("the cover does not decode: %v", err)
	}
	if format != "jpeg" || cfg.Width != 96 {
		t.Errorf("the embedded cover came back %s %dx%d", format, cfg.Width, cfg.Height)
	}

	// Filed under the track's blob, because the picture has no blob of its own:
	// that is what lets the sweep take it when the track goes.
	if _, err := blobs.Stat(t.Context(), coverKey(track.BlobKey, thumbSmall)); err != nil {
		t.Errorf("the embedded cover is not filed under the track: %v", err)
	}

	// And the second ask reads it rather than parsing the tag again.
	if _, err := blobs.Put(t.Context(), coverKey(track.BlobKey, thumbSmall),
		bytes.NewReader([]byte("not a picture")), -1); err != nil {
		t.Fatal(err)
	}
	if again := coverBytes(t, th, "music/album", 96); string(again) != "not a picture" {
		t.Error("the second request parsed the tag again instead of reading what was kept")
	}
}

// TestCoverPrefersTheFolderPicture: with both there, the file beside the music
// wins. Finding it is a listing this request already did; the other means
// parsing a tag.
func TestCoverPrefersTheFolderPicture(t *testing.T) {
	t.Parallel()
	th, service, blobs := thumbs(t)

	track := write(t, service, "music/album/01.flac", flacWithCover(t, 400, 400), "audio/flac")
	beside := writeImage(t, service, "music/album/folder.jpg", 300, 300)

	body, _, err := th.Cover(t.Context(), owner, "music/album", 96)
	if err != nil {
		t.Fatalf("Cover: %v", err)
	}
	_ = body.Close()

	if _, err := blobs.Stat(t.Context(), thumbKey(beside.BlobKey, thumbSmall)); err != nil {
		t.Errorf("the folder picture was not used: %v", err)
	}
	if _, err := blobs.Stat(t.Context(), coverKey(track.BlobKey, thumbSmall)); err == nil {
		t.Error("the tag was parsed even though a picture was sitting beside the music")
	}
}

func TestCoverOfAFolderWithNoPictureAnywhere(t *testing.T) {
	t.Parallel()
	th, service, _ := thumbs(t)

	// A track with no tag, and a file that is not audio at all.
	write(t, service, "music/album/01.flac", []byte("fLaC"), "audio/flac")
	write(t, service, "music/album/notes.txt", []byte("liner notes"), "text/plain")

	if _, _, err := th.Cover(t.Context(), owner, "music/album", 96); !errors.Is(err, ErrNoEmbeddedCover) {
		t.Errorf("Cover = %v, want ErrNoEmbeddedCover", err)
	}
}

func TestCoverOfAFolderWithNothingPlayable(t *testing.T) {
	t.Parallel()
	th, service, _ := thumbs(t)

	write(t, service, "music/album/notes.txt", []byte("liner notes"), "text/plain")

	// Nothing to look inside, which is a folder with no cover rather than a
	// tag that could not be read.
	if _, _, err := th.Cover(t.Context(), owner, "music/album", 96); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Cover = %v, want ErrNotFound", err)
	}
}

// TestFirstTrack is the rule that keeps a folder of twelve tracks from costing
// twelve ranged reads on every request that finds no art.
func TestFirstTrack(t *testing.T) {
	t.Parallel()
	_, service, _ := thumbs(t)

	write(t, service, "music/album/03 third.flac", []byte("x"), "audio/flac")
	write(t, service, "music/album/01 first.mp3", []byte("x"), "audio/mpeg")
	write(t, service, "music/album/02 second.m4a", []byte("x"), "audio/mp4")
	write(t, service, "music/album/notes.txt", []byte("x"), "text/plain")
	writeImage(t, service, "music/album/scan.jpg", 10, 10)
	if _, err := service.Mkdir(t.Context(), owner, "music/album/extras"); err != nil {
		t.Fatal(err)
	}

	entries, err := service.List(t.Context(), owner, "music/album")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := firstTrack(entries)
	if !ok {
		t.Fatal("firstTrack found nothing in a folder of tracks")
	}
	// Path order, and nothing that is not audio.
	if got.Path != "music/album/01 first.mp3" {
		t.Errorf("firstTrack = %q", got.Path)
	}

	// A folder with no audio in it has no first track, which is what makes the
	// answer "no cover" rather than an error.
	entries, err = service.List(t.Context(), owner, "music")
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := firstTrack(entries); ok {
		t.Errorf("firstTrack = %q in a folder of directories", got.Path)
	}
}

func coverBytes(t *testing.T, th *Thumbs, dir string, px int) []byte {
	t.Helper()
	body, size, err := th.Cover(t.Context(), owner, dir, px)
	if err != nil {
		t.Fatalf("Cover(%q): %v", dir, err)
	}
	defer func() { _ = body.Close() }()

	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(got)) != size {
		t.Errorf("Cover reported %d bytes and returned %d", size, len(got))
	}
	return got
}

// flacWithCover builds a FLAC whose PICTURE block holds a real JPEG, which is
// the shape a tagger writes.
func flacWithCover(t *testing.T, w, h int) []byte {
	t.Helper()

	var picture bytes.Buffer
	if err := jpeg.Encode(&picture, gradientImage(w, h), nil); err != nil {
		t.Fatal(err)
	}
	return flacFile(
		flacBlock(0, make([]byte, 34), false),
		flacBlock(6, flacPicked(frontCover, picture.String()), true),
	)
}
