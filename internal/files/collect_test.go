package files_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
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

// orphan puts an object in the store that no row will ever point at.
//
// That is what a process killed between the blob and its row leaves behind,
// and since #272 it is very nearly the only way one gets there: an overwrite
// takes its own predecessor now, so the sweep is the net under the cases
// nobody announced rather than the routine collector it used to be.
func orphan(t *testing.T, blobs storage.Storage, name, body string) string {
	t.Helper()
	key := "document/2026/01/01/" + name + ".txt"
	if _, err := blobs.Put(t.Context(), key, strings.NewReader(body), -1); err != nil {
		t.Fatalf("put the orphan %q: %v", key, err)
	}
	return key
}

// TestCollectTakesABlobNoRowPointsAt is the leak this exists for -- and since
// #276 it takes it to the trash rather than destroying it, because a blob
// nobody can account for is as likely to be a database that went wrong as a
// file that rotted.
func TestCollectTakesABlobNoRowPointsAt(t *testing.T) {
	t.Parallel()
	s, blobs := service(t)

	live := write(t, s, "notes.txt", "version two")
	left := orphan(t, blobs, "ORPHANEDBYACRASH", "version one")
	if blobCount(t, blobs) != 2 {
		t.Fatal("the fixture is not two objects, so this test is testing nothing")
	}

	// No grace at all, because the blobs were written a moment ago.
	done, err := s.Collect(t.Context(), 0)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if done.Trashed != 1 || done.Deleted != 0 || done.Scanned != 2 {
		t.Errorf("Collect = %+v, want one of two moved and nothing destroyed", done)
	}

	// Nothing was freed, and that is the point: both blobs are still there.
	if done.Bytes != 0 {
		t.Errorf("Bytes = %d, want nothing freed by a pass that destroyed nothing", done.Bytes)
	}
	if blobCount(t, blobs) != 2 {
		t.Error("a blob was destroyed by a pass that should only have moved one")
	}
	if got := read(t, s, "notes.txt"); got != "version two" {
		t.Errorf("the live file reads %q", got)
	}
	if _, serr := blobs.Stat(t.Context(), left); serr != nil {
		t.Errorf("the unaccounted blob was destroyed: %v", serr)
	}

	// It is in the trash, under no owner, because nobody can say whose it was.
	batches, err := s.Trash(t.Context(), "", db.TrashCursor{}, 10)
	if err != nil {
		t.Fatalf("Trash: %v", err)
	}
	if len(batches) != 1 || batches[0].Files != 1 {
		t.Fatalf("the unaccounted blobs are %+v, want one", batches)
	}
	// And not in anybody's: a page shows deletions, and this is not one.
	if mine, _ := s.Trash(t.Context(), owner, db.TrashCursor{}, 10); len(mine) != 0 {
		t.Errorf("an unaccounted blob showed up as somebody's deletion: %+v", mine)
	}
	// Nor can it be put back: there is no path to put it at.
	if _, err := s.Restore(t.Context(), "", batches[0].ID); !errors.Is(err, db.ErrConflict) {
		t.Errorf("Restore of an unaccounted blob = %v, want ErrConflict", err)
	}

	// A second pass does not find it again: the trash holds its key now.
	switch again, aerr := s.Collect(t.Context(), 0); {
	case aerr != nil:
		t.Fatalf("Collect: %v", aerr)
	case again.Trashed != 0:
		t.Errorf("the second pass moved %d blobs that were already in the trash", again.Trashed)
	}

	// And destroying the batch is what finally frees the room.
	if _, _, err := s.DestroyTrashed(t.Context(), "", batches[0].ID); err != nil {
		t.Fatalf("DestroyTrashed: %v", err)
	}
	if _, err := blobs.Stat(t.Context(), left); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("the unaccounted blob survived being destroyed: %v", err)
	}
	if _, err := blobs.Stat(t.Context(), live.BlobKey); err != nil {
		t.Errorf("the live blob was destroyed with it: %v", err)
	}
}

