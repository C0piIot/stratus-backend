package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/sniff"
)

// BatchSize is how many files one pass looks at. Small enough that a first run
// over a large library reports progress and can be interrupted, large enough
// that the query is not the cost.
const BatchSize = 64

// Indexer fills in the metadata of files that have none.
type Indexer struct {
	files   *files.Service
	meta    db.MediaIndex
	tmpDir  string
	ffprobe string
	// wake carries no value and holds at most one: it says "do not wait for the
	// timer", which is all a caller can usefully tell this.
	wake chan struct{}
}

// LookupFFprobe finds ffprobe, which is a hard requirement rather than an
// optional extra: without it there is no duration for a track and no dimensions
// for a video, and half a media library is worse than an honest refusal to
// start.
func LookupFFprobe() (string, error) {
	path, err := exec.LookPath("ffprobe")
	if err != nil {
		return "", fmt.Errorf("ffprobe is required for media indexing: %w", err)
	}
	return path, nil
}

// LookupFFmpeg finds the other one, and it is required for the same reason and
// one more: whether a file can have a thumbnail is answered by CanThumbnail, a
// function of the name alone. If the binary's absence changed that answer, the
// listing would have to ask the generator instead of knowing, and a build would
// offer pictures it cannot make.
func LookupFFmpeg() (string, error) {
	path, err := exec.LookPath("ffmpeg")
	if err != nil {
		return "", fmt.Errorf("ffmpeg is required for thumbnails of what Go cannot decode: %w", err)
	}
	return path, nil
}

// NewIndexer wires an indexer. tmpDir is where blobs are spooled for ffprobe,
// and ffprobe is its path, from LookupFFprobe.
func NewIndexer(f *files.Service, meta db.MediaIndex, tmpDir, ffprobe string) *Indexer {
	return &Indexer{files: f, meta: meta, tmpDir: tmpDir, ffprobe: ffprobe, wake: make(chan struct{}, 1)}
}

// Notice says a file has just been written. It is what internal/files calls
// through its watcher, and what turns "indexed within a minute" into "indexed
// now".
//
// The file it names is deliberately ignored: what runs next is the ordinary
// batch over the ordinary query, so there is one path that indexes anything and
// no second one that could disagree with it. That also makes five hundred
// uploads one wake-up rather than five hundred, since the channel holds one.
//
// Never blocks, and never fails. A notice that is dropped -- because a pass is
// already about to run, or because this process dies before it does -- costs
// the file a wait for the next tick, which is exactly where it was before.
func (i *Indexer) Notice(db.File) {
	select {
	case i.wake <- struct{}{}:
	default:
	}
}

// Woken is closed-over by the composition root's loop: it waits on this as well
// as on its timer.
func (i *Indexer) Woken() <-chan struct{} { return i.wake }

// IndexBatch extracts metadata for up to BatchSize files and returns how many
// it wrote. A full batch means there is probably more to do.
func (i *Indexer) IndexBatch(ctx context.Context) (int, error) {
	pending, err := i.meta.PendingMedia(ctx, Version, BatchSize)
	if err != nil {
		return 0, fmt.Errorf("find files to index: %w", err)
	}

	for n, f := range pending {
		if err := ctx.Err(); err != nil {
			return n, err
		}
		if err := i.meta.PutMedia(ctx, i.index(ctx, f)); err != nil {
			return n, fmt.Errorf("store the metadata of %q: %w", f.Path, err)
		}
	}
	return len(pending), nil
}

// index never fails: a file it cannot read gets a row saying why, because the
// alternative is reading it again on every pass for the rest of time.
func (i *Indexer) index(ctx context.Context, f db.File) db.Media {
	m, err := i.extract(ctx, f)
	m.FileID = f.ID
	m.IndexedAt = time.Now()
	m.Version = Version
	// Which bytes this describes. A file replaced later keeps this row and its
	// id, and the queue compares the two validators to notice.
	m.ETag = f.ETag
	if err != nil {
		if m.Kind == "" {
			// The name, because a file that could not be read could not be
			// classified from its bytes either -- that is often the same
			// failure. An extractor that got as far as knowing what it was
			// looking at says so itself.
			m.Kind = kindOf(f, db.KindOther)
		}
		m.Error = err.Error()
	}
	return m
}

