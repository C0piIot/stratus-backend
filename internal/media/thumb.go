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

// generating bounds how many thumbnails are decoded at once.
//
// A photo grid asks for everything it can see the first time a folder is
// opened, and decoding a twelve-megapixel JPEG costs about fifty megabytes
// while it happens. Twenty at once on the smallest machine anybody runs this on
// is the difference between a slow page and an OOM kill. The browser's lazy
// loading keeps the number small; this keeps it bounded.
const generating = 4

// Thumbs makes thumbnails and remembers them.
type Thumbs struct {
	blobs storage.Storage
	files *files.Service
	// decoding is the semaphore generating describes. Buffered rather than a
	// sync primitive so that waiting for it can be cancelled with the request.
	decoding chan struct{}
}

// NewThumbs wires the generator. It takes the blob store directly because a
// derived object has no database row -- the blob-plus-row invariant internal
// files exists to hold does not apply to something regenerable.
func NewThumbs(blobs storage.Storage, service *files.Service) *Thumbs {
	return &Thumbs{blobs: blobs, files: service, decoding: make(chan struct{}, generating)}
}

// File returns a thumbnail of the file at path, making it if this is the first
// ask, and ErrNoThumbnail if this build cannot read that format.
func (t *Thumbs) File(ctx context.Context, owner, path string, px int) (io.ReadCloser, int64, error) {
	f, err := t.files.Stat(ctx, owner, path)
	if err != nil {
		return nil, 0, err
	}
	if f.IsDir {
		// A folder has cover art rather than a thumbnail, and that is Cover's
		// question. Saying so beats making a picture of nothing.
		return nil, 0, fmt.Errorf("%w: %q is a directory", ErrNoThumbnail, path)
	}
	return t.open(ctx, f, px)
}

// Cover returns the artwork for the folder dir, at about px pixels.
//
// Two places hold it and this is the only thing that knows there are two: a
// picture beside the tracks, or one inside the first of them. A caller asks for
// a folder's cover and gets bytes.
func (t *Thumbs) Cover(ctx context.Context, owner, dir string, px int) (io.ReadCloser, int64, error) {
	entries, err := t.files.List(ctx, owner, dir)
	if err != nil {
		return nil, 0, fmt.Errorf("look for cover art in %q: %w", dir, err)
	}

	if picture, ok := folderCover(entries); ok {
		return t.open(ctx, picture, px)
	}
	if track, ok := firstTrack(entries); ok {
		return t.fromTrack(ctx, track, px)
	}
	return nil, 0, fmt.Errorf("cover art in %q: %w", dir, storage.ErrNotFound)
}

// open returns a thumbnail of f, making it if this is the first ask.
//
// Two requests for the same missing thumbnail both generate it and both write
// it. That is deliberate rather than overlooked: the content is a pure function
// of the input, so the second write stores the same bytes as the first, and a
// single-flight would be machinery guarding against duplicated work rather than
// against a wrong answer. It becomes worth adding when a photo grid asks for
// five hundred at once, which is #8's problem.
func (t *Thumbs) open(ctx context.Context, f db.File, px int) (io.ReadCloser, int64, error) {
	size := snapSize(px)
	return t.cached(ctx, thumbKey(f.BlobKey, size), func() ([]byte, error) {
		if !decodableInGo(f.Path) {
			return nil, fmt.Errorf("%w: %s", ErrNoThumbnail, path.Ext(f.Path))
		}
		body, err := t.files.OpenFile(ctx, f)
		if err != nil {
			return nil, fmt.Errorf("open %q: %w", f.Path, err)
		}
		defer func() { _ = body.Close() }()
		return reduceTo(body, f.Path, size)
	})
}

// fromTrack returns a thumbnail of the picture inside a track.
//
// Filed under the track's blob and not the picture's, because the picture has no
// blob of its own -- which is what keeps the sweep able to collect it: delete the
// track and its cover goes with it.
func (t *Thumbs) fromTrack(ctx context.Context, f db.File, px int) (io.ReadCloser, int64, error) {
	size := snapSize(px)
	return t.cached(ctx, coverKey(f.BlobKey, size), func() ([]byte, error) {
		body, err := t.files.OpenFile(ctx, f)
		if err != nil {
			return nil, fmt.Errorf("open %q: %w", f.Path, err)
		}
		defer func() { _ = body.Close() }()

		// Off the blob and through a ranged read: the tag is at the head of
		// every one of these containers, so a track in a bucket costs a few
		// kilobytes rather than the whole file.
		picture, err := embeddedCover(body, f.Path)
		if err != nil {
			return nil, err
		}
		return reduceTo(bytes.NewReader(picture), f.Path, size)
	})
}

