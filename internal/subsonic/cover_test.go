package subsonic_test

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/url"
	"strconv"
	"testing"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/files"
)

// TestGetCoverArt follows the ids a client is given, the way the browse tests
// do: the album says which cover to ask for, and asking for it returns a
// picture. A client never builds a cover id of its own.
func TestGetCoverArt(t *testing.T) {
	t.Parallel()
	l := newLibrary(t)
	track := l.add(t, "music/Homogenic/01 Hunter.flac", song("Björk", "Homogenic", "Hunter", 1))
	l.addImage(t, "music/Homogenic/cover.jpg", 800, 800)

	env := response(t, get(t, l, "getAlbumList2", query("f", "json", "type", "alphabeticalByName")))
	list, _ := env["albumList2"].(map[string]any)
	album := only(t, list["album"])
	art, ok := album["coverArt"].(string)
	if !ok {
		t.Fatalf("the album carries no coverArt id: %v", album)
	}

	rec := get(t, l, "getCoverArt", query("id", art, "size", "300"))
	if rec.Code != 200 {
		t.Fatalf("getCoverArt = %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "image/jpeg" {
		t.Fatalf("Content-Type = %q, want image/jpeg whatever the cover was stored as", got)
	}
	if got := rec.Header().Get("Content-Length"); got != strconv.Itoa(rec.Body.Len()) {
		t.Errorf("Content-Length = %q for a body of %d bytes", got, rec.Body.Len())
	}

	// It decodes, and it is the size that was asked for rather than the size it
	// was stored at.
	cfg, format, err := image.DecodeConfig(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("the answer is not an image: %v", err)
	}
	if format != "jpeg" {
		t.Errorf("format = %s", format)
	}
	if cfg.Width != 300 || cfg.Height != 300 {
		t.Errorf("the cover came back %dx%d, want 300x300", cfg.Width, cfg.Height)
	}

	// A track carries its album's cover id rather than its own, because a
	// folder holds one picture for the record: a client asking per row would be
	// the same lookup done once per track.
	if got := coverIDOfFirstSong(t, l, art); got != art {
		t.Errorf("a track's coverArt = %q, want its album's %q", got, art)
	}
	// It is still answered for a track's own id, because the older clients ask
	// that way, and it is the same picture.
	fromSong := get(t, l, "getCoverArt", query("id", songIDOf(track.ID), "size", "300"))
	if fromSong.Code != 200 {
		t.Fatalf("getCoverArt for a song id = %d", fromSong.Code)
	}
	if !bytes.Equal(fromSong.Body.Bytes(), rec.Body.Bytes()) {
		t.Error("a track's cover is not its album's")
	}
}

// TestCoverArtIsKeptAsADerivedBlob is the point of the machinery underneath: the
// second request is a read and not a decode, and what it reads is collectable
// with the file it was made from.
func TestCoverArtIsKeptAsADerivedBlob(t *testing.T) {
	t.Parallel()
	l := newLibrary(t)
	l.add(t, "music/Homogenic/01 Hunter.flac", song("Björk", "Homogenic", "Hunter", 1))
	cover := l.addImage(t, "music/Homogenic/cover.jpg", 800, 800)

	derived := files.DerivedKey(cover.BlobKey, "300.jpg")
	if _, err := l.blobs.Stat(t.Context(), derived); err == nil {
		t.Fatal("the thumbnail exists before anything asked for it, so this test is testing nothing")
	}

	first := get(t, l, "getCoverArt", query("id", albumIDOf("Björk", "Homogenic"), "size", "300"))
	if first.Code != 200 {
		t.Fatalf("getCoverArt = %d", first.Code)
	}

	// Under the derived prefix, filed by the blob it was made from, which is
	// what lets one sweep collect both.
	info, err := l.blobs.Stat(t.Context(), derived)
	if err != nil {
		t.Fatalf("the thumbnail was not kept: %v", err)
	}
	if info.Size != int64(first.Body.Len()) {
		t.Errorf("the stored thumbnail is %d bytes and the answer was %d", info.Size, first.Body.Len())
	}

	// And the second answer is the same bytes.
	second := get(t, l, "getCoverArt", query("id", albumIDOf("Björk", "Homogenic"), "size", "300"))
	if !bytes.Equal(second.Body.Bytes(), first.Body.Bytes()) {
		t.Error("the second request answered a different picture")
	}
}

// TestCoverArtSizesSnapToALadder is why the size is not whatever a client
// typed: the size is part of the key, so an arbitrary one means an unbounded
// set of derived objects that nothing will ever ask for twice.
func TestCoverArtSizesSnapToALadder(t *testing.T) {
	t.Parallel()
	l := newLibrary(t)
	l.add(t, "music/Homogenic/01 Hunter.flac", song("Björk", "Homogenic", "Hunter", 1))
	l.addImage(t, "music/Homogenic/cover.jpg", 2000, 2000)

	id := albumIDOf("Björk", "Homogenic")
	tests := map[string]int{
		// Rounded up, because a thumbnail bigger than asked for still displays
		// and a smaller one does not.
		"50":   96,
		"96":   96,
		"97":   300,
		"300":  300,
		"9999": 1200,
	}
	for asked, want := range tests {
		rec := get(t, l, "getCoverArt", query("id", id, "size", asked))
		cfg, _, err := image.DecodeConfig(bytes.NewReader(rec.Body.Bytes()))
		if err != nil {
			t.Fatalf("size=%s: %v", asked, err)
		}
		if cfg.Width != want {
			t.Errorf("size=%s came back %d wide, want %d", asked, cfg.Width, want)
		}
	}

	// No size at all is the largest of the ladder rather than the original: a
	// 2000-pixel scan is not what a client asking for cover art wants.
	rec := get(t, l, "getCoverArt", query("id", id))
	cfg, _, err := image.DecodeConfig(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Width != 1200 {
		t.Errorf("with no size the cover came back %d wide, want the largest of the ladder", cfg.Width)
	}
}

// TestCoverArtSmallerThanAskedForIsNotEnlarged: a 200-pixel cover asked for at
// 300 comes back at 200, because blowing it up makes it blurrier than the file
// on disk and no larger in any useful sense.
func TestCoverArtSmallerThanAskedForIsNotEnlarged(t *testing.T) {
	t.Parallel()
	l := newLibrary(t)
	l.add(t, "music/Homogenic/01 Hunter.flac", song("Björk", "Homogenic", "Hunter", 1))
	l.addImage(t, "music/Homogenic/cover.jpg", 200, 200)

	rec := get(t, l, "getCoverArt", query("id", albumIDOf("Björk", "Homogenic"), "size", "300"))
	cfg, _, err := image.DecodeConfig(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Width != 200 {
		t.Errorf("a 200 pixel cover came back %d wide", cfg.Width)
	}
}

// TestCoverArtKeepsTheAspectRatio, because a square thumbnail of a rectangular
// scan is a stretched picture and a client cannot undo it.
func TestCoverArtKeepsTheAspectRatio(t *testing.T) {
	t.Parallel()
	l := newLibrary(t)
	l.add(t, "music/Wide/01.flac", song("Wide", "Wide", "One", 1))
	l.addImage(t, "music/Wide/cover.jpg", 1200, 600)

	rec := get(t, l, "getCoverArt", query("id", albumIDOf("Wide", "Wide"), "size", "300"))
	cfg, _, err := image.DecodeConfig(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Width != 300 || cfg.Height != 150 {
		t.Errorf("a 2:1 cover came back %dx%d, want 300x150", cfg.Width, cfg.Height)
	}
}

// TestCoverArtPrefersTheConventionalName is why the names are a list in an
// order: a folder often holds a back cover and a scan of the booklet too, and
// picking one of those is worse than picking none.
func TestCoverArtPrefersTheConventionalName(t *testing.T) {
	t.Parallel()
	l := newLibrary(t)
	l.add(t, "music/Homogenic/01 Hunter.flac", song("Björk", "Homogenic", "Hunter", 1))
	// Written in the losing order, so directory order cannot be what decides.
	back := l.addImage(t, "music/Homogenic/thumb.png", 400, 400)
	front := l.addImage(t, "music/Homogenic/cover.jpg", 800, 800)

	rec := get(t, l, "getCoverArt", query("id", albumIDOf("Björk", "Homogenic"), "size", "300"))
	if rec.Code != 200 {
		t.Fatalf("getCoverArt = %d", rec.Code)
	}
	if _, err := l.blobs.Stat(t.Context(), files.DerivedKey(front.BlobKey, "300.jpg")); err != nil {
		t.Errorf("cover.jpg is not what was used: %v", err)
	}
	if _, err := l.blobs.Stat(t.Context(), files.DerivedKey(back.BlobKey, "300.jpg")); err == nil {
		t.Error("thumb.png was used even though cover.jpg is there")
	}
}

// TestCoverArtFromAPNG, because a cover is as often a PNG as a JPEG and the
// answer is a JPEG either way: one output format is one path through every
// client.
func TestCoverArtFromAPNG(t *testing.T) {
	t.Parallel()
	l := newLibrary(t)
	l.add(t, "music/Homogenic/01 Hunter.flac", song("Björk", "Homogenic", "Hunter", 1))
	l.addImage(t, "music/Homogenic/folder.png", 500, 500)

	rec := get(t, l, "getCoverArt", query("id", albumIDOf("Björk", "Homogenic"), "size", "96"))
	if rec.Code != 200 {
		t.Fatalf("getCoverArt = %d", rec.Code)
	}
	_, format, err := image.DecodeConfig(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if format != "jpeg" {
		t.Errorf("a PNG cover came back as %s", format)
	}
}

// TestCoverArtFromInsideTheTrack is the other place artwork lives, and for a
// library bought as downloads it is the only place: no picture in the folder,
// one inside the file.
func TestCoverArtFromInsideTheTrack(t *testing.T) {
	t.Parallel()
	l := newLibrary(t)
	l.addTrackWithCover(t, "music/Vespertine/01 Hidden Place.flac",
		song("Björk", "Vespertine", "Hidden Place", 1), 600, 600)

	rec := get(t, l, "getCoverArt", query("id", albumIDOf("Björk", "Vespertine"), "size", "300"))
	if rec.Code != 200 {
		t.Fatalf("getCoverArt = %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "image/jpeg" {
		t.Fatalf("Content-Type = %q", got)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("the answer is not an image: %v", err)
	}
	if cfg.Width != 300 || cfg.Height != 300 {
		t.Errorf("the embedded cover came back %dx%d, want 300x300", cfg.Width, cfg.Height)
	}
}

// TestCoverArtPrefersTheFolderPicture: when both are there the file beside the
// music wins, because finding it is a listing this request already did and
// reading the other means parsing a tag.
func TestCoverArtPrefersTheFolderPicture(t *testing.T) {
	t.Parallel()
	l := newLibrary(t)
	track := l.addTrackWithCover(t, "music/Vespertine/01 Hidden Place.flac",
		song("Björk", "Vespertine", "Hidden Place", 1), 600, 600)
	beside := l.addImage(t, "music/Vespertine/cover.jpg", 400, 400)

	rec := get(t, l, "getCoverArt", query("id", albumIDOf("Björk", "Vespertine"), "size", "96"))
	if rec.Code != 200 {
		t.Fatalf("getCoverArt = %d", rec.Code)
	}
	// Which one was used is visible in the store: the derived object is filed
	// under the blob it was made from.
	if _, err := l.blobs.Stat(t.Context(), files.DerivedKey(beside.BlobKey, "96.jpg")); err != nil {
		t.Errorf("the folder picture was not the one used: %v", err)
	}
	if _, err := l.blobs.Stat(t.Context(), files.DerivedKey(track.BlobKey, "cover-96.jpg")); err == nil {
		t.Error("the tag was parsed even though a picture was sitting beside the music")
	}
}

// TestEmbeddedCoverIsFiledUnderTheTrack is what keeps it collectable: the
// picture has no blob of its own, so the derived object hangs off the track's --
// delete the track and its cover goes with it.
func TestEmbeddedCoverIsFiledUnderTheTrack(t *testing.T) {
	t.Parallel()
	l := newLibrary(t)
	track := l.addTrackWithCover(t, "music/Vespertine/01 Hidden Place.flac",
		song("Björk", "Vespertine", "Hidden Place", 1), 200, 200)

	if code := get(t, l, "getCoverArt", query("id", albumIDOf("Björk", "Vespertine"), "size", "96")).Code; code != 200 {
		t.Fatalf("getCoverArt = %d", code)
	}
	if _, err := l.blobs.Stat(t.Context(), files.DerivedKey(track.BlobKey, "cover-96.jpg")); err != nil {
		t.Errorf("the embedded cover is not filed under the track: %v", err)
	}
}

// TestCoverArtOfAFolder is the folder-browsing half: a client that walked into
// a directory asks for that directory's picture.
func TestCoverArtOfAFolder(t *testing.T) {
	t.Parallel()
	l := newLibrary(t)
	l.add(t, "music/Homogenic/01 Hunter.flac", song("Björk", "Homogenic", "Hunter", 1))
	l.addImage(t, "music/Homogenic/cover.jpg", 400, 400)

	rec := get(t, l, "getCoverArt", query("id", dirIDOf("music/Homogenic"), "size", "96"))
	if rec.Code != 200 {
		t.Fatalf("getCoverArt = %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "image/jpeg" {
		t.Errorf("Content-Type = %q", got)
	}
}

// TestNoCoverArt is the normal state of half a library, so it is answered as
// "there is none" rather than as a failure -- and in XML, because this endpoint
// answers bytes.
func TestNoCoverArt(t *testing.T) {
	t.Parallel()
	l := newLibrary(t)
	l.add(t, "music/Bare/01.flac", song("Bare", "Bare", "One", 1))

	tests := []struct {
		name string
		id   string
		want float64
	}{
		{name: "an album with no picture beside it", id: albumIDOf("Bare", "Bare"), want: 70},
		{name: "an album that does not exist", id: albumIDOf("Nobody", "Nothing"), want: 70},
		{name: "an id of no recognised kind", id: "xx-nonsense", want: 70},
		{name: "an album id that is not base64", id: "al-!!!!", want: 70},
		{name: "an artist, which has no one cover to stand for it", id: artistIDOf("Bare"), want: 70},
		{name: "a directory that is not there", id: dirIDOf("nowhere"), want: 70},
		{name: "a song nothing has", id: songIDOf(99999), want: 70},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			q := url.Values{"c": {"tests"}, "u": {username}, "p": {password}, "id": {tt.id}, "f": {"json"}}
			assertXMLError(t, get(t, l, "getCoverArt", q.Encode()), tt.want)
		})
	}

	// And no id at all is the missing-parameter code, as everywhere else.
	q := url.Values{"c": {"tests"}, "u": {username}, "p": {password}}
	assertXMLError(t, get(t, l, "getCoverArt", q.Encode()), 10)
}

// TestCoverArtOfSomethingUndecodable: a cover.jpg that is not a JPEG at all is
// not a client's problem to distinguish from there being none, but it must not
// be a crash or a hang either.
func TestCoverArtOfSomethingUndecodable(t *testing.T) {
	t.Parallel()
	l := newLibrary(t)
	l.add(t, "music/Broken/01.flac", song("Broken", "Broken", "One", 1))
	l.write(t, "music/Broken/cover.jpg", "this is not a JPEG")

	rec := get(t, l, "getCoverArt", query("id", albumIDOf("Broken", "Broken")))
	// The generic code rather than 70: the file is there and we cannot read it,
	// which is worth telling apart in a log even though a client draws the same
	// placeholder either way.
	assertXMLError(t, rec, 0)
}

// TestCoverArtSurvivesAClientHangingUp: the picture is already in hand when
// the write fails, so there is nothing to answer with and nothing to do but
// log it -- and above all not panic.
func TestCoverArtSurvivesAClientHangingUp(t *testing.T) {
	t.Parallel()
	l := newLibrary(t)
	l.add(t, "music/Homogenic/01 Hunter.flac", song("Björk", "Homogenic", "Hunter", 1))
	l.addImage(t, "music/Homogenic/cover.jpg", 200, 200)

	req := request(t, "getCoverArt", query("id", albumIDOf("Björk", "Homogenic")))
	l.ServeHTTP(&brokenWriter{ok: 0}, req)
}

// coverIDOfFirstSong reads an album's first track and returns the cover id it
// carries, which is the album's own -- a folder holds one picture for the
// record, so a track asking for its own would be the same lookup per row.
func coverIDOfFirstSong(t *testing.T, l *library, albumID string) string {
	t.Helper()
	env := response(t, get(t, l, "getAlbum", query("f", "json", "id", albumID)))
	full, _ := env["album"].(map[string]any)
	songs, _ := full["song"].([]any)
	if len(songs) == 0 {
		t.Fatalf("the album has no tracks: %v", full)
	}
	one, _ := songs[0].(map[string]any)
	return str(t, one["coverArt"])
}

// addTrackWithCover writes a FLAC carrying a picture in a PICTURE block, which
// is the shape a tagger writes. Built here rather than committed so the case
// that asserts the dimensions carries them.
func (l *library) addTrackWithCover(t *testing.T, p string, m db.Media, w, h int) db.File {
	t.Helper()

	picture := jpegBytes(t, w, h)

	// The block: a picture type, a MIME type and a description with their
	// lengths, four dimensions nothing reads back, and the bytes.
	var block bytes.Buffer
	be := func(n uint32) { _ = binary.Write(&block, binary.BigEndian, n) }
	be(3) // front cover
	be(uint32(len("image/jpeg")))
	block.WriteString("image/jpeg")
	be(0) // no description
	be(uint32(w))
	be(uint32(h))
	be(24)
	be(0)
	be(uint32(len(picture)))
	block.Write(picture)

	// The file: the magic, a STREAMINFO nothing reads, and the picture block
	// last.
	var file bytes.Buffer
	file.WriteString("fLaC")
	file.Write(append([]byte{0, 0, 0, 34}, make([]byte, 34)...))
	n := block.Len()
	file.Write([]byte{0x80 | 6, byte(n >> 16), byte(n >> 8), byte(n)})
	file.Write(block.Bytes())

	return l.index(t, l.write(t, p, file.String()), m)
}

// addImage writes a real picture of the given size, encoded from its extension.
// A committed fixture would do, but generating one keeps the dimensions in the
// case that asserts them.
func (l *library) addImage(t *testing.T, p string, w, h int) db.File {
	t.Helper()

	var body bytes.Buffer
	var err error
	if len(p) > 4 && p[len(p)-4:] == ".png" {
		err = png.Encode(&body, gradient(w, h))
	} else {
		err = jpeg.Encode(&body, gradient(w, h), nil)
	}
	if err != nil {
		t.Fatalf("encoding %q: %v", p, err)
	}
	return l.write(t, p, body.String())
}

func jpegBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := jpeg.Encode(&out, gradient(w, h), nil); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// gradient rather than one colour: a flat image compresses to almost nothing
// and would hide a resize doing nothing at all.
func gradient(w, h int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 128, A: 255})
		}
	}
	return img
}
