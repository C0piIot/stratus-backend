package files_test

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/db/dbtest"
	"github.com/C0piIot/stratus-backend/internal/db/sqlite"
	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/storage"
	"github.com/C0piIot/stratus-backend/internal/storage/disk"
	"github.com/C0piIot/stratus-backend/internal/storage/storagetest"
)

// What this package does when half of a write succeeds.
//
// A file is a row and a blob across two seams with no transaction between them,
// which is stated in CLAUDE.md and in Write itself: the mitigation is ordering,
// **blob first and row second**, so a failure leaves a collectable orphan rather
// than a row pointing at nothing. Every test of that invariant until now
// asserted the half where both succeed.

// breakable returns a service whose backends can be made to fail, plus the real
// ones, so a case can look at what the store actually holds afterwards.
func breakable(t *testing.T) (blobs storage.Storage, meta db.Store) {
	t.Helper()
	dir := t.TempDir()

	onDisk, err := disk.New(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = onDisk.Close() })

	store, err := sqlite.New(t.Context(), filepath.Join(dir, "stratus.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return onDisk, store
}

// TestWriteCleansUpWhenTheRowFails is the first half of what the ordering
// buys. The blob is already down when the row is attempted, and there is no
// transaction spanning two seams -- there cannot be -- so Write removes it by
// hand. The file does not exist and nothing is left over.
func TestWriteCleansUpWhenTheRowFails(t *testing.T) {
	t.Parallel()
	blobs, meta := breakable(t)

	broken := files.New(blobs, dbtest.FailOn(t, meta, "PutFile"))
	_, err := broken.Write(t.Context(), owner, "notes.txt", strings.NewReader("hello"), 5, "text/plain")
	if !errors.Is(err, dbtest.ErrInjected) {
		t.Fatalf("Write = %v, want the injected failure", err)
	}

	working := files.New(blobs, meta)
	if _, err := working.Stat(t.Context(), owner, "notes.txt"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("Stat after a failed write = %v, want ErrNotFound", err)
	}
	for range blobs.List(t.Context(), "") {
		t.Error("the store holds an object after a write whose row failed")
	}
}

// TestWriteLeavesACollectableOrphanWhenTheCleanupAlsoFails is the second half,
// and the one the ordering exists for.
//
// That cleanup is best effort by construction: it is a second call to a seam
// that just failed the first one. When it fails too, what is left is a blob
// nothing points at -- garbage the sweep already knows how to take. The
// alternative ordering leaves a row pointing at nothing, which is a file that
// exists until somebody tries to read it, and no sweep can fix that.
//
// Both seams fail here, which is the only way to reach it: one injector per
// port, each breaking one call.
func TestWriteLeavesACollectableOrphanWhenTheCleanupAlsoFails(t *testing.T) {
	t.Parallel()
	blobs, meta := breakable(t)

	working := files.New(blobs, meta)
	// A second file, so the index is not empty when the sweep runs: a database
	// referencing nothing at all is the case Collect refuses outright.
	if _, err := working.Write(t.Context(), owner, "keep.txt", strings.NewReader("keep"), 4, "text/plain"); err != nil {
		t.Fatal(err)
	}

	broken := files.New(storagetest.FailOn(t, blobs, "Delete"), dbtest.FailOn(t, meta, "PutFile"))
	_, err := broken.Write(t.Context(), owner, "notes.txt", strings.NewReader("hello"), 5, "text/plain")
	if !errors.Is(err, dbtest.ErrInjected) {
		t.Fatalf("Write = %v, want the row failure and not the cleanup's", err)
	}

	// No row, so nothing above this package can see the file.
	if _, serr := working.Stat(t.Context(), owner, "notes.txt"); !errors.Is(serr, db.ErrNotFound) {
		t.Errorf("Stat after a failed write = %v, want ErrNotFound", serr)
	}
	// And a blob nobody points at, which is the shape of the damage.
	if got := blobCount(t, blobs); got != 2 {
		t.Fatalf("the store holds %d blobs, want the live one and the orphan", got)
	}

	// The sweep takes it and leaves the live one, which is what makes the
	// damage temporary. No grace, because both were written a moment ago.
	done, err := working.Collect(t.Context(), 0)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if done.Deleted != 1 {
		t.Errorf("Collect deleted %d, want the orphan alone", done.Deleted)
	}
	if got := read(t, working, "keep.txt"); got != "keep" {
		t.Errorf("the live file reads %q", got)
	}
}

// TestWriteWritesNoBlobWhenTheStoreRefuses is the other order, and the reason
// the ordering is not arbitrary: nothing is left behind at all, because nothing
// was written. A row is never created for bytes that did not land.
func TestWriteWritesNoBlobWhenTheStoreRefuses(t *testing.T) {
	t.Parallel()
	blobs, meta := breakable(t)

	broken := files.New(storagetest.FailOn(t, blobs, "Put"), meta)
	_, err := broken.Write(t.Context(), owner, "notes.txt", strings.NewReader("hello"), 5, "text/plain")
	if !errors.Is(err, storagetest.ErrInjected) {
		t.Fatalf("Write = %v, want the injected failure", err)
	}

	working := files.New(blobs, meta)
	if _, err := working.Stat(t.Context(), owner, "notes.txt"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("a row was written for a blob that never landed: %v", err)
	}
	for range blobs.List(t.Context(), "") {
		t.Error("the store holds an object after a Put that failed")
	}
}

// TestDestroyingADeletionReportsABlobItCouldNotDelete: the rows go in one
// transaction and the blobs afterwards, so a store that refuses leaves garbage
// the caller has to hear about -- the deletion is gone as far as the trash is
// concerned, and silence would make the leak invisible.
//
// Remove itself can no longer fail this way: it touches no blob at all (#274).
func TestDestroyingADeletionReportsABlobItCouldNotDelete(t *testing.T) {
	t.Parallel()
	blobs, meta := breakable(t)

	working := files.New(blobs, meta)
	if _, err := working.Write(t.Context(), owner, "notes.txt", strings.NewReader("hello"), 5, "text/plain"); err != nil {
		t.Fatal(err)
	}

	// A second file, so the index is not empty for the sweep at the end.
	if _, err := working.Write(t.Context(), owner, "keep.txt", strings.NewReader("keep"), 4, "text/plain"); err != nil {
		t.Fatal(err)
	}
	if err := working.Remove(t.Context(), owner, "notes.txt"); err != nil {
		t.Fatal(err)
	}
	batches, err := working.Trash(t.Context(), owner, db.TrashCursor{}, 10)
	if err != nil || len(batches) != 1 {
		t.Fatalf("Trash = %+v, %v", batches, err)
	}

	broken := files.New(storagetest.FailOn(t, blobs, "Delete"), meta)
	if _, _, derr := broken.DestroyTrashed(t.Context(), owner, batches[0].ID); !errors.Is(derr, storagetest.ErrInjected) {
		t.Fatalf("DestroyTrashed = %v, want the injected failure", derr)
	}

	// The row is gone even so, which is what makes this a leak and not a
	// half-deleted file: the tree is consistent and the store has garbage in it.
	if _, err := working.Stat(t.Context(), owner, "notes.txt"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("the row survived a delete that reported a blob failure: %v", err)
	}
	if got := blobCount(t, blobs); got != 2 {
		t.Errorf("the store holds %d blobs, want the live one and the leak", got)
	}
	// And the sweep is what eventually takes it.
	if done, err := working.Collect(t.Context(), 0); err != nil || done.Deleted != 1 {
		t.Errorf("Collect = %+v, %v, want it to take the leak alone", done, err)
	}
}

// TestCollectStopsWhenItCannotRead is the rule that keeps a sweep from becoming
// the thing that loses data. It reads the database first and the store second,
// and either half failing means it does not know what is referenced -- so it
// stops rather than deleting what it could not account for.
func TestCollectStopsWhenItCannotRead(t *testing.T) {
	t.Parallel()
	blobs, meta := breakable(t)

	working := files.New(blobs, meta)
	if _, err := working.Write(t.Context(), owner, "notes.txt", strings.NewReader("hello"), 5, "text/plain"); err != nil {
		t.Fatal(err)
	}

	t.Run("the database will not say what is referenced", func(t *testing.T) {
		broken := files.New(blobs, dbtest.FailOn(t, meta, "BlobKeys"))
		if _, err := broken.Collect(t.Context(), 0); !errors.Is(err, dbtest.ErrInjected) {
			t.Errorf("Collect = %v, want the injected failure", err)
		}
	})

	t.Run("the store will not say what it holds", func(t *testing.T) {
		broken := files.New(storagetest.FailOn(t, blobs, "List"), meta)
		if _, err := broken.Collect(t.Context(), 0); !errors.Is(err, storagetest.ErrInjected) {
			t.Errorf("Collect = %v, want the injected failure", err)
		}
	})

	// The live blob is still there after both, which is the assertion that
	// matters: a sweep that could not read has deleted nothing.
	if got := blobCount(t, blobs); got != 1 {
		t.Errorf("the store holds %d blobs, want the live one untouched", got)
	}
}

// TestCollectReportsABlobItCouldNotDelete, because a sweep that says it removed
// what it did not would have the operator believing a bill went away.
func TestCollectReportsABlobItCouldNotDelete(t *testing.T) {
	t.Parallel()
	blobs, meta := breakable(t)

	working := files.New(blobs, meta)
	if _, err := working.Write(t.Context(), owner, "notes.txt", strings.NewReader("one"), 3, "text/plain"); err != nil {
		t.Fatal(err)
	}
	// A blob nothing points at, which is what the sweep is the net for.
	if _, err := blobs.Put(t.Context(), "document/2026/01/01/NOROWHOLDSTHIS.txt",
		strings.NewReader("two"), -1); err != nil {
		t.Fatal(err)
	}

	broken := files.New(storagetest.FailOn(t, blobs, "Delete"), meta)
	done, err := broken.Collect(t.Context(), 0)
	if !errors.Is(err, storagetest.ErrInjected) {
		t.Fatalf("Collect = %v, want the injected failure", err)
	}
	if done.Deleted != 0 {
		t.Errorf("Collect reported %d deleted after deleting none", done.Deleted)
	}
}

// TestEmptyingTheTrashSurvivesNothing: the pass is a loop over rows, and a
// database that will not yield them has to stop the pass rather than report
// an empty trash.
func TestEmptyingTheTrashSurvivesNothing(t *testing.T) {
	t.Parallel()
	blobs, meta := breakable(t)
	working := files.New(blobs, meta)
	if _, err := working.Write(t.Context(), owner, "notes.txt", strings.NewReader("hello"), 5, "text/plain"); err != nil {
		t.Fatal(err)
	}
	if err := working.Remove(t.Context(), owner, "notes.txt"); err != nil {
		t.Fatal(err)
	}

	broken := files.New(blobs, dbtest.FailOn(t, meta, "ExpiredTrash"))
	if _, err := broken.EmptyTrash(t.Context(), time.Now()); !errors.Is(err, dbtest.ErrInjected) {
		t.Fatalf("EmptyTrash = %v, want the injected failure", err)
	}

	// And one that cannot read the deletion it is about to destroy leaves it
	// alone rather than deleting the rows it could not list.
	alsoBroken := files.New(blobs, dbtest.FailOn(t, meta, "TrashedIn"))
	if _, err := alsoBroken.EmptyTrash(t.Context(), time.Now()); !errors.Is(err, dbtest.ErrInjected) {
		t.Fatalf("EmptyTrash = %v, want the injected failure", err)
	}
	if batches, _ := working.Trash(t.Context(), owner, db.TrashCursor{}, 10); len(batches) != 1 {
		t.Error("the deletion was destroyed by a pass that failed")
	}
}

// TestRestoreSurvivesNothing: a restore is one transaction, so a database that
// fails halfway has to leave the deletion in the trash and the tree alone --
// the half-restored folder is the thing this must never produce.
func TestRestoreSurvivesNothing(t *testing.T) {
	t.Parallel()
	blobs, meta := breakable(t)
	working := files.New(blobs, meta)
	if _, err := working.Mkdir(t.Context(), owner, "album"); err != nil {
		t.Fatal(err)
	}
	if _, err := working.Write(t.Context(), owner, "album/one.jpg", strings.NewReader("one"), 3, "image/jpeg"); err != nil {
		t.Fatal(err)
	}
	if err := working.Remove(t.Context(), owner, "album"); err != nil {
		t.Fatal(err)
	}
	batches, err := working.Trash(t.Context(), owner, db.TrashCursor{}, 10)
	if err != nil || len(batches) != 1 {
		t.Fatalf("Trash = %+v, %v", batches, err)
	}

	for _, method := range []string{"TrashedIn", "FileByPath", "CreateDir", "PutFile"} {
		broken := files.New(blobs, dbtest.FailOn(t, meta, method))
		if _, rerr := broken.Restore(t.Context(), owner, batches[0].ID); !errors.Is(rerr, dbtest.ErrInjected) {
			t.Errorf("Restore with %s broken = %v, want the injected failure", method, rerr)
		}
		// Nothing landed, and the deletion is still there to try again.
		if _, serr := working.Stat(t.Context(), owner, "album"); !errors.Is(serr, db.ErrNotFound) {
			t.Errorf("%s: a failed restore left part of the tree behind", method)
		}
		if left, _ := working.Trash(t.Context(), owner, db.TrashCursor{}, 10); len(left) != 1 {
			t.Errorf("%s: a failed restore emptied the trash anyway", method)
		}
	}
}
