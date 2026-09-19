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

// TestCollectKeepsDerivedObjectsAlive is the rule this sweep gained with
// thumbnails, and the reason it is not optional.
//
// A derived object has no row of its own and never will. Without the rule, every
// thumbnail would be swept an hour after it was made and the lazy path would
// generate it again -- forever. That failure has no symptom: nothing is wrong,
// nothing is missing, and the cost shows up in a CPU graph and an egress bill
// rather than in a log.
func TestCollectKeepsDerivedObjectsAlive(t *testing.T) {
	t.Parallel()
	s, blobs := service(t)

	live := write(t, s, "photo.jpg", "the original")
	derived := files.DerivedKey(live.BlobKey, "300.jpg")
	if _, err := blobs.Put(t.Context(), derived, strings.NewReader("a thumbnail"), -1); err != nil {
		t.Fatal(err)
	}

	// No grace at all, so age cannot be what saves it.
	done, err := s.Collect(t.Context(), 0)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if done.Deleted != 0 {
		t.Errorf("Collect deleted %d objects from a library with no garbage in it", done.Deleted)
	}
	if _, err := blobs.Stat(t.Context(), derived); err != nil {
		t.Errorf("the thumbnail of a live file was collected: %v", err)
	}
}

// TestCollectSweepsAnOlderGeneration is what makes files.DerivedGeneration mean
// anything (#161): a picture made by a generator that has moved on is garbage
// even though the file it was made from is right there.
//
// Without it, raising the generation would stop the old object being served and
// leave it on the disk for as long as its parent lived -- which is the
// objection that kept a derived key a pure function of its parent's, and the
// reason a thumbnail made before the EXIF fix stayed sideways for good.
func TestCollectSweepsAnOlderGeneration(t *testing.T) {
	t.Parallel()
	s, blobs := service(t)

	live := write(t, s, "photo.jpg", "the original")
	current := files.DerivedKey(live.BlobKey, "300.jpg")
	// Written by hand, because there is no way to ask DerivedKey for a key it
	// no longer writes -- which is the property being tested from the other
	// side. The second is what the very first thumbnails looked like, before
	// any of this existed.
	older := files.DerivedPrefix + live.BlobKey + "/g0-300.jpg"
	unstamped := files.DerivedPrefix + live.BlobKey + "/300.jpg"

	for _, key := range []string{current, older, unstamped} {
		if _, err := blobs.Put(t.Context(), key, strings.NewReader("a thumbnail"), -1); err != nil {
			t.Fatal(err)
		}
	}

	done, err := s.Collect(t.Context(), 0)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if done.Deleted != 2 {
		t.Errorf("Collect deleted %d objects, want the two an older generator made", done.Deleted)
	}
	if _, err := blobs.Stat(t.Context(), current); err != nil {
		t.Errorf("the picture this build makes was collected: %v", err)
	}
	for _, key := range []string{older, unstamped} {
		if _, err := blobs.Stat(t.Context(), key); err == nil {
			t.Errorf("%s survived, so raising the generation would only hide it", key)
		}
	}
}

// TestCollectSweepsDerivedObjectsWithTheirParent is the other half, and the
// case a key convention buys over a table: the derived key carries its
// parent's, so one rule collects both -- including after an overwrite, which
// leaves the old blob orphaned and its thumbnails filed under a key nothing
// will ever look for again.
func TestCollectSweepsDerivedObjectsWithTheirParent(t *testing.T) {
	t.Parallel()
	s, blobs := service(t)

	first := write(t, s, "photo.jpg", "version one")
	orphaned := files.DerivedKey(first.BlobKey, "300.jpg")
	if _, err := blobs.Put(t.Context(), orphaned, strings.NewReader("a thumbnail"), -1); err != nil {
		t.Fatal(err)
	}

	second := write(t, s, "photo.jpg", "version two")
	kept := files.DerivedKey(second.BlobKey, "300.jpg")
	if _, err := blobs.Put(t.Context(), kept, strings.NewReader("the new thumbnail"), -1); err != nil {
		t.Fatal(err)
	}

	done, err := s.Collect(t.Context(), 0)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	// The overwritten blob and its thumbnail, and nothing else.
	if done.Deleted != 2 {
		t.Errorf("Collect deleted %d objects, want the old blob and its thumbnail", done.Deleted)
	}
	if _, err := blobs.Stat(t.Context(), orphaned); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("the thumbnail of the overwritten blob survived: %v", err)
	}
	if _, err := blobs.Stat(t.Context(), kept); err != nil {
		t.Errorf("the live file's thumbnail was collected: %v", err)
	}
	if _, err := blobs.Stat(t.Context(), second.BlobKey); err != nil {
		t.Errorf("the live blob was collected: %v", err)
	}
}

// TestCollectLeavesAnUnparseableDerivedKeyAlone is the conservative half of the
// rule: an object directly under the prefix names no parent, so nothing can say
// whether it is garbage. Deleting what cannot be judged is how a sweep becomes
// the thing that loses data.
func TestCollectLeavesAnUnparseableDerivedKeyAlone(t *testing.T) {
	t.Parallel()
	s, blobs := service(t)

	write(t, s, "photo.jpg", "the original")
	stray := files.DerivedPrefix + "stray"
	if _, err := blobs.Put(t.Context(), stray, strings.NewReader("?"), -1); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Collect(t.Context(), 0); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if _, err := blobs.Stat(t.Context(), stray); err != nil {
		t.Errorf("an object nothing can judge was deleted: %v", err)
	}
}
