// Package thumb makes the pictures: a thumbnail of a photograph or a film, a
// cover out of a track's head, a folder or a lookup, and the pass that makes
// them ahead of anybody asking. It reads what internal/media knows about a file
// and decides nothing about what plays, which is internal/media/stream's.
package thumb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"log/slog"
	"os"
	"path"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/evanoberholster/imagemeta"
	"golang.org/x/image/draw"

	_ "image/png" // registered so a PNG cover decodes

	"github.com/stratus-private-cloud/stratus-backend/internal/db"
	"github.com/stratus-private-cloud/stratus-backend/internal/files"
	"github.com/stratus-private-cloud/stratus-backend/internal/media"
	"github.com/stratus-private-cloud/stratus-backend/internal/storage"
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

// defaultMaxWait is how long a request may stand in the queue before it is
// refused anyway. The bottom of the queue bounds a burst; this bounds a decode
// that turns out to be slow -- a 4K frame through ffmpeg on a shared CPU --
// behind which the queue is short but the wait is not.
const defaultMaxWait = 10 * time.Second

// Service makes thumbnails and remembers them.
type Service struct {
	blobs storage.Storage
	files *files.Service
	// ffmpeg is the path to the binary that reads what Go cannot, and tmpDir is
	// where a blob is put down for it: it opens files and seeks in them, which
	// the storage port does not offer.
	ffmpeg string
	tmpDir string
	// decoding is the semaphore generating sizes (slots.go). Buffered rather
	// than a sync primitive so that waiting for it can be cancelled with the
	// request.
	decoding chan struct{}
	// inQueue counts what is waiting for that semaphore and what holds it, and
	// queue is the most there may be. A channel cannot report its own queue,
	// so the counter is what makes refusing possible at all.
	inQueue atomic.Int64
	queue   int
	// maxWait is how long one may stand in it. A field rather than the
	// constant it is set from, so a test can reach the deadline without
	// standing there for ten media.Seconds.
	maxWait time.Duration
	// meta is where a verdict is written down where a page and a pass can
	// read it in one query (#341). The note beside the picture answers one
	// blob at a time, which is the right shape for the next request about
	// that file and the wrong one for a hundred rows or a library.
	meta db.MediaIndex
	// lookups is where a fetched cover is found from, and nil until
	// WithLookups: see art.go.
	lookups db.Lookups
	// yield is asked before a pass decodes, and nil until YieldTo. Prepare is
	// the passes' alone, so a picture somebody asked for never waits on it.
	yield media.Yield
}

// New wires the generator. It takes the blob store directly because a
// derived object has no database row -- the blob-plus-row invariant internal
// files exists to hold does not apply to something regenerable.
func New(blobs storage.Storage, service *files.Service, meta db.MediaIndex, ffmpeg, tmpDir string) *Service {
	slots := generating()
	queue := decodeQueue(slots)
	// Said once at startup, because it is worked out from the machine and a
	// number nobody set is a number nobody would otherwise know.
	slog.Info("thumbnails", "at_once", slots, "queue", queue,
		"cpus", runtime.GOMAXPROCS(0), "memory_limit", media.MemoryLimit(os.DirFS("/")))
	return &Service{
		blobs: blobs, files: service, meta: meta, ffmpeg: ffmpeg, tmpDir: tmpDir,
		decoding: make(chan struct{}, slots), queue: queue, maxWait: defaultMaxWait,
	}
}

// YieldTo makes the passes ask y before each picture they decode (#455).
func (t *Service) YieldTo(y media.Yield) *Service {
	t.yield = y
	return t
}

