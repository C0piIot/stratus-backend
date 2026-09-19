package files_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/db/dbtest"
	"github.com/C0piIot/stratus-backend/internal/db/sqlite"
	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/storage"
	"github.com/C0piIot/stratus-backend/internal/storage/disk"
	"github.com/C0piIot/stratus-backend/internal/storage/storagetest"
)

// tree builds an album with a file, a subdirectory and a file inside it, which
// is the smallest shape that exercises the recursion and the ordering both.
func tree(t *testing.T, s *files.Service) {
	t.Helper()
	for _, dir := range []string{"album", "album/raw"} {
		if _, err := s.Mkdir(t.Context(), owner, dir); err != nil {
			t.Fatal(err)
		}
	}
	write(t, s, "album/one.jpg", "one")
	write(t, s, "album/raw/two.dng", "two")
}

// TestCopyTree is the feature: everything under the source lands under the
// destination, and the two are independent afterwards.
func TestCopyTree(t *testing.T) {
	t.Parallel()
	s, _ := service(t)
	tree(t, s)

	created, err := s.Copy(t.Context(), owner, "album", "backup", true)
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if !created {
		t.Error("a copy to a path that did not exist reported that it was not created")
	}

	if got := read(t, s, "backup/one.jpg"); got != "one" {
		t.Errorf("backup/one.jpg = %q", got)
	}
	if got := read(t, s, "backup/raw/two.dng"); got != "two" {
		t.Errorf("backup/raw/two.dng = %q", got)
	}
	if dir, err := s.Stat(t.Context(), owner, "backup/raw"); err != nil || !dir.IsDir {
		t.Errorf("backup/raw = %+v, %v, want a directory", dir, err)
	}
	// And the original is where it was.
	if got := read(t, s, "album/raw/two.dng"); got != "two" {
		t.Errorf("the source changed: %q", got)
	}
}

// TestCopyDoesNotShareBlobs is the property somebody would notice if it were
// wrong, and the reason the tempting one-insert version is not what this does:
// Remove deletes a blob as soon as its transaction commits, so a shared one
// would take the copy with it.
func TestCopyDoesNotShareBlobs(t *testing.T) {
	t.Parallel()
	s, _ := service(t)
	tree(t, s)

	if _, err := s.Copy(t.Context(), owner, "album", "backup", true); err != nil {
		t.Fatal(err)
	}

	source, err := s.Stat(t.Context(), owner, "album/one.jpg")
	if err != nil {
		t.Fatal(err)
	}
	copied, err := s.Stat(t.Context(), owner, "backup/one.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if source.BlobKey == copied.BlobKey {
		t.Fatalf("both rows point at %q, so deleting either would destroy both", source.BlobKey)
	}

	// Which is the thing that matters, said as the user would meet it.
	if err := s.Remove(t.Context(), owner, "album"); err != nil {
		t.Fatal(err)
	}
	if got := read(t, s, "backup/one.jpg"); got != "one" {
		t.Errorf("deleting the original left the copy reading %q", got)
	}
}

// TestCopyShallow is Depth: 0 on a collection: the collection and not its
// members, which is RFC 4918 9.8.3 and the one copy that moves no bytes.
func TestCopyShallow(t *testing.T) {
	t.Parallel()
	s, blobs := service(t)
	tree(t, s)
	before := blobCount(t, blobs)

	if _, err := s.Copy(t.Context(), owner, "album", "backup", false); err != nil {
		t.Fatalf("Copy: %v", err)
	}

	if dir, err := s.Stat(t.Context(), owner, "backup"); err != nil || !dir.IsDir {
		t.Errorf("backup = %+v, %v, want an empty directory", dir, err)
	}
	if _, err := s.Stat(t.Context(), owner, "backup/one.jpg"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("a member was copied at depth zero: %v", err)
	}
	if got := blobCount(t, blobs); got != before {
		t.Errorf("the store holds %d blobs, want the %d it had: a shallow copy writes none", got, before)
	}
}

// TestCopyThatFailsHalfwayLeavesNothing is #43's actual subject. A copy that
// dies in the middle must not leave half a tree, because RFC 4918 9.8.5 would
// then want a multistatus naming what failed and there is no such thing to send
// through this library.
func TestCopyThatFailsHalfwayLeavesNothing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

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

	s := files.New(blobs, meta)
	tree(t, s)
	before := blobCount(t, blobs)

	// The same tree, through a store whose writes fail.
	broken := files.New(storagetest.FailOn(t, blobs, "Put"), meta)
	if _, cerr := broken.Copy(t.Context(), owner, "album", "backup", true); !errors.Is(cerr, storagetest.ErrInjected) {
		t.Fatalf("Copy = %v, want the injected failure", cerr)
	}

	if _, serr := s.Stat(t.Context(), owner, "backup"); !errors.Is(serr, db.ErrNotFound) {
		t.Errorf("the destination exists after a failed copy: %v", serr)
	}
	if got := blobCount(t, blobs); got != before {
		t.Errorf("the store holds %d blobs and held %d: a failed copy left bytes behind", got, before)
	}
}

// TestCopyThatCannotCommitLeavesNothing is the other half of the same promise,
// from the side the blobs are already written: the transaction is the last
// thing that happens, so a database that refuses it leaves a destination that
// never existed and bytes that belong to nobody.
func TestCopyThatCannotCommitLeavesNothing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

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

	s := files.New(blobs, meta)
	tree(t, s)
	before := blobCount(t, blobs)

	broken := files.New(blobs, dbtest.FailOn(t, meta, "PutFile"))
	if _, cerr := broken.Copy(t.Context(), owner, "album", "backup", true); !errors.Is(cerr, dbtest.ErrInjected) {
		t.Fatalf("Copy = %v, want the injected failure", cerr)
	}

	if _, serr := s.Stat(t.Context(), owner, "backup"); !errors.Is(serr, db.ErrNotFound) {
		t.Errorf("the destination exists after a copy that could not commit: %v", serr)
	}
	// The blobs were written before the transaction, which is the ordering this
	// package lives by -- so what matters is that they were cleaned up, and
	// what saves us when that fails is that the sweep would take them anyway.
	if got := blobCount(t, blobs); got != before {
		t.Errorf("the store holds %d blobs and held %d", got, before)
	}
}

