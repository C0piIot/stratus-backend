package files_test

import (
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/storage"
)

// adopted is one row of somebody else's bucket, shaped the way an importer
// hands it over: a key this server did not choose, and no ETag, because the
// instance it came from had no hash of the content to give.
func adopted(path, key string, size int64) db.File {
	return db.File{
		Path:     path,
		BlobKey:  key,
		Size:     size,
		MTime:    time.Unix(1700000000, 0).UTC(),
		MIMEType: "image/jpeg",
	}
}

// keysIn is everything in the store, sorted, so that two listings can be
// compared.
func keysIn(t *testing.T, blobs storage.Storage) []string {
	t.Helper()
	var keys []string
	for info, err := range blobs.List(t.Context(), "") {
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		keys = append(keys, info.Key)
	}
	sort.Strings(keys)
	return keys
}

func TestAdoptingABucketWritesRowsAndNoBytes(t *testing.T) {
	s, blobs := service(t)
	if _, err := blobs.Put(t.Context(), "urn:oid:7", strings.NewReader("sevenby"), 7); err != nil {
		t.Fatal(err)
	}

	count, skipped, err := s.Adopt(t.Context(), owner, []db.File{
		adopted("album/inner.jpg", "urn:oid:7", 7),
	})
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if count != 1 || skipped != 0 {
		t.Fatalf("adopted %d skipped %d, want 1 and 0", count, skipped)
	}

	// The whole point: the row points at the key the bucket already used.
	f, err := s.Stat(t.Context(), owner, "album/inner.jpg")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if f.BlobKey != "urn:oid:7" {
		t.Errorf("blob key = %q, want the one the bucket already had", f.BlobKey)
	}
	if got := read(t, s, "album/inner.jpg"); got != "sevenby" {
		t.Errorf("read back %q", got)
	}
}

func TestAdoptingBuildsTheFoldersOnTheWay(t *testing.T) {
	s, blobs := service(t)
	if _, err := blobs.Put(t.Context(), "urn:oid:8", strings.NewReader("x"), 1); err != nil {
		t.Fatal(err)
	}

	// No directory row is handed over, which is what a caller that only knows
	// about files looks like.
	if _, _, err := s.Adopt(t.Context(), owner, []db.File{
		adopted("a/b/c/deep.jpg", "urn:oid:8", 1),
	}); err != nil {
		t.Fatalf("Adopt: %v", err)
	}

	for _, dir := range []string{"a", "a/b", "a/b/c"} {
		f, err := s.Stat(t.Context(), owner, dir)
		if err != nil {
			t.Fatalf("Stat(%q): %v", dir, err)
		}
		if !f.IsDir {
			t.Errorf("%q is not a directory", dir)
		}
	}
}

func TestAdoptingATakenPathSkipsItRatherThanReplacingIt(t *testing.T) {
	s, blobs := service(t)
	mine := write(t, s, "notes.txt", "what I wrote")
	if _, err := blobs.Put(t.Context(), "urn:oid:9", strings.NewReader("theirs"), 6); err != nil {
		t.Fatal(err)
	}

	count, skipped, err := s.Adopt(t.Context(), owner, []db.File{
		adopted("notes.txt", "urn:oid:9", 6),
	})
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if count != 0 || skipped != 1 {
		t.Fatalf("adopted %d skipped %d, want 0 and 1", count, skipped)
	}

	// An import is not a write: what was there stays, which is also what makes
	// running an interrupted import again finish it rather than undo it.
	f, err := s.Stat(t.Context(), owner, "notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	if f.BlobKey != mine.BlobKey {
		t.Errorf("blob key = %q, want the row that was already there", f.BlobKey)
	}
	if got := read(t, s, "notes.txt"); got != "what I wrote" {
		t.Errorf("read back %q", got)
	}
}

func TestAdoptingADirectoryRowKeepsAnEmptyFolder(t *testing.T) {
	s, _ := service(t)

	if _, _, err := s.Adopt(t.Context(), owner, []db.File{
		{Path: "empty", IsDir: true, MTime: time.Unix(1700000000, 0).UTC()},
	}); err != nil {
		t.Fatalf("Adopt: %v", err)
	}

	f, err := s.Stat(t.Context(), owner, "empty")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !f.IsDir {
		t.Error("the folder did not survive the import")
	}
}

func TestAdoptingRefusesAPathThisServerWillNotStore(t *testing.T) {
	s, _ := service(t)

	_, _, err := s.Adopt(t.Context(), owner, []db.File{adopted("../escape.jpg", "urn:oid:1", 1)})
	if !errors.Is(err, db.ErrInvalidPath) {
		t.Fatalf("err = %v, want an invalid path", err)
	}
}

func TestAdoptingNeedsAnOwner(t *testing.T) {
	s, _ := service(t)

	_, _, err := s.Adopt(t.Context(), "", []db.File{adopted("x.jpg", "urn:oid:1", 1)})
	if !errors.Is(err, db.ErrConflict) {
		t.Fatalf("err = %v, want a conflict", err)
	}
}

func TestAdoptingLeavesTheBucketAlone(t *testing.T) {
	s, blobs := service(t)
	if _, err := blobs.Put(t.Context(), "urn:oid:11", strings.NewReader("mine"), 4); err != nil {
		t.Fatal(err)
	}

	before := keysIn(t, blobs)
	if _, _, err := s.Adopt(t.Context(), owner, []db.File{
		adopted("one.jpg", "urn:oid:11", 4),
	}); err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	after := keysIn(t, blobs)

	// Not a byte moved is the whole promise, and the store is where it would
	// show: no copy under a key of ours, and nothing written or deleted.
	if len(before) != len(after) {
		t.Fatalf("the store went from %v to %v", before, after)
	}
	for i := range before {
		if before[i] != after[i] {
			t.Errorf("the store went from %v to %v", before, after)
		}
	}
}