// File returns a thumbnail of the file at path, making it if this is the first
// ask, and ErrNoThumbnail if this build cannot read that format.
func (t *Service) File(ctx context.Context, owner, path string, px int) (io.ReadCloser, int64, error) {
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
// Three places hold it and this is the only thing that knows there are three:
// a picture beside the tracks, one inside the first of them, or one fetched
// for the album from outside (#217) -- in that order, because the first two
// are the user's own. A caller asks for a folder's cover and gets bytes.
//
// This is the folder's answer: the first track in it decides, whatever
// album it belongs to. An album asks through CoverOf with a track of its own,
// because a folder holding several albums -- a flat library -- has one first
// track and they are not all its.
func (t *Service) Cover(ctx context.Context, owner, dir string, px int) (io.ReadCloser, int64, error) {
	entries, err := t.files.List(ctx, owner, dir)
	if err != nil {
		return nil, 0, fmt.Errorf("look for cover art in %q: %w", dir, err)
	}

	if picture, ok := folderCover(entries); ok {
		return t.open(ctx, picture, px)
	}
	if track, ok := firstTrack(entries); ok {
		return t.trackCover(ctx, track, px)
	}
	return nil, 0, fmt.Errorf("cover art in %q: %w", dir, storage.ErrNotFound)
}

// CoverOf returns the artwork for the album track f belongs to, at about px
// pixels: a picture beside it in its folder, else the one inside f itself,
// else the one fetched for its album. The same three places as Cover, with
// the track chosen by the caller rather than by the folder's listing -- which
// is the difference between an album's cover and a folder's when the two are
// not the same thing.
func (t *Service) CoverOf(ctx context.Context, owner string, f db.File, px int) (io.ReadCloser, int64, error) {
	dir := db.ParentOf(f.Path)
	entries, err := t.files.List(ctx, owner, dir)
	if err != nil {
		return nil, 0, fmt.Errorf("look for cover art in %q: %w", dir, err)
	}
	if picture, ok := folderCover(entries); ok {
		return t.open(ctx, picture, px)
	}
	return t.trackCover(ctx, f, px)
}

// trackCover is the second and third place: the picture inside the track,
// and failing that the one fetched for its album (#217). The fetched half's
// own "nothing" keeps the tag's answer, so the surfaces above go on telling a
// 404 from a failure the way they always have.
func (t *Service) trackCover(ctx context.Context, track db.File, px int) (io.ReadCloser, int64, error) {
	body, size, err := t.fromTrack(ctx, track, px)
	if !errors.Is(err, ErrNoEmbeddedCover) {
		return body, size, err
	}
	body, size, ferr := t.fetchedArt(ctx, track, px)
	switch {
	case ferr == nil:
		return body, size, nil
	case errors.Is(ferr, errNoArt), errors.Is(ferr, storage.ErrNotFound), errors.Is(ferr, db.ErrNotFound):
		return nil, 0, err
	default:
		return nil, 0, ferr
	}
}

// open returns a thumbnail of f, making it if this is the first ask.
//
// Two requests for the same missing thumbnail both generate it and both write
// it. That is deliberate rather than overlooked: the content is a pure function
// of the input, so the second write stores the same bytes as the first, and a
// single-flight would be machinery guarding against duplicated work rather than
// against a wrong answer. It becomes worth adding when a photo grid asks for
// five hundred at once, which is #8's problem.
func (t *Service) open(ctx context.Context, f db.File, px int) (io.ReadCloser, int64, error) {
	if !CanThumbnail(f.Path, f.Size) {
		// Including the file that is simply too big, which is refused here
		// rather than attempted: the listing does not offer one either, so
		// this is the answer to somebody who asked for the URL directly.
		//
		// Nothing is remembered about it, because nothing was read: the answer
		// is a function of the name and of this build, so a build that learns
		// the format changes it with no object to invalidate.
		return nil, 0, fmt.Errorf("%w: %s", ErrNoThumbnail, path.Ext(f.Path))
	}

	size := snapSize(px)
	refused := verdict{note: verdictKey(f.BlobKey), says: ErrNoThumbnail, file: f.ID, blobKey: f.BlobKey}
	return t.cached(ctx, thumbKey(f.BlobKey, size), refused, func() ([]byte, error) {
		made := make(map[thumbSize][]byte, 1)
		if err := t.decodeInto(ctx, f, []thumbSize{size}, made); err != nil {
			return nil, err
		}
		return made[size], nil
	})
}

// decodeInto makes every size asked for out of one decode.
//
// That is the whole of what a pass is worth over the lazy path (#341): the
// decode is the cost, a second encode is a hundredth of it, and the sizes a
// grid and a listing use are 96 and 300. A request asks for one size and gets
// the same path with one entry in it.
func (t *Service) decodeInto(ctx context.Context, f db.File, sizes []thumbSize, out map[thumbSize][]byte) error {
	return t.fromStore(ctx, f, func(body io.ReadSeeker) error {
		pic, err := t.decode(ctx, f, body, largest(sizes))
		if err != nil {
			return err
		}
		for _, size := range sizes {
			made, err := pic.at(size)
			if err != nil {
				return err
			}
			out[size] = made
		}
		return nil
	})
}

// decode is that one decode, by whichever half can read the file: Go for a
// JPEG or a PNG, and ffmpeg for a HEIC or a frame out of a video.
//
// The second costs a copy of the file, which is the one thing #48 was about
// removing -- and it is unavoidable here for the reason it was unavoidable
// there, that a binary opens files and seeks in them. What makes it acceptable
// is that it happens once per file: the sizes come out of the frame, and the
// next ask reads the derived blob.
func (t *Service) decode(ctx context.Context, f db.File, body io.ReadSeeker, size thumbSize) (decoded, error) {
	if decodableInGo(f.Path) {
		return decodeInGo(body, f.Path)
	}

	// Small files are copied whole, which is the simplest thing and costs
	// megabytes. Above the bound nothing is copied: what a decoder needs is
	// the headers and the first frames, and media.Window fetches exactly those
	// (#149).
	local, cleanup, err := t.localCopy(body, f)
	if err != nil {
		return decoded{}, err
	}
	defer cleanup()

	frame, err := decodeScaled(ctx, t.ffmpeg, local, int(size), media.IsVideo(f.Path))
	if err != nil {
		return decoded{}, fmt.Errorf("%w: %q: %w", ErrNoThumbnail, f.Path, err)
	}
	// Straightened by ffmpeg already, and scaled by it to the largest size
	// asked for -- so a smaller one is reduced from that frame rather than
	// decoded a second time. Those pixels are not bit for bit what a decode
	// straight to 96 produces, which costs nothing: it is the same picture at
	// the same size, and the alternative is a second 4K decode to save a
	// resize.
	return decoded{src: frame, name: f.Path}, nil
}

// largest is the size a decode has to produce for a set of them, since every
// smaller one is reduced from it.
func largest(sizes []thumbSize) thumbSize {
	var biggest thumbSize
	for _, size := range sizes {
		biggest = max(biggest, size)
	}
	return biggest
}

// localCopy is the file handed to ffmpeg: the whole thing when that is cheap,
// and a media.Window onto it when it is not.
//
// There is no third option. A film that cannot be windowed has no thumbnail,
// because the alternative is the download the rule forbids -- and the listing
// does not offer one for it either, so nothing here is answering a question
// somebody could have asked.
func (t *Service) localCopy(body io.ReadSeeker, f db.File) (string, func(), error) {
	if f.Size <= media.MaxSpool {
		return media.Spool(body, t.tmpDir, f.Path)
	}
	return media.Window(body, f.Size, f.Path, t.tmpDir, media.WindowHead)
}

// fromTrack returns a thumbnail of the picture inside a track.
//
// Filed under the track's blob and not the picture's, because the picture has no
// blob of its own -- which is what keeps the sweep able to collect it: delete the
// track and its cover goes with it.
func (t *Service) fromTrack(ctx context.Context, f db.File, px int) (io.ReadCloser, int64, error) {
	size := snapSize(px)
	// A track with no picture in its head is the normal state of half a music
	// library, so what is remembered says exactly that: the album pages and
	// getCoverArt answer a track with no cover with a 404 and a track whose
	// picture will not decode with a 500, and a note that forgot which is
	// which would turn the first into the second for ever.
	refused := verdict{note: coverVerdictKey(f.BlobKey), says: ErrNoEmbeddedCover}
	return t.cached(ctx, coverKey(f.BlobKey, size), refused, func() ([]byte, error) {
		var made []byte
		err := t.fromStore(ctx, f, func(body io.ReadSeeker) error {
			// Off the blob and through a ranged read: the tag is at the head of
			// every one of these containers, so a track in a bucket costs a few
			// kilobytes rather than the whole file.
			picture, err := embeddedCover(body, f.Path)
			if err != nil {
				return err
			}
			made, err = reduceTo(bytes.NewReader(picture), f.Path, size)
			return err
		})
		return made, err
	})
}

// fromStore opens f and runs make over a reader that remembers what the store
// did.
//
// That is #157's distinction on this side of the package, and it is only
// answerable here: a bucket that stops answering halfway through a decode
// looks exactly like a file that does not decode, because the decoder rephrases
// every failed read as its own "these are not pixels". Only the reader between
// the two knows which it was, and getting it wrong now costs more than it used
// to -- a verdict is written down.
func (t *Service) fromStore(ctx context.Context, f db.File, use func(io.ReadSeeker) error) error {
	body, err := t.files.OpenFile(ctx, f)
	if err != nil {
		return fmt.Errorf("%w: open %q: %w", media.ErrUnreachable, f.Path, err)
	}
	defer func() { _ = body.Close() }()

	r := &media.StoreReader{ReadSeekCloser: body}
	err = use(r)
	if err == nil || r.Err == nil {
		return err
	}
	return fmt.Errorf("%w: %w", media.ErrUnreachable, r.Err)
}

// verdict is where the outcome of a decode is written down, and what the next
// caller is told when it was a refusal.
//
// Two places, because they answer two readers. **The note** is an object
// beside the picture that was not made, keyed by the blob and holding the
// reason in words for whoever is reading a data directory -- and it is what
// the next request asks about, since the question there is about bytes and an
// object is keyed by them. **The row** is the same fact where a page and a
// pass can read it in one query rather than in one HEAD per file (#341). They
// carry the same week, so they cannot disagree for long: the sweep takes the
// note after one, and PendingThumbs offers a refusal again after the same one.
//
// The sentinel is the caller's because the surfaces behind it answer them
// differently, and a note that flattened the two would be a regression the
// first request hides and the second shows.
type verdict struct {
	note string
	says error
	// file and blobKey are the row half, and file is zero where there is no
	// row to use: cover art is a second picture per blob and the column is
	// about the one a grid draws, so a track's answer stays a note alone.
	file    int64
	blobKey string
}

// cached is the lazy half: answer what is stored, or make it, keep it and
// answer that -- or, for what has already been refused once, refuse it again
// without reading a byte (#338).
func (t *Service) cached(
	ctx context.Context, key string, refused verdict, make func() ([]byte, error),
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

	// And so is this one, for a stronger reason: a file nothing can decode must
	// not take a place in the queue in front of one something can. An AV1
	// recording in a folder of photographs used to cost a media.Spool and an ffmpeg
	// run on every page view, and on the one-CPU instance that is the slot the
	// photograph beside it was waiting for.
	if _, ok := t.refusedBefore(ctx, refused.note); ok {
		return nil, 0, fmt.Errorf("%w: refused before, see %q", refused.says, refused.note)
	}

	// The queue in front of it has a bottom and a deadline, and refusing is
	// the better answer past either: a thumbnail is regenerable and the
	// request will be made again, while a request that stands in line for a
	// minute holds a connection and keeps the only CPU busy for the pages
	// behind it.
	if t.inQueue.Add(1) > int64(t.queue) {
		t.inQueue.Add(-1)
		return nil, 0, media.ErrBusy
	}
	defer func() { t.inQueue.Add(-1) }()

	wait := time.NewTimer(t.maxWait)
	defer wait.Stop()
	select {
	case t.decoding <- struct{}{}:
		defer func() { <-t.decoding }()
	case <-wait.C:
		return nil, 0, media.ErrBusy
	case <-ctx.Done():
		return nil, 0, ctx.Err()
	}

	made, err := make()
	if err != nil {
		t.remember(ctx, refused, err)
		return nil, 0, err
	}
	// The row learns from the lazy path as well as from the pass, so a folder
	// somebody opens once is a folder the pass does not come back to.
	t.record(ctx, refused, db.ThumbMade)
	if _, err := t.blobs.Put(ctx, key, bytes.NewReader(made), int64(len(made))); err != nil {
		// A thumbnail that could not be stored is still a thumbnail. Serving it
		// and logging the failure beats refusing a picture we are holding.
		slog.WarnContext(ctx, "storing a thumbnail", "key", key, "err", err)
	}
	return io.NopCloser(bytes.NewReader(made)), int64(len(made)), nil
}

// refusedBefore reports whether this blob has already been judged, and when.
//
// A Stat and not a Get: what the note says is for whoever reads the store, and
// the answer here is that there is one. A store that fails some other way says
// no, which costs a decode -- the Get above has already passed on anything
// worse than a missing object.
func (t *Service) refusedBefore(ctx context.Context, key string) (time.Time, bool) {
	info, err := t.blobs.Stat(ctx, key)
	return info.ModTime, err == nil
}

// remember writes the refusal down, and writes nothing when the failure was not
// about the file.
//
// The note is a cache in the sweep's sense, so it is collected a week after it
// was written whatever its parent is doing -- which is the answer to the thing
// a verdict cannot know: whether this build is still the build that made it.
// Adding a decoder to the ffmpeg recipe un-refuses every file that needed it,
// a week later and with no generation raised and no thumbnail regenerated. The
// price is one attempt per refused file per week, which is nothing, and it is
// what makes it safe to be wrong here.
func (t *Service) remember(ctx context.Context, v verdict, why error) {
	if !isVerdict(ctx, why) {
		return
	}

	reason := why.Error()
	if _, err := t.blobs.Put(ctx, v.note, strings.NewReader(reason), int64(len(reason))); err != nil {
		slog.WarnContext(ctx, "storing a thumbnail refusal", "key", v.note, "err", err)
	}
	t.record(ctx, v, db.ThumbRefused)
}

// isVerdict says whether an error is about the file rather than about this
// server's reach, which is #157's distinction and the whole of what decides
// whether anything is written down at all.
func isVerdict(ctx context.Context, why error) bool {
	switch {
	case media.Unreachable(why), errors.Is(why, storage.ErrNotFound):
		// The bytes were not read, so there is nothing to judge. A blob the
		// store does not have is the same answer: a database pointed at the
		// wrong bucket would otherwise write a verdict for every file in the
		// library (#271's fear, one layer along).
		return false
	case ctx.Err() != nil, errors.Is(why, context.Canceled), errors.Is(why, context.DeadlineExceeded):
		// Whoever was looking at the page left, or the decode ran out of time.
		// Neither is the file's fault.
		return false
	}
	return true
}

// record writes the verdict where it can be read in batch, and says nothing
// about a picture that has no row to be said on.
//
// A file with no media row is not an error worth reporting: either the
// extractor has not reached it yet, in which case the pass will, or it was
// deleted while its picture was being made, in which case there is nothing to
// record it against.
func (t *Service) record(ctx context.Context, v verdict, state db.ThumbState) {
	t.recordAt(ctx, v, state, time.Now())
}

// recordAt is record for a verdict reached earlier than now.
func (t *Service) recordAt(ctx context.Context, v verdict, state db.ThumbState, at time.Time) {
	if v.file == 0 {
		return
	}
	err := t.meta.PutThumbVerdict(ctx, db.ThumbVerdict{
		FileID: v.file, State: state, BlobKey: v.blobKey,
		Generation: files.DerivedGeneration, At: at,
	})
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		slog.WarnContext(ctx, "recording what became of a thumbnail", "file", v.file, "err", err)
	}
}