func (i *Indexer) extract(ctx context.Context, f db.File) (db.Media, error) {
	body, err := i.files.OpenFile(ctx, f)
	if err != nil {
		return db.Media{}, err
	}
	defer func() { _ = body.Close() }()

	kind, err := i.classify(body, f)
	if err != nil {
		return db.Media{}, err
	}

	switch kind {
	case db.KindImage:
		return extractImage(body)

	case db.KindAudio, db.KindVideo:
		// A container that states what it is near one end is read where it
		// lies, because the store reads ranges: an MP4 or a QuickTime file
		// through its boxes, a Matroska or a WebM through its elements.
		if kind == db.KindVideo && readableInPlace(f.Path) {
			if m, perr := i.probeInPlace(ctx, f); perr == nil {
				return m, nil
			}
		}

		// Everything else needs a local copy, and that is where the line is:
		// nothing large is downloaded to be looked at. What a file does not say
		// in its head stays unsaid.
		if f.Size > maxSpool {
			return db.Media{Kind: kind}, errTooLargeToRead
		}

		path, cleanup, err := i.spoolFile(ctx, f)
		if err != nil {
			return db.Media{}, err
		}
		defer cleanup()

		report, err := runProbe(ctx, i.ffprobe, path)
		if err != nil {
			return db.Media{}, err
		}
		return report.mediaFrom(kind), nil

	default:
		return db.Media{Kind: db.KindOther}, nil
	}
}

// classify reads the head of the file and asks what it is, leaving the reader
// back at the beginning for whichever extractor gets it.
//
// One ranged read per file, where the kinds that extract nothing used to cost
// none at all -- and a second range for an image, since rewinding reopens the
// body on a bucket. It is paid once per file while its bytes do not change,
// which the queue now knows (#143), and it buys the two answers a name cannot
// give: what a camcorder recording is, and what a file with no extension is.
func (i *Indexer) classify(body io.ReadSeeker, f db.File) (db.Kind, error) {
	head := make([]byte, sniff.HeadSize)
	n, err := io.ReadFull(body, head)
	// A file shorter than the window is ordinary, and an empty one is a file
	// too: neither is a failure, and both are classified from what there is.
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return db.KindOther, fmt.Errorf("read the head of %q: %w", f.Path, err)
	}
	if _, err := body.Seek(0, io.SeekStart); err != nil {
		return db.KindOther, fmt.Errorf("rewind %q: %w", f.Path, err)
	}

	_, sniffed := sniff.Sniff(head[:n])
	return kindOf(f, sniffed), nil
}

// probeInPlace reads the metadata out of the blob itself, over ranges, without
// a local copy of any kind.
func (i *Indexer) probeInPlace(ctx context.Context, f db.File) (db.Media, error) {
	body, err := i.files.OpenFile(ctx, f)
	if err != nil {
		return db.Media{}, err
	}
	defer func() { _ = body.Close() }()

	if matroska(f.Path) {
		return probeMatroska(body, f.Size)
	}
	return probeVideo(body, f.Size)
}

// readableInPlace reports whether one of the readers here claims the file.
func readableInPlace(name string) bool { return isobmff(name) || matroska(name) }

// spool copies a blob to a local file, because the tools seek and the storage
// port streams.
//
// For a large video on S3 this downloads the whole thing once. That is the
// honest cost of the abstraction: #48 removed it for probing an MP4, which is
// read out of its own container over ranges, and it stays for everything a
// binary has to open -- every other container, and every frame turned into a
// thumbnail.
//
// No context here: what makes it cancellable is the reader, which is a ranged
// read off the store carrying the caller's.
func spool(body io.Reader, dir, name string) (string, func(), error) {
	tmp, err := os.CreateTemp(dir, "spool-*")
	if err != nil {
		return "", nil, fmt.Errorf("spool %q: %w", name, err)
	}
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}

	if _, err := io.Copy(tmp, body); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("spool %q: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("spool %q: %w", name, err)
	}
	return tmp.Name(), func() { _ = os.Remove(tmp.Name()) }, nil
}

// spoolFile is spool over a file in the tree, which is how both callers reach
// it: read the blob, write it down, hand the path to a binary.
func (i *Indexer) spoolFile(ctx context.Context, f db.File) (string, func(), error) {
	body, err := i.files.OpenFile(ctx, f)
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = body.Close() }()

	return spool(body, i.tmpDir, f.Path)
}

// TempDir is where spooled blobs go -- for the indexer and for the thumbnail
// generator alike -- under the data directory so that a multi-gigabyte video
// does not land on whatever /tmp happens to be.
func TempDir(dataDir string) (string, error) {
	dir := filepath.Join(dataDir, ".index")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	// Anything still in there is from a process that is no longer running, the
	// same argument the blob store makes about its own reserved directory.
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", dir, err)
	}
	for _, e := range entries {
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("remove %s: %w", e.Name(), err)
		}
	}
	return dir, nil
}