// TestCollectKeepsThePicturesOfWhatItMoved is why the pass counts before it
// deletes: an original that goes to the trash stops being garbage in that
// same pass, and whether its thumbnails survive must not depend on the order
// the store happened to list them in.
func TestCollectKeepsThePicturesOfWhatItMoved(t *testing.T) {
	t.Parallel()
	s, blobs := service(t)

	left := orphan(t, blobs, "APHOTOGRAPHNOROWHOLDS", "pixels")
	thumb := files.DerivedKey(left, "300.jpg")
	if _, err := blobs.Put(t.Context(), thumb, strings.NewReader("a thumbnail"), -1); err != nil {
		t.Fatal(err)
	}
	write(t, s, "keep.txt", "so the index is not empty")

	switch done, err := s.Collect(t.Context(), 0); {
	case err != nil:
		t.Fatalf("Collect: %v", err)
	case done.Trashed != 1 || done.Deleted != 0:
		t.Errorf("Collect = %+v, want the original moved and its picture left alone", done)
	}
	if _, err := blobs.Stat(t.Context(), thumb); err != nil {
		t.Errorf("the picture of what was moved was destroyed: %v", err)
	}

	// And it goes when the original does.
	batches, _ := s.Trash(t.Context(), "", db.TrashCursor{}, 10)
	if _, _, err := s.DestroyTrashed(t.Context(), "", batches[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := blobs.Stat(t.Context(), thumb); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("the picture outlived the blob it was made from: %v", err)
	}
}

// TestCollectRespectsTheGrace is the rule that keeps this from deleting uploads
// in flight: a write puts the blob down before the row, so a blob with no row
// may simply not have finished.
func TestCollectRespectsTheGrace(t *testing.T) {
	t.Parallel()
	s, blobs := service(t)
	write(t, s, "notes.txt", "one")
	orphan(t, blobs, "WRITTENASECONDAGO", "two")

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
	// A deleted blob is in the trash, not in limbo: it has an owner and a
	// month, and the sweep is not who decides to throw it away (#274).
	done, err := s.Collect(t.Context(), 0)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if done.Deleted != 0 {
		t.Errorf("Collect took %d blobs that belong to the trash", done.Deleted)
	}
	if blobCount(t, blobs) != 2 {
		t.Error("the surviving file or the trashed one lost its blob")
	}

	// And once the trash lets go, the same sweep is what would have taken it
	// -- except that destroying a deletion takes its bytes itself.
	batches, err := s.Trash(t.Context(), owner, db.TrashCursor{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.DestroyTrashed(t.Context(), owner, batches[0].ID); err != nil {
		t.Fatal(err)
	}
	if blobCount(t, blobs) != 1 {
		t.Error("destroying the deletion left its bytes behind")
	}
}

// TestCollectRefusesAnEmptyIndex is the guard against the worst possible
// outcome: a database pointed at a fresh file, a store full of somebody's
// photos, and a sweep that concludes none of them are referenced.
func TestCollectRefusesAnEmptyIndex(t *testing.T) {
	t.Parallel()
	s, blobs := service(t)
	// A store with somebody's photograph in it and a database that knows
	// nothing about it, which is what a DSN pointed somewhere new looks like.
	orphan(t, blobs, "SOMEBODYSPHOTOGRAPH", "irreplaceable")

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

// TestCollectKeepsAFreshCache: a cached derived object is collected a week
// after it was written, not the moment its sweep sees it.
func TestCollectKeepsAFreshCache(t *testing.T) {
	t.Parallel()
	s, blobs := service(t)
	live := write(t, s, "film.mkv", "the original")
	cache := files.DerivedKey(live.BlobKey, files.CachePrefix+"hls-h264-0-6000.ts")
	if _, err := blobs.Put(t.Context(), cache, strings.NewReader("a segment"), -1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Collect(t.Context(), 0); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if _, err := blobs.Stat(t.Context(), cache); err != nil {
		t.Errorf("a cache written a moment ago was collected: %v", err)
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
// parent's, so one rule collects both.
func TestCollectSweepsDerivedObjectsWithTheirParent(t *testing.T) {
	t.Parallel()
	s, blobs := service(t)

	// A picture whose original is not in the store at all, which is what is
	// left when a blob was destroyed and its derived objects were not. The
	// parent is a key, not an object: nothing holds it and nothing will.
	orphaned := files.DerivedKey("image/2026/01/01/APARENTNOROWHOLDS.jpg", "300.jpg")
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
	// The picture with no original, and nothing else. What is derived is
	// destroyed rather than kept, because it can be made again.
	if done.Deleted != 1 || done.Trashed != 0 {
		t.Errorf("Collect = %+v, want the parentless picture destroyed and nothing moved", done)
	}
	if _, err := blobs.Stat(t.Context(), orphaned); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("the thumbnail of the orphaned blob survived: %v", err)
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
