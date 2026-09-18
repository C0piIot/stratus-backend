package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/sniff"
	"github.com/C0piIot/stratus-backend/internal/storage"
)

// BatchSize is how many files one pass looks at. Small enough that a first run
// over a large library reports progress and can be interrupted, large enough
// that the query is not the cost.
const BatchSize = 64

// noticeBuffer is how many written files are held for the indexer to get to.
// Small on purpose: what makes a small buffer safe is that overflowing it is
// reported rather than swallowed, and then the query finds what was dropped.
const noticeBuffer = 256

// Indexer fills in the metadata of files that have none.
type Indexer struct {
	files   *files.Service
	meta    db.MediaIndex
	tmpDir  string
	ffprobe string
	// notices are the files writes have handed over, which is the ordinary way
	// work arrives and costs no query at all.
	notices chan db.File
	// lost holds at most one and says "there was a write I could not hold on
	// to": the signal that the query has to run sooner than the interval.
	lost chan struct{}
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
	return &Indexer{
		files: f, meta: meta, tmpDir: tmpDir, ffprobe: ffprobe,
		notices: make(chan db.File, noticeBuffer),
		lost:    make(chan struct{}, 1),
	}
}

// Notice says a file has just been written. It is what internal/files calls
// through its watcher, and it carries the file because we already know which
// one it is: asking the database that question is a scan of every row, and it
// costs the same whether the answer is a file or nothing (#158).
//
// This is not a second extractor. IndexFile and IndexBatch reach the same one;
// what differs is how the file was found.
//
// Never blocks, and never fails, which is its only promise -- it is called from
// inside a write. A notice this cannot hold raises lost instead, so the query
// runs sooner rather than the file waiting for the interval; that is what makes
// a buffer of a few hundred safe rather than a number to tune.
func (i *Indexer) Notice(f db.File) {
	select {
	case i.notices <- f:
		return
	default:
	}
	select {
	case i.lost <- struct{}{}:
	default:
	}
}

// Noticed hands out the files writes have named. The composition root's loop
// waits on this, which is the ordinary way anything gets indexed.
func (i *Indexer) Noticed() <-chan db.File { return i.notices }

// LostTrack fires when a notice was dropped. What is on the other side of it is
// the query, brought forward: something was written and this does not know
// what.
func (i *Indexer) LostTrack() <-chan struct{} { return i.lost }

// IndexBatch extracts metadata for up to BatchSize files and returns how many
// it reached a verdict about. A full batch means there is probably more to do.
//
// A file deferred because the store would not answer does not count, which is
// the brake: the loop in internal/app only comes straight back for more when a
// pass filled its batch, so an unreachable store makes it wait for the interval
// rather than walk the whole library against something that is not there.
//
// now is the caller's, the way files.CollectUploads takes it: what a deferred
// file waits for is a time, and a clock this package read for itself would make
// that untestable without an hour to spare.
func (i *Indexer) IndexBatch(ctx context.Context, now time.Time) (int, error) {
	pending, err := i.meta.PendingMedia(ctx, Version, now, BatchSize)
	if err != nil {
		return 0, fmt.Errorf("find files to index: %w", err)
	}

	attempts := make([]attempt, 0, len(pending))
	for _, f := range pending {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		attempts = append(attempts, i.index(ctx, f))
	}

	var indexed, deferred int
	var why error
	for _, a := range attempts {
		if err := ctx.Err(); err != nil {
			return indexed, err
		}

		switch put, err := i.record(ctx, now, a, attempts); {
		case err != nil:
			return indexed, err
		case put:
			indexed++
		default:
			deferred++
			if why == nil {
				why = a.err
			}
		}
	}
	if deferred > 0 {
		slog.Warn("deferred indexing", "files", deferred, "retrying_in", retryAfter, "err", why)
	}
	return indexed, nil
}

// IndexFile extracts the metadata of one file a write has just named, without
// asking which files need it: that question is a scan of every row, and the
// answer was already in hand (#158).
//
// The same extractor and the same row as a batch, judged the same way. What it
// cannot do is the batch's reading of a whole failed batch at once, which is
// why a lone file whose blob is missing gets a verdict -- exactly what a batch
// of one already decides.
func (i *Indexer) IndexFile(ctx context.Context, now time.Time, f db.File) error {
	a := i.index(ctx, f)
	put, err := i.record(ctx, now, a, []attempt{a})
	switch {
	case err != nil:
		return err
	case !put:
		slog.Warn("deferred indexing", "file", a.path, "retrying_in", retryAfter, "err", a.err)
	}
	return nil
}

