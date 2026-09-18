package media

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/storage/storagetest"
)

// TestIndexFileAsksNothing is the whole claim of #158: a file a write named is
// read without the query that used to be the only way to find it, and that
// query is a scan of every file row.
func TestIndexFileAsksNothing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	blobs, meta := backends(t, dir)
	counted := &countingIndex{MediaIndex: meta}
	f := write(t, files.New(blobs, meta), "photos/IMG_0001.jpg", exifJPEG(t), "image/jpeg")

	indexer := NewIndexer(files.New(blobs, meta), counted, mustTempDir(t, dir), "ffprobe-does-not-exist")
	if err := indexer.IndexFile(t.Context(), time.Now(), f); err != nil {
		t.Fatalf("IndexFile: %v", err)
	}

	if counted.asked != 0 {
		t.Errorf("the database was asked what to index %d times, want none", counted.asked)
	}

	got, err := meta.MediaByFile(t.Context(), f.ID)
	if err != nil {
		t.Fatalf("MediaByFile: %v", err)
	}
	if !got.Indexed() || got.Camera != "Apple iPhone 15 Pro" || got.Version != Version {
		t.Errorf("got %+v, want the same row a batch would have written", got)
	}

	// And the queue agrees it is done, so the safety net will not do it again.
	if pending, perr := meta.PendingMedia(t.Context(), Version, time.Now(), 10); perr != nil || len(pending) != 0 {
		t.Errorf("PendingMedia = %+v, %v", pending, perr)
	}
}

// TestIndexFileOfSomethingDeleted is the failure the direct path introduces:
// upload and delete in quick succession leaves a notice naming a row that is
// gone, and the metadata has nowhere to go. That is a shrug and not an error.
func TestIndexFileOfSomethingDeleted(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	blobs, meta := backends(t, dir)
	service := files.New(blobs, meta)
	f := write(t, service, "photos/fleeting.jpg", exifJPEG(t), "image/jpeg")
	if err := service.Remove(t.Context(), owner, "photos/fleeting.jpg"); err != nil {
		t.Fatal(err)
	}

	indexer := over(t, dir, blobs, meta)
	if err := indexer.IndexFile(t.Context(), time.Now(), f); err != nil {
		t.Errorf("IndexFile of a deleted file = %v, want nothing to report", err)
	}
	if _, err := meta.MediaByFile(t.Context(), f.ID); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("MediaByFile = %v, want no row for a file that is gone", err)
	}
}

// TestIndexFileDefersWhatItCannotReach: the rule from #157 is the extractor's
// and not the batch's, so it holds on this path too.
func TestIndexFileDefersWhatItCannotReach(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	blobs, meta := backends(t, dir)
	f := write(t, files.New(blobs, meta), "photos/IMG_0001.jpg", exifJPEG(t), "image/jpeg")

	broken := storagetest.FailOn(t, blobs, "Get")
	indexer := NewIndexer(files.New(broken, meta), meta, mustTempDir(t, dir), "ffprobe-does-not-exist")
	if err := indexer.IndexFile(t.Context(), time.Now(), f); err != nil {
		t.Fatalf("IndexFile: %v", err)
	}

	got, err := meta.MediaByFile(t.Context(), f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RetryAt.IsZero() {
		t.Errorf("a store that would not answer became a verdict: %+v", got)
	}
}

// countingIndex counts the one call this issue is about.
type countingIndex struct {
	db.MediaIndex
	asked int
}

func (c *countingIndex) PendingMedia(ctx context.Context, version int, now time.Time, limit int) ([]db.File, error) {
	c.asked++
	return c.MediaIndex.PendingMedia(ctx, version, now, limit)
}

func mustTempDir(t *testing.T, dir string) string {
	t.Helper()
	tmp, err := TempDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return tmp
}
