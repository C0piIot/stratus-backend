package files_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/storage"
)

// blobCount is what the store actually holds, which is the only number that
// matters here: the database's opinion is the input, not the answer.
func blobCount(t *testing.T, blobs storage.Storage) int {
	t.Helper()
	var n int
	for _, err := range blobs.List(t.Context(), "") {
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		n++
	}
	return n
}

// TestCollectTakesTheOverwrittenBlob is the leak this exists for: a fresh key
// on every write means the previous blob is left behind, on purpose, so that a
// failed overwrite cannot destroy what it was replacing.
func TestCollectTakesTheOverwrittenBlob(t *testing.T) {
	t.Parallel()
	s, blobs := service(t)

	first := write(t, s, "notes.txt", "version one")
	second := write(t, s, "notes.txt", "version two")
	if blobCount(t, blobs) != 2 {
		t.Fatal("the overwrite did not leave the previous blob behind, so this test is testing nothing")
	}

	// No grace at all, because the blobs were written a moment ago.
	done, err := s.Collect(t.Context(), 0)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if done.Deleted != 1 || done.Scanned != 2 {
		t.Errorf("Collect = %+v, want one of two deleted", done)
	}
	if done.Bytes != int64(len("version one")) {
		t.Errorf("Bytes = %d, want the size of the orphan", done.Bytes)
	}

	// The live file still reads, which is the difference between collecting
	// garbage and losing data.
	if got := read(t, s, "notes.txt"); got != "version two" {
		t.Errorf("the live file reads %q", got)
	}
	if _, err := blobs.Stat(t.Context(), second.BlobKey); err != nil {
		t.Errorf("the live blob was collected: %v", err)
	}
	if _, err := blobs.Stat(t.Context(), first.BlobKey); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("the orphan survived: %v", err)
	}
}

// TestCollectRespectsTheGrace is the rule that keeps this from deleting uploads
// in flight: a write puts the blob down before the row, so a blob with no row
// may simply not have finished.
func TestCollectRespectsTheGrace(t *testing.T) {
	t.Parallel()
	s, blobs := service(t)
	write(t, s, "notes.txt", "one")
	write(t, s, "notes.txt", "two")

	done, err := s.Collect(t.Context(), time.Hour)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if done.Deleted != 0 {
		t.Errorf("Collect deleted %d blobs written seconds ago", done.Deleted)
	}
	if blobCount(t, blobs) != 2 {
		t.Error("something was deleted despite the grace period")
	}
}

func TestCollectLeavesALiveLibraryAlone(t *testing.T) {
	t.Parallel()
	s, blobs := service(t)
	if _, err := s.Mkdir(t.Context(), owner, "album"); err != nil {
		t.Fatal(err)
	}
	write(t, s, "album/one.jpg", "one")
	write(t, s, "album/two.jpg", "two")
	write(t, s, "top.txt", "top")

	done, err := s.Collect(t.Context(), 0)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if done.Deleted != 0 {
		t.Errorf("Collect deleted %d blobs from a library with no garbage in it", done.Deleted)
	}
	if got := blobCount(t, blobs); got != 3 {
		t.Errorf("the store holds %d blobs, want 3", got)
	}
	// A directory has no blob, so it must not make the collector look for one.
	if done.Scanned != 3 {
		t.Errorf("Scanned = %d, want 3", done.Scanned)
	}
}

func TestCollectAfterARecursiveDelete(t *testing.T) {
	t.Parallel()
	s, blobs := service(t)
	if _, err := s.Mkdir(t.Context(), owner, "album"); err != nil {
		t.Fatal(err)
	}
	write(t, s, "album/one.jpg", "one")
	write(t, s, "keep.txt", "keep")

	if err := s.Remove(t.Context(), owner, "album"); err != nil {
		t.Fatal(err)
	}
	// Remove deletes the blobs itself, so there should be nothing left over.
	done, err := s.Collect(t.Context(), 0)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if done.Deleted != 0 {
		t.Errorf("Collect found %d orphans after a delete that cleans up after itself", done.Deleted)
	}
	if blobCount(t, blobs) != 1 {
		t.Error("the surviving file lost its blob")
	}
}

// TestCollectRefusesAnEmptyIndex is the guard against the worst possible
// outcome: a database pointed at a fresh file, a store full of somebody's
// photos, and a sweep that concludes none of them are referenced.
func TestCollectRefusesAnEmptyIndex(t *testing.T) {
	t.Parallel()
	s, blobs := service(t)
	write(t, s, "photo.jpg", "irreplaceable")
	if err := s.Remove(t.Context(), owner, "photo.jpg"); err != nil {
		t.Fatal(err)
	}
	// Put an object back with no row pointing at it, which is what a store
	// looks like next to a database that knows nothing about it.
	if _, err := blobs.Put(t.Context(), "blobs/AA/BB/orphan", strings.NewReader("irreplaceable"), -1); err != nil {
		t.Fatal(err)
	}

	_, err := s.Collect(t.Context(), 0)
	if !errors.Is(err, files.ErrEmptyIndex) {
		t.Fatalf("Collect = %v, want ErrEmptyIndex", err)
	}
	if blobCount(t, blobs) != 1 {
		t.Error("it deleted something anyway")
	}
}
