package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png" // registered so a PNG cover decodes
	"io"
	"log/slog"
	"path"
	"strconv"
	"strings"

	"golang.org/x/image/draw"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/storage"
)

// Thumbnails are generated on first request and kept in the blob store.
//
// **Lazily, not on upload.** A phone backing up five hundred photos would
// otherwise pay a decode and a resize per PUT with the client waiting, which
// turns an overnight backup into something that times out. Lazily is also the
// only path that covers files which arrived some other way -- a bucket adopted
// in place, where nothing ever passed through the write path.
//
// **In the blob store, not a cache of its own.** That was going to be a cache
// port with disk, database and Redis backends, and it should not be: a third
// pluggable seam is what principle 3 forbids and Redis is named in principle 1
// as a thing this project does not have. The store already satisfies every
// requirement -- a container with no volume loses nothing, deleting one
// regenerates it, and the key is a pure function of the original's.

// thumbSize is one of the sizes a thumbnail comes in.
//
// A fixed ladder rather than any number a caller asks for, because the size is
// part of the key: an arbitrary size means an unbounded set of derived objects,
// and a client that asks for 301 pixels once leaves one behind forever. Callers
// ask in pixels and snapSize decides.
type thumbSize int

// The ladder. Small is a list row, medium a grid cell, large a cover filling a
// phone screen.
const (
	thumbSmall  thumbSize = 96
	thumbMedium thumbSize = 300
	thumbLarge  thumbSize = 1200
)

// thumbLadder is the ladder in ascending order, which is what snapSize walks.
var thumbLadder = []thumbSize{thumbSmall, thumbMedium, thumbLarge}

// snapSize rounds a requested size up to the ladder, because a thumbnail bigger
// than asked for still displays and a smaller one does not. A request for more
// than the largest gets the largest: this serves thumbnails, and the original
// is what WebDAV is for.
func snapSize(px int) thumbSize {
	for _, size := range thumbLadder {
		if px <= int(size) {
			return size
		}
	}
	return thumbLarge
}

// thumbQuality is the JPEG quality derived images are written at. 82 is the
// usual place to stop: the file stops shrinking meaningfully below it and the
// artefacts start showing above.
const thumbQuality = 82

// maxThumbSource bounds what will be decoded. A decoded image is four bytes a
// pixel whatever the file compressed to, so a 100 MP photo is 400 MB of memory
// -- and a malformed header claiming that is the cheapest way to exhaust a
// server. Nothing a camera produces comes close to this.
const maxThumbSource = 50_000_000

// ErrNoThumbnail means the file is not something this build can turn into
// pixels. HEIC and video need the ffmpeg path, which is not wired yet; camera
// raw needs the embedded preview, which is a different technique.
var ErrNoThumbnail = errors.New("media: no thumbnail for this file")

// Thumbs makes thumbnails and remembers them.
type Thumbs struct {
	blobs storage.Storage
	files *files.Service
}

// NewThumbs wires the generator. It takes the blob store directly because a
// derived object has no database row -- the blob-plus-row invariant internal
// files exists to hold does not apply to something regenerable.
func NewThumbs(blobs storage.Storage, service *files.Service) *Thumbs {
	return &Thumbs{blobs: blobs, files: service}
}

// Open returns a thumbnail of f at size, making it if this is the first ask.
//
// Two requests for the same missing thumbnail both generate it and both write
// it. That is deliberate rather than overlooked: the content is a pure function
// of the input, so the second write stores the same bytes as the first, and a
// single-flight would be machinery guarding against duplicated work rather than
// against a wrong answer. It becomes worth adding when a photo grid asks for
// five hundred at once, which is #8's problem.
func (t *Thumbs) Open(ctx context.Context, f db.File, px int) (io.ReadCloser, int64, error) {
	size := snapSize(px)
	key := thumbKey(f.BlobKey, size)

	body, info, err := t.blobs.Get(ctx, key, storage.All())
	switch {
	case err == nil:
		return body, info.Size, nil
	case !errors.Is(err, storage.ErrNotFound):
		return nil, 0, fmt.Errorf("read the thumbnail of %q: %w", f.Path, err)
	}

	made, err := t.generate(ctx, f, size)
	if err != nil {
		return nil, 0, err
	}
	if _, err := t.blobs.Put(ctx, key, bytes.NewReader(made), int64(len(made))); err != nil {
		// A thumbnail that could not be stored is still a thumbnail. Serving it
		// and logging the failure beats refusing a picture we are holding.
		slog.WarnContext(ctx, "storing a thumbnail", "key", key, "err", err)
	}
	return io.NopCloser(bytes.NewReader(made)), int64(len(made)), nil
}