// cached is the lazy half: answer what is stored, or make it, keep it and
// answer that.
func (t *Thumbs) cached(
	ctx context.Context, key string, make func() ([]byte, error),
) (io.ReadCloser, int64, error) {
	body, info, err := t.blobs.Get(ctx, key, storage.All())
	switch {
	case err == nil:
		return body, info.Size, nil
	case !errors.Is(err, storage.ErrNotFound):
		return nil, 0, fmt.Errorf("read %q: %w", key, err)
	}

	// The cached read above is deliberately outside the bound: serving a
	// thumbnail that exists costs no pixels and must not queue behind one being
	// made. Only the decode is limited.
	select {
	case t.decoding <- struct{}{}:
		defer func() { <-t.decoding }()
	case <-ctx.Done():
		return nil, 0, ctx.Err()
	}

	made, err := make()
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

// reduceTo decodes, reduces and encodes, which is the same path whether the
// pixels came from a file of their own or out of a tag.
//
// JPEG for everything, including a PNG cover: a thumbnail is a photograph of
// something most of the time, transparency is not meaningful at this size, and
// one output format means one path through every client.
func reduceTo(r io.Reader, name string, size thumbSize) ([]byte, error) {
	src, _, err := image.Decode(io.LimitReader(r, maxThumbSource))
	if err != nil {
		return nil, fmt.Errorf("%w: decoding %q: %w", ErrNoThumbnail, name, err)
	}

	var out bytes.Buffer
	if err := jpeg.Encode(&out, reduce(src, int(size)), &jpeg.Options{Quality: thumbQuality}); err != nil {
		return nil, fmt.Errorf("encode a thumbnail of %q: %w", name, err)
	}
	return out.Bytes(), nil
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

// folderCover picks the artwork out of a directory listing.
//
// This is where an album's cover lives for most libraries: ripped or downloaded
// as a folder, with the picture next to the tracks.
func folderCover(entries []db.File) (db.File, bool) {
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
				return f, true
			}
		}
	}
	return db.File{}, false
}

// firstTrack is the file whose tag is read when the folder holds no picture.
//
// The *first* one in path order and not whichever has a picture, deliberately: a
// folder of twelve tracks would otherwise cost twelve ranged reads on every
// request that finds no art, and every track of a record carries the same cover
// in practice. A folder whose first track alone is untagged reports no cover,
// which is the price.
func firstTrack(entries []db.File) (db.File, bool) {
	best := db.File{}
	for _, f := range entries {
		if f.IsDir || byExtension[strings.ToLower(path.Ext(f.Path))] != db.KindAudio {
			continue
		}
		if best.Path == "" || f.Path < best.Path {
			best = f
		}
	}
	return best, best.Path != ""
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

// thumbKey and coverKey are where a derived picture lives. The size is in the
// name and the parent blob is in the path, which is what lets one sweep collect
// both -- see files.DerivedKey.
//
// Two names because one blob can have two derived pictures: a track is both
// something with a thumbnail of its own one day and the place its album's cover
// is kept today.
func thumbKey(blobKey string, size thumbSize) string {
	return files.DerivedKey(blobKey, strconv.Itoa(int(size))+".jpg")
}

func coverKey(blobKey string, size thumbSize) string {
	return files.DerivedKey(blobKey, "cover-"+strconv.Itoa(int(size))+".jpg")
}

// CanThumbnail reports whether this build can turn the file at p into pixels.
//
// Exported so that a page deciding whether to render an image asks the same
// question the generator will answer, rather than keeping a second list of
// extensions that drifts from this one. It is a property of the build and not
// of the file, which is why it is computed here and stored nowhere: the day the
// ffmpeg path is wired, every HEIC changes its answer without a byte moving.
func CanThumbnail(p string) bool { return decodableInGo(p) }

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