// reduceTo decodes, reduces and encodes, which is the same path whether the
// pixels came from a file of their own or out of a tag.
//
// JPEG for everything, including a PNG cover: a thumbnail is a photograph of
// something most of the time, transparency is not meaningful at this size, and
// one output format means one path through every client.
func reduceTo(r io.ReadSeeker, name string, size thumbSize) ([]byte, error) {
	pic, err := decodeInGo(r, name)
	if err != nil {
		return nil, err
	}
	return pic.at(size)
}

// decoded is a picture and what is still to be done to it, which is what lets
// one decode answer for more than one size.
type decoded struct {
	src  image.Image
	name string
	// rotation is the EXIF turn Go's decoder does not apply, and zero for a
	// frame ffmpeg has already straightened.
	rotation int
}

// at fits the picture in a box of size and writes the JPEG.
func (d decoded) at(size thumbSize) ([]byte, error) {
	// Reduced first and turned after: a quarter turn of a thumbnail is a
	// hundredth of the work of turning twelve megapixels, and it cannot fall out
	// of the box either -- a quarter turn swaps the two sides and both of them
	// already fit.
	return writeJPEG(orient(reduce(d.src, int(size)), d.rotation), d.name)
}

// decodeInGo is the half of the decode this binary does itself.
func decodeInGo(r io.ReadSeeker, name string) (decoded, error) {
	// Before the decode, because the decoder does not look: a photograph taken
	// with the camera turned says so in its EXIF and image.Decode hands back the
	// pixels as they were stored, on their side.
	rotation := orientationOf(r)

	src, _, err := image.Decode(io.LimitReader(r, maxThumbSource))
	if err != nil {
		return decoded{}, fmt.Errorf("%w: decoding %q: %w", ErrNoThumbnail, name, err)
	}
	return decoded{src: src, name: name, rotation: rotation}, nil
}