// coverNames are the file names an album's artwork goes by, in the order they
// are preferred. It is a list and not a guess at "the only image in there": a
// folder often holds a back cover and a scan of the booklet too, and picking
// one of those is worse than picking none.
var coverNames = []string{
	"cover", "folder", "front", "album", "albumart", "albumartsmall", "thumb",
}

// coverExtensions are what a cover is stored as, and both are what Go decodes
// without help. A cover.webp exists in the wild and waits for that decoder.
var coverExtensions = []string{".jpg", ".jpeg", ".png"}

// FolderCover finds the artwork sitting beside the music in dir, or
// storage.ErrNotFound if there is none.
//
// This is where an album's cover lives for most libraries: ripped or downloaded
// as a folder, with the picture next to the tracks. The other place is inside
// the files themselves, which needs an FFmpeg build carrying the audio demuxers
// and an image muxer -- see the issue, it is a recipe change and not a design
// one.
func (t *Thumbs) FolderCover(ctx context.Context, owner, dir string) (db.File, error) {
	entries, err := t.files.List(ctx, owner, dir)
	if err != nil {
		return db.File{}, fmt.Errorf("look for cover art in %q: %w", dir, err)
	}

	// By preference and not by directory order, so the same folder answers the
	// same picture on every backend: the storage port does not promise an order
	// and neither does a listing.
	byName := make(map[string]db.File, len(entries))
	for _, f := range entries {
		if !f.IsDir {
			byName[strings.ToLower(path.Base(f.Path))] = f
		}
	}
	for _, name := range coverNames {
		for _, ext := range coverExtensions {
			if f, ok := byName[name+ext]; ok {
				return f, nil
			}
		}
	}
	return db.File{}, fmt.Errorf("cover art in %q: %w", dir, storage.ErrNotFound)
}

// generate decodes the original, reduces it and encodes a JPEG.
//
// JPEG for everything, including a PNG cover: a thumbnail is a photograph of
// something most of the time, transparency is not meaningful at this size, and
// one output format means one path through every client.
func (t *Thumbs) generate(ctx context.Context, f db.File, size thumbSize) ([]byte, error) {
	if !decodableInGo(f.Path) {
		return nil, fmt.Errorf("%w: %s", ErrNoThumbnail, path.Ext(f.Path))
	}

	body, err := t.files.OpenFile(ctx, f)
	if err != nil {
		return nil, fmt.Errorf("open %q: %w", f.Path, err)
	}
	defer func() { _ = body.Close() }()

	src, _, err := image.Decode(io.LimitReader(body, maxThumbSource))
	if err != nil {
		return nil, fmt.Errorf("%w: decoding %q: %w", ErrNoThumbnail, f.Path, err)
	}

	var out bytes.Buffer
	if err := jpeg.Encode(&out, reduce(src, int(size)), &jpeg.Options{Quality: thumbQuality}); err != nil {
		return nil, fmt.Errorf("encode a thumbnail of %q: %w", f.Path, err)
	}
	return out.Bytes(), nil
}

// reduce scales src to fit in a box of side px, keeping its aspect ratio and
// never enlarging: a cover smaller than the box is served as it is rather than
// blown up into something blurrier than the original.
//
// CatmullRom because this runs once per image per size and the result is looked
// at many times, so the sharper filter is the cheap one over the life of the
// thumbnail.
func reduce(src image.Image, px int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= px && h <= px {
		return src
	}

	if w > h {
		h = h * px / w
		w = px
	} else {
		w = w * px / h
		h = px
	}
	// A very wide panorama can round the short side to nothing, and an image of
	// zero height encodes to a file nothing can open.
	w, h = max(w, 1), max(h, 1)

	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Src, nil)
	return dst
}

// thumbKey is where a thumbnail lives. The size is in the name and the parent
// blob is in the path, which is what lets one sweep collect both -- see
// files.DerivedKey.
func thumbKey(blobKey string, size thumbSize) string {
	return files.DerivedKey(blobKey, strconv.Itoa(int(size))+".jpg")
}

// decodableInGo reports whether this build can read the file without ffmpeg.
//
// Only what the standard library decodes today. WebP and TIFF wait for the
// x/image decoders, and HEIC and video for the ffmpeg path -- the point of the
// list is that a format is either read here or refused honestly, never read
// badly.
func decodableInGo(p string) bool {
	switch strings.ToLower(path.Ext(p)) {
	case ".jpg", ".jpeg", ".png":
		return true
	}
	return false
}
