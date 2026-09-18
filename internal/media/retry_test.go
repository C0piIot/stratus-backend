package media

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/db/sqlite"
	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/storage"
	"github.com/C0piIot/stratus-backend/internal/storage/disk"
	"github.com/C0piIot/stratus-backend/internal/storage/storagetest"
)

// What this file is about: the difference between "this file cannot be read"
// and "this file could not be reached", which used to be written down the same
// way (#157).

// TestAStoreThatWillNotAnswerIsNotAVerdict is the case the issue was filed for.
// A bucket that times out for thirty seconds marked every file it touched as
// unreadable for good; now it costs them an hour.
func TestAStoreThatWillNotAnswerIsNotAVerdict(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	blobs, meta := backends(t, dir)
	f := write(t, files.New(blobs, meta), "photos/IMG_0001.jpg", exifJPEG(t), "image/jpeg")

	broken := over(t, dir, storagetest.FailOn(t, blobs, "Get"), meta)
	n, err := broken.IndexBatch(t.Context(), time.Now())
	if err != nil {
		t.Fatalf("IndexBatch: %v", err)
	}
	// Nothing was decided, so the loop in internal/app waits rather than coming
	// straight back for another batch against a store that is not answering.
	if n != 0 {
		t.Errorf("IndexBatch counted %d files it never read", n)
	}

	got, err := meta.MediaByFile(t.Context(), f.ID)
	if err != nil {
		t.Fatalf("MediaByFile: %v", err)
	}
	if got.RetryAt.IsZero() {
		t.Fatalf("the row is a verdict: %+v", got)
	}
	if !strings.Contains(got.Error, "could not be read") {
		t.Errorf("error = %q, want it marked as a failure to reach the file", got.Error)
	}

	// The row exists so that the queue can move past it, and the time on it is
	// what brings the file back.
	pending, err := meta.PendingMedia(t.Context(), Version, time.Now(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Errorf("the file is queued again immediately: %+v", pending)
	}
	if later, lerr := meta.PendingMedia(t.Context(), Version, got.RetryAt.Add(time.Minute), 10); lerr != nil || len(later) != 1 {
		t.Fatalf("PendingMedia after the time = %+v, %v", later, lerr)
	}

	// And when the time comes round a working store finishes the job, which is
	// the whole point: nothing had to raise the extractor version to get here.
	healthy := over(t, dir, blobs, meta)
	switch n, ierr := healthy.IndexBatch(t.Context(), got.RetryAt.Add(time.Minute)); {
	case ierr != nil || n != 1:
		t.Fatalf("the pass after the wait indexed %d files, %v", n, ierr)
	}
	got, err = meta.MediaByFile(t.Context(), f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Indexed() || !got.RetryAt.IsZero() || got.Camera != "Apple iPhone 15 Pro" {
		t.Errorf("got %+v, want the metadata and no time left on the row", got)
	}
}

// TestAMissingBlobIsAVerdict is the one answer from the store that is about the
// file: a row pointing at an object nobody has is corruption, and saying so is
// worth more than trying again every hour forever.
func TestAMissingBlobIsAVerdict(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	blobs, meta := backends(t, dir)
	service := files.New(blobs, meta)

	gone := write(t, service, "photos/lost.jpg", exifJPEG(t), "image/jpeg")
	if err := blobs.Delete(t.Context(), gone.BlobKey); err != nil {
		t.Fatal(err)
	}

	// On its own, which is the case the guard below must not swallow: a library
	// with one orphan row in it is a batch where every blob is missing.
	indexer := over(t, dir, blobs, meta)
	if n, err := indexer.IndexBatch(t.Context(), time.Now()); err != nil || n != 1 {
		t.Fatalf("IndexBatch = %d, %v", n, err)
	}

	got, err := meta.MediaByFile(t.Context(), gone.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.RetryAt.IsZero() {
		t.Errorf("a blob nobody has is being waited for: %+v", got)
	}
	if got.Error == "" {
		t.Error("nothing was recorded about a file whose blob is gone")
	}
	if pending, perr := meta.PendingMedia(t.Context(), Version, time.Now(), 10); perr != nil || len(pending) != 0 {
		t.Errorf("PendingMedia = %+v, %v, want the verdict to be final", pending, perr)
	}
}

// TestEveryBlobMissingIsAStoreAndNotALibrary bounds the decision above. A
// database pointed at an empty bucket would otherwise write "your blob is gone"
// across every file at once, which is the disaster files.Collect refuses with
// ErrEmptyIndex for the same reason.
func TestEveryBlobMissingIsAStoreAndNotALibrary(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	blobs, meta := backends(t, dir)
	service := files.New(blobs, meta)

	var written []db.File
	for _, name := range []string{"photos/one.jpg", "photos/two.jpg"} {
		f := write(t, service, name, exifJPEG(t), "image/jpeg")
		written = append(written, f)
		if err := blobs.Delete(t.Context(), f.BlobKey); err != nil {
			t.Fatal(err)
		}
	}

	indexer := over(t, dir, blobs, meta)
	if n, err := indexer.IndexBatch(t.Context(), time.Now()); err != nil || n != 0 {
		t.Fatalf("IndexBatch = %d, %v, want nothing decided", n, err)
	}
	for _, f := range written {
		got, err := meta.MediaByFile(t.Context(), f.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.RetryAt.IsZero() {
			t.Errorf("%s was judged on the word of a store that has nothing in it: %+v", f.Path, got)
		}
	}
}

// TestNowhereToSpoolIsNotAVerdict is the other side of the machine failing: a
// full disk, or a data directory that went away, says nothing about the file
// that happened to be next in the queue.
func TestNowhereToSpoolIsNotAVerdict(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	blobs, meta := backends(t, dir)
	// An AVI, because no reader here claims one: it is the path that needs a
	// local copy, which is the path that needs somewhere to put it.
	f := write(t, files.New(blobs, meta), "films/holiday.avi", []byte("RIFF\x24\x00\x00\x00AVI LIST"), "video/avi")

	indexer := NewIndexer(files.New(blobs, meta), meta, filepath.Join(dir, "not", "there"), "ffprobe-does-not-exist")
	if n, err := indexer.IndexBatch(t.Context(), time.Now()); err != nil || n != 0 {
		t.Fatalf("IndexBatch = %d, %v", n, err)
	}

	got, err := meta.MediaByFile(t.Context(), f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RetryAt.IsZero() {
		t.Errorf("a machine that had nowhere to write became a verdict on the film: %+v", got)
	}
}

// TestAKilledProbeIsNotAVerdict: the out-of-memory killer and the timeout both
// arrive as a process that was signalled, and neither of them read the file.
func TestAKilledProbeIsNotAVerdict(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	blobs, meta := backends(t, dir)
	f := write(t, files.New(blobs, meta), "films/holiday.avi", []byte("RIFF\x24\x00\x00\x00AVI LIST"), "video/avi")

	killed := filepath.Join(t.TempDir(), "ffprobe")
	if err := os.WriteFile(killed, []byte("#!/bin/sh\nkill -9 $$\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	tmp, terr := TempDir(dir)
	if terr != nil {
		t.Fatal(terr)
	}

	indexer := NewIndexer(files.New(blobs, meta), meta, tmp, killed)
	if n, err := indexer.IndexBatch(t.Context(), time.Now()); err != nil || n != 0 {
		t.Fatalf("IndexBatch = %d, %v", n, err)
	}

	got, err := meta.MediaByFile(t.Context(), f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RetryAt.IsZero() {
		t.Errorf("a killed process became a verdict on the film: %+v", got)
	}
}

// TestAFileNobodyCanParseIsStillFinal is the behaviour none of the above may
// break: the reason the row is written at all is that one corrupt file must not
// be re-read on every pass forever.
func TestAFileNobodyCanParseIsStillFinal(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	blobs, meta := backends(t, dir)
	f := write(t, files.New(blobs, meta), "photos/broken.jpg", []byte("\xff\xd8\xff not really a photograph"), "image/jpeg")

	indexer := over(t, dir, blobs, meta)
	if n, err := indexer.IndexBatch(t.Context(), time.Now()); err != nil || n != 1 {
		t.Fatalf("IndexBatch = %d, %v", n, err)
	}

	got, err := meta.MediaByFile(t.Context(), f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Error == "" || !got.RetryAt.IsZero() {
		t.Errorf("got %+v, want a failure with no time on it", got)
	}
	if pending, perr := meta.PendingMedia(t.Context(), Version, time.Now(), 10); perr != nil || len(pending) != 0 {
		t.Errorf("PendingMedia = %+v, %v", pending, perr)
	}
}

// backends builds the two real ones under dir, so that a case can keep hold of
// the store and break it between passes.
func backends(t *testing.T, dir string) (storage.Storage, db.Store) {
	t.Helper()

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
	return blobs, meta
}

// over wires an indexer onto a store that may or may not be the one the files
// were written through.
func over(t *testing.T, dir string, blobs storage.Storage, meta db.Store) *Indexer {
	t.Helper()
	tmp, err := TempDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return NewIndexer(files.New(blobs, meta), meta, tmp, "ffprobe-does-not-exist")
}