// orientationOf reads the EXIF rotation and leaves the reader where it found
// it.
//
// Best effort by design: a PNG has no EXIF, a JPEG from a scanner may have none
// either, and a malformed tag is not worth failing a picture over. Zero means
// "as it is", which is also what the ordinary photograph says.
func orientationOf(r io.ReadSeeker) int {
	defer func() { _, _ = r.Seek(0, io.SeekStart) }()

	exif, err := imagemeta.Decode(r)
	if err != nil {
		return 0
	}
	return int(exif.IFD0.Orientation)
}

// writeJPEG is the one encoder every thumbnail goes through, whatever made the
// pixels and whichever of the two paths fitted them in the box.
func writeJPEG(img image.Image, name string) ([]byte, error) {
	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: thumbQuality}); err != nil {
		return nil, fmt.Errorf("encode a thumbnail of %q: %w", name, err)
	}
	return out.Bytes(), nil
}

// orient turns a picture the way its EXIF says it was taken.
//
// The eight values are the identity, three quarter turns and the four mirrored
// versions of those. The mirrors are rare and cost a line each once the
// coordinate map exists, and leaving them out would be a surprise waiting for
// one particular photograph.
//
// Mapped pixel by pixel rather than through a transform library: at thumbnail
// sizes this is at most a megapixel and a half, it runs once per picture per
// size, and the alternative is a dependency for four lines of arithmetic.
func orient(src image.Image, orientation int) image.Image {
	if orientation <= 1 || orientation > 8 {
		return src
	}

	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	// Five through eight are the ones that put the picture on its side, so the
	// destination is as tall as the source is wide.
	turned := orientation >= 5

	out := image.NewRGBA(image.Rect(0, 0, pick(turned, h, w), pick(turned, w, h)))
	for y := range h {
		for x := range w {
			dx, dy := orientedAt(orientation, x, y, w, h)
			out.Set(dx, dy, src.At(b.Min.X+x, b.Min.Y+y))
		}
	}
	return out
}