// TestCopyRefusesWhatWillNotFit is the check a copy grew because it is the
// first thing here that can fill a disk on purpose, and the moment to find that
// out is before the first byte rather than thirty gigabytes in.
func TestCopyRefusesWhatWillNotFit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

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

	s := files.New(blobs, meta)
	tree(t, s)
	before := blobCount(t, blobs)

	cramped := files.New(&fullStore{Storage: blobs}, meta)
	if _, cerr := cramped.Copy(t.Context(), owner, "album", "backup", true); !errors.Is(cerr, files.ErrNoSpace) {
		t.Fatalf("Copy onto a full store = %v, want ErrNoSpace", cerr)
	}
	if _, serr := s.Stat(t.Context(), owner, "backup"); !errors.Is(serr, db.ErrNotFound) {
		t.Errorf("a copy that was refused made something: %v", serr)
	}
	if got := blobCount(t, blobs); got != before {
		t.Errorf("a copy that was refused wrote %d blobs", got-before)
	}
}

// fullStore is a store with one byte left, which is the only part of the port
// this case is about.
type fullStore struct {
	storage.Storage
}

func (fullStore) FreeSpace(context.Context) (int64, error) { return 1, nil }

// TestCopyOverwritesRatherThanMerges is RFC 4918 9.8.4: what was at the
// destination is gone, not blended with what arrived.
func TestCopyOverwritesRatherThanMerges(t *testing.T) {
	t.Parallel()
	s, _ := service(t)
	tree(t, s)
	if _, err := s.Mkdir(t.Context(), owner, "backup"); err != nil {
		t.Fatal(err)
	}
	write(t, s, "backup/stale.txt", "from before")

	created, err := s.Copy(t.Context(), owner, "album", "backup", true)
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if created {
		t.Error("a copy over something that existed reported that it created it")
	}

	if _, err := s.Stat(t.Context(), owner, "backup/stale.txt"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("what the destination held survived the copy: %v", err)
	}
	if got := read(t, s, "backup/one.jpg"); got != "one" {
		t.Errorf("backup/one.jpg = %q", got)
	}
}

// TestCopyRefusesTheImpossible: the two a rename refuses are the two a copy
// refuses, which is why both go through db.ValidateMove.
func TestCopyRefusesTheImpossible(t *testing.T) {
	t.Parallel()
	s, _ := service(t)
	tree(t, s)

	for name, to := range map[string]string{
		"onto itself":         "album",
		"inside itself":       "album/inner",
		"under a missing dir": "nowhere/backup",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := s.Copy(t.Context(), owner, "album", to, true); err == nil {
				t.Errorf("copying album to %q was allowed", to)
			}
		})
	}
}

// TestCopyGivesUpBeforeItStarts collects the refusals that happen on the way
// in, where nothing has been written yet and the only thing to check is that
// the error comes back rather than being swallowed into half a copy.
func TestCopyGivesUpBeforeItStarts(t *testing.T) {
	t.Parallel()

	t.Run("a source that is not there", func(t *testing.T) {
		t.Parallel()
		s, _ := service(t)
		if _, err := s.Copy(t.Context(), owner, "nothing", "backup", true); !errors.Is(err, db.ErrNotFound) {
			t.Errorf("Copy of a missing source = %v, want ErrNotFound", err)
		}
	})

	t.Run("a tree that cannot be read", func(t *testing.T) {
		t.Parallel()
		s, blobs, meta := serviceOver(t)
		tree(t, s)

		broken := files.New(blobs, dbtest.FailOn(t, meta, "ListFiles"))
		if _, err := broken.Copy(t.Context(), owner, "album", "backup", true); !errors.Is(err, dbtest.ErrInjected) {
			t.Errorf("Copy = %v, want the injected failure", err)
		}
	})

	t.Run("a store that will not say how much room is left", func(t *testing.T) {
		t.Parallel()
		s, blobs, meta := serviceOver(t)
		tree(t, s)

		broken := files.New(&unmeasurableStore{Storage: blobs}, meta)
		if _, err := broken.Copy(t.Context(), owner, "album", "backup", true); !errors.Is(err, errNoAnswer) {
			t.Errorf("Copy = %v, want the store's own failure", err)
		}
	})
}

var errNoAnswer = errors.New("this store cannot say")

type unmeasurableStore struct {
	storage.Storage
}

func (unmeasurableStore) FreeSpace(context.Context) (int64, error) { return 0, errNoAnswer }

// TestCopyOneFile is the case that already worked, kept because the path
// through it changed: it now shares storeBlob with the tree.
func TestCopyOneFile(t *testing.T) {
	t.Parallel()
	s, _ := service(t)
	write(t, s, "notes.txt", "content")

	if _, err := s.Copy(t.Context(), owner, "notes.txt", "copy.txt", true); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if got := read(t, s, "copy.txt"); got != "content" {
		t.Errorf("the copy reads %q", got)
	}
	// The MIME type travels with it rather than being sniffed afresh from a
	// name that changed.
	f, err := s.Stat(t.Context(), owner, "copy.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(f.MIMEType, "text/plain") {
		t.Errorf("the copy is %q", f.MIMEType)
	}
}