// record writes one attempt down and reports whether it is the last word on the
// file. A deferred one carries a time instead, and is still written: a file
// with no row at all would hold the head of the queue (#157).
//
// A row that is gone between the write and this is not a failure. It is
// somebody who uploaded a file and deleted it, and there is nothing to record
// about a file that does not exist.
func (i *Indexer) record(ctx context.Context, now time.Time, a attempt, batch []attempt) (bool, error) {
	m := a.media
	put := !a.deferred(batch)
	if !put {
		m.RetryAt = now.Add(retryAfter)
	}

	switch err := i.meta.PutMedia(ctx, m); {
	case errors.Is(err, db.ErrNotFound):
		return true, nil
	case err != nil:
		return false, fmt.Errorf("store the metadata of %q: %w", a.path, err)
	}
	return put, nil
}

// attempt is what one file's turn produced: the row to write, and the failure
// behind it so that the batch as a whole can be judged before anything is
// written down.
type attempt struct {
	media db.Media
	// path is kept beside the row for the error message, since db.Media carries
	// an id and not a name.
	path string
	err  error
}

// deferred reports whether this failure should carry a time rather than stand
// as the last word on the file.
//
// The store answering that the object is not there is the one failure from that
// side which is about the file: a row pointing at a blob nobody has is
// corruption, and saying so is more use than trying again every hour forever.
// Unless it is the answer for every file in the batch -- then it is a store
// pointed somewhere new rather than a library that rotted, which is the same
// judgement files.Collect makes before it deletes anything, and nothing is
// written down about any of them.
func (a attempt) deferred(batch []attempt) bool {
	if unreachable(a.err) {
		return true
	}
	if !errors.Is(a.err, storage.ErrNotFound) {
		return false
	}
	// One file on its own cannot tell a broken row from a store pointed
	// somewhere new, and the verdict is the more useful guess: a library with a
	// single orphan row in it would otherwise be retried every hour and never
	// say anything.
	if len(batch) < 2 {
		return false
	}
	for _, other := range batch {
		if !errors.Is(other.err, storage.ErrNotFound) {
			return false
		}
	}
	return true
}

// index never fails: a file it cannot read gets a row saying why, because the
// alternative is reading it again on every pass for the rest of time. Whether
// that row is the last word is the caller's to decide, and a.err is what it
// decides with.
func (i *Indexer) index(ctx context.Context, f db.File) attempt {
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
	return attempt{media: m, path: f.Path, err: err}
}

// extract reads the file once, through one reader.
//
// One open and not three: every extractor below seeks to where it wants to
// start, so the same body serves the sniff, the ranged probe and the spool --
// and a single reader is also what makes the failure classifiable, since it is
// the only thing that sees a store's error before a parser rephrases it.
func (i *Indexer) extract(ctx context.Context, f db.File) (db.Media, error) {
	body, err := i.files.OpenFile(ctx, f)
	if err != nil {
		return db.Media{}, err
	}
	defer func() { _ = body.Close() }()

	r := &storeReader{ReadSeekCloser: body}
	m, err := i.extractFrom(ctx, r, f)
	if err == nil || r.err == nil {
		return m, err
	}
	// The store failed on the way, whatever the error says now. That is not a
	// verdict on the file -- except the one answer from that side which is
	// about the file, and which the caller recognises for itself.
	if errors.Is(r.err, storage.ErrNotFound) {
		return m, r.err
	}
	return m, fmt.Errorf("%w: %w", errUnreachable, r.err)
}

func (i *Indexer) extractFrom(ctx context.Context, body io.ReadSeeker, f db.File) (db.Media, error) {
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
			if m, perr := probeInPlace(body, f); perr == nil {
				return m, nil
			}
		}

		// Everything else needs a local copy, and that is where the line is:
		// nothing large is downloaded to be looked at. What a file does not say
		// in its head stays unsaid.
		if f.Size > maxSpool {
			return db.Media{Kind: kind}, errTooLargeToRead
		}

		if _, err := body.Seek(0, io.SeekStart); err != nil {
			return db.Media{}, fmt.Errorf("rewind %q: %w", f.Path, err)
		}
		path, cleanup, err := spool(body, i.tmpDir, f.Path)
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
// a local copy of any kind. Both readers seek to the beginning themselves, so
// it takes the reader wherever the sniff left it.
func probeInPlace(body io.ReadSeeker, f db.File) (db.Media, error) {
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
// Its failures are marked unreachable: a full disk and a directory that is not
// there are facts about this machine, not about the recording.
func spool(body io.Reader, dir, name string) (string, func(), error) {
	tmp, err := os.CreateTemp(dir, "spool-*")
	if err != nil {
		return "", nil, fmt.Errorf("%w: spool %q: %w", errUnreachable, name, err)
	}
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}

	if _, err := io.Copy(tmp, body); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("%w: spool %q: %w", errUnreachable, name, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("%w: spool %q: %w", errUnreachable, name, err)
	}
	return tmp.Name(), func() { _ = os.Remove(tmp.Name()) }, nil
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