// orientedAt is where the pixel at (x, y) of the stored image belongs once the
// picture is the right way up. The numbers are EXIF's, in its order.
func orientedAt(orientation, x, y, w, h int) (int, int) {
	switch orientation {
	case 2: // mirrored
		return w - 1 - x, y
	case 3: // upside down
		return w - 1 - x, h - 1 - y
	case 4: // mirrored, upside down
		return x, h - 1 - y
	case 5: // mirrored, a quarter turn anticlockwise
		return y, x
	case 6: // a quarter turn clockwise
		return h - 1 - y, x
	case 7: // mirrored, a quarter turn clockwise
		return h - 1 - y, w - 1 - x
	case 8: // a quarter turn anticlockwise
		return y, w - 1 - x
	default:
		return x, y
	}
}

func pick(when bool, yes, no int) int {
	if when {
		return yes
	}
	return no
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
		if f.IsDir || media.KindFromExtension(path.Ext(f.Path)) != db.KindAudio {
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
// **Changing what comes out of this package means raising
// files.DerivedGeneration.** The key carries it, so pictures made by an older
// generator stop being served and are collected on the next sweep; without
// that they stay as they were made, which is what happened to every portrait
// photograph thumbnailed before #148 and every black first frame before #149.
// TestTheGeneratorOutputHasNotMoved is the reminder, and it fails on the day
// it matters rather than a release later.
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

// verdictKey and coverVerdictKey are where a refusal is remembered (#338).
//
// One per blob and not one per size, because what failed is the decode and
// every size shares it: a file nothing can read is unreadable at 96 pixels and
// at 1200, and three notes would mean three attempts to learn it.
//
// They are **caches** in the sweep's sense -- `files.CachePrefix` -- so the
// sweep collects one a week after it was written even while its parent lives.
// That is the only honest answer to the thing a verdict cannot know: whether
// the build that judged the file is still this build. The list of formats
// ffmpeg was compiled for is not in the key and could not be, so a recipe that
// gains a decoder would otherwise leave every file that needed it refused
// until somebody raised the generation and regenerated a whole library of
// pictures that were never wrong.
//
// A `.txt` because that is what it is: the reason, in words, for whoever is
// reading a data directory rather than for any code here.
func verdictKey(blobKey string) string {
	return files.DerivedKey(blobKey, files.CachePrefix+"none.txt")
}

func coverVerdictKey(blobKey string) string {
	return files.DerivedKey(blobKey, files.CachePrefix+"cover-none.txt")
}

// CanThumbnail reports whether this build can turn the file at p into pixels.
//
// Exported so that a page deciding whether to render an image asks the same
// question the generator will answer, rather than keeping a second list of
// extensions that drifts from this one. It is a property of the build and not
// of the file, which is why it is computed here and stored nowhere: the day the
// ffmpeg path is wired, every HEIC changes its answer without a byte moving.
func CanThumbnail(p string, size int64) bool {
	switch {
	case decodableInGo(p):
		// Decoded from the stream, so the bound is what will be held in memory
		// rather than what will be copied.
		return size <= maxThumbSource
	case !decodableByFFmpeg(p):
		return false
	case media.Windowable(p):
		// A film this size is not copied, but it can be looked at through a
		// media.Window: its headers and its first frames, which is a few megabytes
		// whatever the film weighs (#149).
		return true
	default:
		// AVI, WMV and MPEG-TS, where there is no media.Window to take -- the same
		// three the duration was refused to for the same reason.
		return size <= media.MaxSpool
	}
}

// decodableInGo reports whether this build can read the file without ffmpeg.
//
// Only what the standard library decodes today. WebP and TIFF wait for the
// x/image decoders -- the point of the list is that a format is either read
// here or refused honestly, never read badly.
func decodableInGo(p string) bool {
	switch strings.ToLower(path.Ext(p)) {
	case ".jpg", ".jpeg", ".png":
		return true
	}
	return false
}

// decodableByFFmpeg is the rest: what the binary in this image was built to
// decode, which is HEIC and the video containers.
//
// **This list mirrors the decoders in build/ffmpeg/Dockerfile**, the same way
// byExtension mirrors the demuxers for ffprobe, and it drifts the same way: an
// extension added here without its decoder there is not a build failure, it is
// a broken thumbnail in production for that format only.
//
// .avif is missing on purpose -- AV1 is out of that build -- and so is camera
// raw, which needs the preview embedded in the file rather than a decode.
func decodableByFFmpeg(p string) bool {
	switch strings.ToLower(path.Ext(p)) {
	case ".heic", ".heif":
		return true
	case ".mp4", ".mov", ".m4v", ".mkv", ".webm", ".avi", ".mpg", ".mpeg", ".wmv":
		return true
	case ".mts", ".m2ts", ".m2t":
		// AVCHD and its relatives, which the recipe learned to demux for this
		// (#146). Not ".ts": the same bytes go by a name TypeScript also uses,
		// and this answer is a function of the name alone.
		return true
	}
	return false
}
