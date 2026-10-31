package thumb

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"testing"
	"time"

	"github.com/stratus-private-cloud/stratus-backend/internal/db"
	"github.com/stratus-private-cloud/stratus-backend/internal/db/dbtest"
	"github.com/stratus-private-cloud/stratus-backend/internal/files"
	"github.com/stratus-private-cloud/stratus-backend/internal/storage/storagetest"
)

// What this file is about: the third place an album's picture can come from
// (#217). A cover the pass fetched is filed under one of the album's tracks
// and served when the folder has none, ahead of a picture inside the track,
// which is as often a video's frame as the record's cover.

// pngBytes is a picture the size asked for, as the archive would hand one
// over.
func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := range w {
		img.Set(x, 0, color.RGBA{R: 200, A: 255})
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// fetchedFor files a cover for the album the track is in and writes the row
// that says so, which is what the pass does after the archive answered.
func fetchedFor(t *testing.T, th *Service, meta db.Store, track db.File, m db.Media) {
	t.Helper()
	m.FileID, m.Kind, m.IndexedAt, m.Version = track.ID, db.KindAudio, time.Now(), 1
	if err := meta.PutMedia(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	if err := th.StoreArt(t.Context(), track, pngBytes(t, 500, 500)); err != nil {
		t.Fatalf("StoreArt: %v", err)
	}
	if err := meta.PutAlbumLookup(t.Context(), db.AlbumLookup{
		Owner: owner, Artist: m.AlbumArtist, Album: m.Album, LookedUpAt: time.Now(),
		ReleaseGroup: "4cf8ba42-5d0b-3a4d-8e8f-4c8f2f3d1a1b", CoverBlob: track.BlobKey,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCoverFallsBackToTheFetchedPicture(t *testing.T) {
	t.Parallel()
	th, service, blobs, meta := thumbsOver(t)
	th.WithLookups(meta)

	track := write(t, service, "music/amber/01.flac", []byte("fLaC"), "audio/flac")
	fetchedFor(t, th, meta, track, db.Media{AlbumArtist: "Autechre", Artist: "Autechre", Album: "Amber", Title: "Foil"})

	got := coverBytes(t, th, "music/amber", 96)
	cfg, format, err := image.DecodeConfig(bytes.NewReader(got))
	if err != nil {
		t.Fatalf("the cover does not decode: %v", err)
	}
	if format != "jpeg" || cfg.Width != 96 {
		t.Errorf("the fetched cover came back %s %dx%d, want a 96 pixel JPEG", format, cfg.Width, cfg.Height)
	}
	// Under the track, like the tag's picture, so the sweep collects it with
	// the track; and in a slot of its own, so the two cannot be confused.
	if _, err := blobs.Stat(t.Context(), artSizeKey(track.BlobKey, thumbSmall)); err != nil {
		t.Errorf("the size made from the fetched cover is not filed under the track: %v", err)
	}
	if _, err := blobs.Stat(t.Context(), coverKey(track.BlobKey, thumbSmall)); err == nil {
		t.Error("the fetched cover was filed where the tag's picture goes")
	}
}

// TestCoverPrefersTheFetchedPictureToTheTag: a picture inside a track is as
// often a video's frame as a record's cover, so the album's own front cover
// is what an album shows when the pass has found one.
func TestCoverPrefersTheFetchedPictureToTheTag(t *testing.T) {
	t.Parallel()
	th, service, blobs, meta := thumbsOver(t)
	th.WithLookups(meta)

	track := write(t, service, "music/amber/01.flac", flacWithCover(t, 400, 400), "audio/flac")
	fetchedFor(t, th, meta, track, db.Media{AlbumArtist: "Autechre", Artist: "Autechre", Album: "Amber", Title: "Foil"})

	coverBytes(t, th, "music/amber", 96)
	if _, err := blobs.Stat(t.Context(), artSizeKey(track.BlobKey, thumbSmall)); err != nil {
		t.Errorf("the fetched cover was not used: %v", err)
	}
	if _, err := blobs.Stat(t.Context(), coverKey(track.BlobKey, thumbSmall)); err == nil {
		t.Error("the tag's picture was used although a cover had been fetched")
	}
}

// TestCoverFallsBackToTheTag: an album the pass found nothing for shows the
// picture inside its track, which is better than an empty square.
func TestCoverFallsBackToTheTag(t *testing.T) {
	t.Parallel()
	th, service, blobs, meta := thumbsOver(t)
	th.WithLookups(meta)

	track := write(t, service, "music/amber/01.flac", flacWithCover(t, 400, 400), "audio/flac")
	coverBytes(t, th, "music/amber", 96)
	if _, err := blobs.Stat(t.Context(), coverKey(track.BlobKey, thumbSmall)); err != nil {
		t.Errorf("the tag's picture was not used: %v", err)
	}

	// And when the fetched one went with a track that was replaced, the tag
	// is still there to answer.
	fetchedFor(t, th, meta, track, db.Media{AlbumArtist: "Autechre", Artist: "Autechre", Album: "Amber", Title: "Foil"})
	if err := blobs.Delete(t.Context(), artKey(track.BlobKey)); err != nil {
		t.Fatal(err)
	}
	coverBytes(t, th, "music/amber", 300)
	if _, err := blobs.Stat(t.Context(), coverKey(track.BlobKey, thumbMedium)); err != nil {
		t.Errorf("the tag did not answer for a fetched picture that is gone: %v", err)
	}
}

// TestALostFetchedPictureIsForgotten: the track it was filed under was
// replaced and the sweep took the picture with it. The answer is the one the
// tag gave, and the row is told so the pass fetches it again.
func TestALostFetchedPictureIsForgotten(t *testing.T) {
	t.Parallel()
	th, service, blobs, meta := thumbsOver(t)
	th.WithLookups(meta)

	track := write(t, service, "music/amber/01.flac", []byte("fLaC"), "audio/flac")
	fetchedFor(t, th, meta, track, db.Media{AlbumArtist: "Autechre", Artist: "Autechre", Album: "Amber", Title: "Foil"})
	if err := blobs.Delete(t.Context(), artKey(track.BlobKey)); err != nil {
		t.Fatal(err)
	}

	if _, _, err := th.Cover(t.Context(), owner, "music/amber", 96); !errors.Is(err, ErrNoEmbeddedCover) {
		t.Errorf("Cover with the fetched picture gone = %v, want ErrNoEmbeddedCover", err)
	}
	row, err := meta.AlbumLookup(t.Context(), owner, "Autechre", "Amber")
	if err != nil {
		t.Fatal(err)
	}
	if row.CoverBlob != "" || row.RetryAt.IsZero() {
		t.Errorf("the row was not told the picture is gone: %+v", row)
	}
	// And nothing was written down as a verdict: the picture was not read,
	// so there was nothing to judge.
	if _, err := blobs.Stat(t.Context(), artVerdictKey(track.BlobKey)); err == nil {
		t.Error("a missing picture was remembered as a refusal")
	}
}

// TestCoverWithoutLookupsIsWhatItWas: a generator nobody gave the rows to
// answers from the folder and the tag, as it did before #217.
func TestCoverWithoutLookupsIsWhatItWas(t *testing.T) {
	t.Parallel()
	th, service, _, meta := thumbsOver(t)

	track := write(t, service, "music/amber/01.flac", []byte("fLaC"), "audio/flac")
	fetchedFor(t, th, meta, track, db.Media{AlbumArtist: "Autechre", Artist: "Autechre", Album: "Amber", Title: "Foil"})

	if _, _, err := th.Cover(t.Context(), owner, "music/amber", 96); !errors.Is(err, ErrNoEmbeddedCover) {
		t.Errorf("Cover = %v, want ErrNoEmbeddedCover", err)
	}
}

func TestStoreArtRefusesWhatIsNotAPicture(t *testing.T) {
	t.Parallel()
	th, service, blobs := thumbs(t)
	track := write(t, service, "music/amber/01.flac", []byte("fLaC"), "audio/flac")

	err := th.StoreArt(t.Context(), track, []byte("<html>a page where a picture should be</html>"))
	if !errors.Is(err, ErrNotAPicture) {
		t.Errorf("StoreArt of a page = %v, want ErrNotAPicture", err)
	}
	if _, err := blobs.Stat(t.Context(), artKey(track.BlobKey)); err == nil {
		t.Error("the page was stored anyway")
	}
}

// TestFetchedPictureIsNotLookedForWithoutAnAlbum: a track nothing has read,
// and one whose tags name no album, have no row to find a picture by.
func TestFetchedPictureIsNotLookedForWithoutAnAlbum(t *testing.T) {
	t.Parallel()
	th, service, _, meta := thumbsOver(t)
	th.WithLookups(meta)

	write(t, service, "music/unread/01.flac", []byte("fLaC"), "audio/flac")
	if _, _, err := th.Cover(t.Context(), owner, "music/unread", 96); !errors.Is(err, ErrNoEmbeddedCover) {
		t.Errorf("Cover of a track nothing has read = %v, want ErrNoEmbeddedCover", err)
	}

	loose := write(t, service, "music/loose/01.flac", []byte("fLaC"), "audio/flac")
	if err := meta.PutMedia(t.Context(), db.Media{FileID: loose.ID, Kind: db.KindAudio, IndexedAt: time.Now(), Version: 1, Title: "Loose"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := th.Cover(t.Context(), owner, "music/loose", 96); !errors.Is(err, ErrNoEmbeddedCover) {
		t.Errorf("Cover of a track with no album = %v, want ErrNoEmbeddedCover", err)
	}

	// And an album that was looked up and had no cover to fetch.
	bare := write(t, service, "music/bare/01.flac", []byte("fLaC"), "audio/flac")
	if err := meta.PutMedia(t.Context(), db.Media{FileID: bare.ID, Kind: db.KindAudio, IndexedAt: time.Now(), Version: 1,
		AlbumArtist: "Nobody", Album: "Nothing", Title: "Bare"}); err != nil {
		t.Fatal(err)
	}
	if err := meta.PutAlbumLookup(t.Context(), db.AlbumLookup{Owner: owner, Artist: "Nobody", Album: "Nothing", LookedUpAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := th.Cover(t.Context(), owner, "music/bare", 96); !errors.Is(err, ErrNoEmbeddedCover) {
		t.Errorf("Cover of an album with nothing fetched = %v, want ErrNoEmbeddedCover", err)
	}
}

// TestAFetchedPictureThatWillNotDecodeIsRemembered: what the archive handed
// over passed StoreArt's look at its header and still does not decode. That
// is a verdict on the bytes, written down so it is not decoded again on every
// request, and a failure rather than "no cover" -- somebody should see it.
func TestAFetchedPictureThatWillNotDecodeIsRemembered(t *testing.T) {
	t.Parallel()
	th, service, blobs, meta := thumbsOver(t)
	th.WithLookups(meta)

	track := write(t, service, "music/amber/01.flac", []byte("fLaC"), "audio/flac")
	fetchedFor(t, th, meta, track, db.Media{AlbumArtist: "Autechre", Artist: "Autechre", Album: "Amber", Title: "Foil"})
	// A header that decodes and a body that does not.
	truncated := pngBytes(t, 500, 500)[:40]
	if _, err := blobs.Put(t.Context(), artKey(track.BlobKey), bytes.NewReader(truncated), int64(len(truncated))); err != nil {
		t.Fatal(err)
	}

	_, _, err := th.Cover(t.Context(), owner, "music/amber", 96)
	if err == nil || errors.Is(err, ErrNoEmbeddedCover) {
		t.Fatalf("Cover over a picture that will not decode = %v, want a failure", err)
	}
	if _, err := blobs.Stat(t.Context(), artVerdictKey(track.BlobKey)); err != nil {
		t.Errorf("the refusal was not written down: %v", err)
	}
	// The second ask is answered from the note, as no cover.
	if _, _, err := th.Cover(t.Context(), owner, "music/amber", 96); !errors.Is(err, ErrNoEmbeddedCover) {
		t.Errorf("the second ask = %v, want the tag's answer from the note", err)
	}
}

// TestStoreArtRefusesAPictureTooLargeToDecode: a header claiming a hundred
// megapixels is the cheapest way to exhaust a server, and a cover is not one.
func TestStoreArtRefusesAPictureTooLargeToDecode(t *testing.T) {
	t.Parallel()
	th, service, _ := thumbs(t)
	track := write(t, service, "music/amber/01.flac", []byte("fLaC"), "audio/flac")

	// A PNG header and nothing else: DecodeConfig reads the dimensions out
	// of the IHDR chunk and stops.
	huge := pngBytes(t, 1, 1)
	ihdr := huge[8+8 : 8+8+13]
	binary.BigEndian.PutUint32(ihdr[0:4], 20_000)
	binary.BigEndian.PutUint32(ihdr[4:8], 20_000)
	binary.BigEndian.PutUint32(huge[8+8+13:8+8+17], crc32.ChecksumIEEE(huge[8+4:8+8+13]))

	err := th.StoreArt(t.Context(), track, huge)
	if !errors.Is(err, ErrNotAPicture) {
		t.Errorf("StoreArt of a 400 megapixel header = %v, want ErrNotAPicture", err)
	}
}

// TestFetchedPictureOverBackendsThatWillNotAnswer: a database that refuses
// the tags or the row, and a store that refuses the picture, are failures and
// not "no cover" -- except the store refusing to keep one, which StoreArt
// reports to the pass.
func TestFetchedPictureOverBackendsThatWillNotAnswer(t *testing.T) {
	t.Parallel()
	for _, method := range []string{"MediaByFile", "AlbumLookup"} {
		t.Run(method, func(t *testing.T) {
			t.Parallel()
			th, service, _, meta := thumbsOver(t)
			track := write(t, service, "music/amber/01.flac", []byte("fLaC"), "audio/flac")
			fetchedFor(t, th, meta, track, db.Media{AlbumArtist: "Autechre", Artist: "Autechre", Album: "Amber", Title: "Foil"})

			broken := dbtest.FailOn(t, meta, method)
			over := New(th.blobs, files.New(th.blobs, broken), broken, stubFFmpeg(t), t.TempDir()).WithLookups(broken)
			_, _, err := over.Cover(t.Context(), owner, "music/amber", 96)
			if !errors.Is(err, dbtest.ErrInjected) {
				t.Errorf("Cover with %s refusing = %v, want the injected failure", method, err)
			}
		})
	}

	t.Run("Put", func(t *testing.T) {
		t.Parallel()
		th, service, blobs := thumbs(t)
		track := write(t, service, "music/amber/01.flac", []byte("fLaC"), "audio/flac")
		refusing := New(storagetest.FailOn(t, blobs, "Put"), service, nil, stubFFmpeg(t), t.TempDir())
		if err := refusing.StoreArt(t.Context(), track, pngBytes(t, 500, 500)); err == nil || errors.Is(err, ErrNotAPicture) {
			t.Errorf("StoreArt over a store that refuses = %v, want the store's failure", err)
		}
		_ = th
	})
}

// TestCoverOfAnAlbumInAFlatFolder is the bug a flat library showed (#217): a
// folder holding every album has one first track, and asking the folder gave
// every album that track's cover. Asked through a track of its own, each
// album gets its own picture -- and none at all when its own has none.
func TestCoverOfAnAlbumInAFlatFolder(t *testing.T) {
	t.Parallel()
	th, service, blobs, meta := thumbsOver(t)
	th.WithLookups(meta)

	// Path order puts the 80KIDZ track first in the folder; the fetched
	// picture belongs to the other album.
	first := write(t, service, "music/80KIDZ - This Is My Shit.flac", []byte("fLaC"), "audio/flac")
	if err := meta.PutMedia(t.Context(), db.Media{FileID: first.ID, Kind: db.KindAudio, IndexedAt: time.Now(), Version: 1,
		AlbumArtist: "80KIDZ", Album: "This Is My Shit", Title: "This Is My Shit"}); err != nil {
		t.Fatal(err)
	}
	obel := write(t, service, "music/Agnes Obel - Familiar.flac", []byte("fLaC"), "audio/flac")
	fetchedFor(t, th, meta, obel, db.Media{AlbumArtist: "Agnes Obel", Artist: "Agnes Obel", Album: "Aventine", Title: "Familiar"})

	body, _, err := th.CoverOf(t.Context(), owner, obel, 96)
	if err != nil {
		t.Fatalf("CoverOf the album with a fetched picture: %v", err)
	}
	_ = body.Close()
	if _, _, err := th.CoverOf(t.Context(), owner, first, 96); !errors.Is(err, ErrNoEmbeddedCover) {
		t.Errorf("CoverOf the album with none = %v, want ErrNoEmbeddedCover rather than its neighbour's", err)
	}
	// The folder's own answer is still its first track's, which is what a
	// folder browser asks for.
	if _, _, err := th.Cover(t.Context(), owner, "music", 96); !errors.Is(err, ErrNoEmbeddedCover) {
		t.Errorf("Cover of the folder = %v, want the first track's answer", err)
	}
	if _, err := blobs.Stat(t.Context(), artSizeKey(obel.BlobKey, thumbSmall)); err != nil {
		t.Errorf("the album's picture was not filed under its own track: %v", err)
	}
}

// TestCoverOfPrefersThePictureBesideTheTrack: the folder's picture still wins
// for an album asked by its track, as it does for the folder.
func TestCoverOfPrefersThePictureBesideTheTrack(t *testing.T) {
	t.Parallel()
	th, service, blobs := thumbs(t)
	track := write(t, service, "music/album/01.flac", flacWithCover(t, 400, 400), "audio/flac")
	beside := writeImage(t, service, "music/album/cover.jpg", 300, 300)

	body, _, err := th.CoverOf(t.Context(), owner, track, 96)
	if err != nil {
		t.Fatalf("CoverOf: %v", err)
	}
	_ = body.Close()
	if _, err := blobs.Stat(t.Context(), thumbKey(beside.BlobKey, thumbSmall)); err != nil {
		t.Errorf("the folder picture was not used: %v", err)
	}
	if _, err := blobs.Stat(t.Context(), coverKey(track.BlobKey, thumbSmall)); err == nil {
		t.Error("the tag was parsed with a picture beside the track")
	}
}
