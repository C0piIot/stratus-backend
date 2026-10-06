package files_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
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

const owner = "edu"

// service wires the real backends rather than fakes. Both are in-process, and
// the invariants under test are precisely the ones that only appear when two
// real seams are involved.
func service(t *testing.T, opts ...files.Option) (*files.Service, storage.Storage) {
	t.Helper()
	s, blobs, _ := serviceOver(t, opts...)
	return s, blobs
}

// serviceOver is service plus the metadata store, for the cases that have to
// look at a row from the other side or break one on purpose.
func serviceOver(t *testing.T, opts ...files.Option) (*files.Service, storage.Storage, db.Store) {
	t.Helper()
	dir := t.TempDir()

	blobs, err := disk.New(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("disk.New: %v", err)
	}
	t.Cleanup(func() { _ = blobs.Close() })

	meta, err := sqlite.New(t.Context(), filepath.Join(dir, "stratus.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = meta.Close() })
	if err := meta.Migrate(t.Context()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return files.New(blobs, meta, opts...), blobs, meta
}

func write(t *testing.T, s *files.Service, path, body string) db.File {
	t.Helper()
	f, err := s.Write(t.Context(), owner, path, strings.NewReader(body), int64(len(body)), "text/plain")
	if err != nil {
		t.Fatalf("Write(%q): %v", path, err)
	}
	return f
}

func read(t *testing.T, s *files.Service, path string) string {
	t.Helper()
	body, _, err := s.Open(t.Context(), owner, path)
	if err != nil {
		t.Fatalf("Open(%q): %v", path, err)
	}
	defer func() { _ = body.Close() }()
	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read %q: %v", path, err)
	}
	return string(got)
}

func TestWriteAndRead(t *testing.T) {
	t.Parallel()
	s, _ := service(t)
	const body = "a photo, allegedly"

	f := write(t, s, "photo.jpg", body)
	if f.Size != int64(len(body)) {
		t.Errorf("Size = %d, want %d", f.Size, len(body))
	}
	// A strong validator: the digest of what was stored, not a guess from a
	// size and a timestamp.
	sum := sha256.Sum256([]byte(body))
	if want := hex.EncodeToString(sum[:]); f.ETag != want {
		t.Errorf("ETag = %s, want the content digest %s", f.ETag, want)
	}
	if got := read(t, s, "photo.jpg"); got != body {
		t.Errorf("read %q, want %q", got, body)
	}
}

func TestWriteWithUnknownSize(t *testing.T) {
	t.Parallel()
	s, _ := service(t)
	const body = "chunked, no content-length"

	f, err := s.Write(t.Context(), owner, "note.txt", strings.NewReader(body), -1, "text/plain")
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if f.Size != int64(len(body)) {
		t.Errorf("Size = %d, want %d", f.Size, len(body))
	}
}

// TestOverwriteDropsWhatItReplaced: a new key every time, so the previous
// content is intact until the row that replaces it has committed -- and gone
// immediately afterwards, with the pictures made from it, rather than left for
// a sweep that runs once a day (#272).
func TestOverwriteDropsWhatItReplaced(t *testing.T) {
	t.Parallel()
	s, blobs := service(t)

	first := write(t, s, "notes.txt", "version one")
	thumb := files.DerivedKey(first.BlobKey, "300.jpg")
	if _, err := blobs.Put(t.Context(), thumb, strings.NewReader("a thumbnail"), -1); err != nil {
		t.Fatal(err)
	}

	second := write(t, s, "notes.txt", "version two")
	if first.BlobKey == second.BlobKey {
		t.Error("the overwrite reused the blob key, so the old content was destroyed in place")
	}
	if got := read(t, s, "notes.txt"); got != "version two" {
		t.Errorf("read %q, want the second version", got)
	}
	if _, err := blobs.Stat(t.Context(), first.BlobKey); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("the replaced blob is still there: %v", err)
	}
	if _, err := blobs.Stat(t.Context(), thumb); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("the picture made from the replaced blob is still there: %v", err)
	}
	if n := blobCount(t, blobs); n != 1 {
		t.Errorf("the store holds %d objects, want the live one alone", n)
	}
}

// TestWritingOverADirectoryDropsNothing: a directory has no blob, so there is
// no key to drop and nothing may be invented in its place. The write is
// refused by the database, which is where that rule lives.
func TestWritingOverADirectoryDropsNothing(t *testing.T) {
	t.Parallel()
	s, blobs := service(t)
	if _, err := s.Mkdir(t.Context(), owner, "album"); err != nil {
		t.Fatal(err)
	}
	write(t, s, "album/photo.jpg", "a photograph")

	if _, err := s.Write(t.Context(), owner, "album",
		strings.NewReader("not a directory"), 15, "text/plain"); err == nil {
		t.Fatal("writing a file over a directory was allowed")
	}
	if n := blobCount(t, blobs); n != 1 {
		t.Errorf("the store holds %d objects, want the photograph alone", n)
	}
	if got := read(t, s, "album/photo.jpg"); got != "a photograph" {
		t.Errorf("the photograph reads %q", got)
	}
}

// TestWritingSomewhereNewDropsNothing is the other side of it: there is no
// previous content, and a write must not go looking for one to delete.
func TestWritingSomewhereNewDropsNothing(t *testing.T) {
	t.Parallel()
	s, blobs := service(t)

	write(t, s, "one.txt", "one")
	write(t, s, "two.txt", "two")
	if n := blobCount(t, blobs); n != 2 {
		t.Errorf("the store holds %d objects, want both", n)
	}
}

// TestAnOverwriteThatCannotTidyUpStillSucceeds: the bytes are stored and the
// row is committed, so the write happened. Failing it because the *previous*
// content could not be swept away would be a lie about what the server did,
// and what is left behind is exactly the orphan the sweep exists for.
func TestAnOverwriteThatCannotTidyUpStillSucceeds(t *testing.T) {
	t.Parallel()
	blobs, meta := breakable(t)
	s := files.New(blobs, meta)
	first := write(t, s, "notes.txt", "version one")

	broken := files.New(storagetest.FailOn(t, blobs, "Delete"), meta)
	if _, err := broken.Write(t.Context(), owner, "notes.txt",
		strings.NewReader("version two"), 11, "text/plain"); err != nil {
		t.Fatalf("Write over a store that will not delete = %v, want it to succeed", err)
	}
	if got := read(t, s, "notes.txt"); got != "version two" {
		t.Errorf("read %q, want the second version", got)
	}
	if _, err := blobs.Stat(t.Context(), first.BlobKey); err != nil {
		t.Fatalf("the orphan is not where the sweep will find it: %v", err)
	}

	// And the net under it still works: the orphan goes to the trash, which
	// is where the sweep puts what it cannot account for (#276).
	switch done, err := s.Collect(t.Context(), 0); {
	case err != nil:
		t.Fatalf("Collect: %v", err)
	case done.Trashed != 1:
		t.Errorf("Collect = %+v, want the orphan the write could not", done)
	}
}

func TestParentMustExist(t *testing.T) {
	t.Parallel()
	s, blobs := service(t)

	_, err := s.Write(t.Context(), owner, "album/photo.jpg", strings.NewReader("x"), 1, "image/jpeg")
	if !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("Write into a missing directory = %v, want ErrNotFound", err)
	}
	// And the blob it had already written was cleaned up rather than left
	// behind for the collector.
	var found int
	for range blobs.List(t.Context(), "") {
		found++
	}
	if found != 0 {
		t.Errorf("%d blobs left behind by a refused write", found)
	}

	if _, err := s.Mkdir(t.Context(), owner, "album/inner"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("Mkdir into a missing directory = %v, want ErrNotFound", err)
	}
}

func TestParentMustBeADirectory(t *testing.T) {
	t.Parallel()
	s, _ := service(t)
	write(t, s, "notes.txt", "not a directory")

	_, err := s.Write(t.Context(), owner, "notes.txt/inner.txt", strings.NewReader("x"), 1, "text/plain")
	if !errors.Is(err, db.ErrConflict) {
		t.Errorf("Write under a file = %v, want ErrConflict", err)
	}

	// The other two writers, because this is the branch no database constraint
	// could take over: a foreign key would find the parent row and be satisfied
	// by it, file or directory.
	if _, err := s.Mkdir(t.Context(), owner, "notes.txt/inner"); !errors.Is(err, db.ErrConflict) {
		t.Errorf("Mkdir under a file = %v, want ErrConflict", err)
	}
	write(t, s, "photo.jpg", "bytes")
	if err := s.Move(t.Context(), owner, "photo.jpg", "notes.txt/photo.jpg"); !errors.Is(err, db.ErrConflict) {
		t.Errorf("Move under a file = %v, want ErrConflict", err)
	}
}

func TestMkdirThenWriteInside(t *testing.T) {
	t.Parallel()
	s, _ := service(t)

	dir, err := s.Mkdir(t.Context(), owner, "album")
	if err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if !dir.IsDir {
		t.Error("Mkdir returned something that is not a directory")
	}
	write(t, s, "album/photo.jpg", "bytes")

	listing, err := s.List(t.Context(), owner, "album")
	if err != nil {
		t.Fatal(err)
	}
	if len(listing) != 1 || listing[0].Path != "album/photo.jpg" {
		t.Errorf("listing = %+v", listing)
	}
}

// TestListPage is the one thing this layer adds to the port's paging: whether
// there is another page. It is answered by asking for a row more than was
// wanted and dropping it, which is why the extra row must never reach the
// caller.
func TestListPage(t *testing.T) {
	t.Parallel()
	s, _ := service(t)
	if _, err := s.Mkdir(t.Context(), owner, "album"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.jpg", "b.jpg", "c.jpg"} {
		write(t, s, "album/"+name, "bytes")
	}

	first, more, err := s.ListPage(t.Context(), owner, "album", db.FileOrder{}, db.Cursor{}, 2)
	if err != nil {
		t.Fatalf("ListPage: %v", err)
	}
	if len(first) != 2 || first[0].Path != "album/a.jpg" || first[1].Path != "album/b.jpg" {
		t.Fatalf("first page = %+v, want two rows", first)
	}
	if !more {
		t.Error("the first page of three rows says it is the last")
	}

	last, more, err := s.ListPage(t.Context(), owner, "album", db.FileOrder{}, db.After(first[1]), 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(last) != 1 || last[0].Path != "album/c.jpg" {
		t.Errorf("second page = %+v, want the remaining row", last)
	}
	if more {
		t.Error("the last page offers another one")
	}

	if _, _, err := s.ListPage(t.Context(), owner, "album", db.FileOrder{}, db.Cursor{}, 0); err == nil {
		t.Error("a page of no rows was allowed")
	}
}

func TestOpenADirectory(t *testing.T) {
	t.Parallel()
	s, _ := service(t)
	if _, err := s.Mkdir(t.Context(), owner, "album"); err != nil {
		t.Fatal(err)
	}

	if _, _, err := s.Open(t.Context(), owner, "album"); !errors.Is(err, db.ErrConflict) {
		t.Errorf("Open on a directory = %v, want ErrConflict", err)
	}
}

func TestRemoveFile(t *testing.T) {
	t.Parallel()
	s, blobs := service(t)
	f := write(t, s, "doomed.txt", "bytes")

	if err := s.Remove(t.Context(), owner, "doomed.txt"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := s.Stat(t.Context(), owner, "doomed.txt"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("Stat after Remove = %v, want ErrNotFound", err)
	}
	// The row moved to the trash and **the blob did not move at all** (#274):
	// deleting costs no copying and no room, which is what makes keeping it
	// for a month affordable.
	if _, err := blobs.Stat(t.Context(), f.BlobKey); err != nil {
		t.Errorf("the bytes were destroyed by a delete: %v", err)
	}
	batches, err := s.Trash(t.Context(), owner, db.TrashCursor{}, 10)
	if err != nil {
		t.Fatalf("Trash: %v", err)
	}
	if len(batches) != 1 {
		t.Fatalf("the trash holds %d deletions, want the one", len(batches))
	}
	if got := batches[0]; got.Root != "doomed.txt" || got.Files != 1 || got.Bytes != 5 {
		t.Errorf("the deletion is %+v, want one file of five bytes at doomed.txt", got)
	}

	// And destroying it for good takes the bytes with it.
	if _, _, err := s.DestroyTrashed(t.Context(), owner, batches[0].ID); err != nil {
		t.Fatalf("DestroyTrashed: %v", err)
	}
	if _, err := blobs.Stat(t.Context(), f.BlobKey); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("the blob survived being destroyed: %v", err)
	}
}

// TestTheTrashKeepsOneEntryPerDeletion: a folder of a thousand photographs is
// a thousand rows and one accident, and the page shows accidents.
func TestTheTrashKeepsOneEntryPerDeletion(t *testing.T) {
	t.Parallel()
	s, _ := service(t)
	if _, err := s.Mkdir(t.Context(), owner, "album"); err != nil {
		t.Fatal(err)
	}
	write(t, s, "album/one.jpg", "one")
	write(t, s, "album/two.jpg", "two")
	write(t, s, "loose.txt", "x")

	if err := s.Remove(t.Context(), owner, "album"); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(t.Context(), owner, "loose.txt"); err != nil {
		t.Fatal(err)
	}

	batches, err := s.Trash(t.Context(), owner, db.TrashCursor{}, 10)
	if err != nil {
		t.Fatalf("Trash: %v", err)
	}
	if len(batches) != 2 {
		t.Fatalf("the trash holds %d deletions, want two", len(batches))
	}
	// A tree is named by the folder that was deleted rather than by the first
	// file in it. Which of the two comes first is not asserted here: they are
	// the same millisecond apart, so the order falls to the batch id, and the
	// case that walks a page in order is in the conformance suite, where the
	// clock is the test's.
	byRoot := map[string]db.TrashBatch{}
	for _, b := range batches {
		byRoot[b.Root] = b
	}
	if got := byRoot["album"]; got.Files != 2 {
		t.Errorf("the tree is %+v, want two files under album", got)
	}
	if got := byRoot["loose.txt"]; got.Files != 1 {
		t.Errorf("the single file is %+v, want one", got)
	}

	totals, err := s.TrashTotals(t.Context(), owner)
	if err != nil {
		t.Fatalf("TrashTotals: %v", err)
	}
	if totals.Files != 3 || totals.Bytes != 7 {
		t.Errorf("the trash totals %+v, want three files and seven bytes", totals)
	}
}

// TestTheTrashIsEmptiedByAge is what makes it a trash and not a leak.
func TestTheTrashIsEmptiedByAge(t *testing.T) {
	t.Parallel()
	s, blobs := service(t)
	f := write(t, s, "doomed.txt", "bytes")
	if err := s.Remove(t.Context(), owner, "doomed.txt"); err != nil {
		t.Fatal(err)
	}

	// Nothing is old enough yet.
	switch done, err := s.EmptyTrash(t.Context(), time.Now().Add(-time.Hour)); {
	case err != nil:
		t.Fatalf("EmptyTrash: %v", err)
	case done.Batches != 0:
		t.Errorf("it emptied %d deletions that are minutes old", done.Batches)
	}
	if _, err := blobs.Stat(t.Context(), f.BlobKey); err != nil {
		t.Fatalf("the bytes went early: %v", err)
	}

	switch done, err := s.EmptyTrash(t.Context(), time.Now()); {
	case err != nil:
		t.Fatalf("EmptyTrash: %v", err)
	case done.Batches != 1 || done.Files != 1 || done.Bytes != 5:
		t.Errorf("EmptyTrash = %+v, want the one deletion", done)
	}
	if _, err := blobs.Stat(t.Context(), f.BlobKey); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("the bytes outlived the trash: %v", err)
	}
	if batches, _ := s.Trash(t.Context(), owner, db.TrashCursor{}, 10); len(batches) != 0 {
		t.Errorf("the trash still lists %d deletions", len(batches))
	}
}

// TestRemoveTree is what DELETE on a collection means, and the reason the
// deletion runs inside a transaction: depth first, so every directory is empty
// by the time it is removed.
func TestRemoveTree(t *testing.T) {
	t.Parallel()
	s, blobs, meta := serviceOver(t)

	for _, dir := range []string{"album", "album/raw"} {
		if _, err := s.Mkdir(t.Context(), owner, dir); err != nil {
			t.Fatal(err)
		}
	}
	one := write(t, s, "album/one.jpg", "one")
	two := write(t, s, "album/raw/two.dng", "two")
	survivor := write(t, s, "keep.txt", "keep")

	if err := s.Remove(t.Context(), owner, "album"); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	for _, path := range []string{"album", "album/raw", "album/one.jpg", "album/raw/two.dng"} {
		if _, err := s.Stat(t.Context(), owner, path); !errors.Is(err, db.ErrNotFound) {
			t.Errorf("%q survived the recursive delete: %v", path, err)
		}
	}
	// The bytes are all still there, the deleted ones in the trash and the
	// survivor in the library.
	for _, key := range []string{one.BlobKey, two.BlobKey, survivor.BlobKey} {
		if _, err := blobs.Stat(t.Context(), key); err != nil {
			t.Errorf("blob %q was destroyed by a delete: %v", key, err)
		}
	}
	// Directories go into the trash too, with no bytes of their own, so that
	// putting the tree back is possible later (#275).
	batches, err := s.Trash(t.Context(), owner, db.TrashCursor{}, 10)
	if err != nil {
		t.Fatalf("Trash: %v", err)
	}
	if len(batches) != 1 || batches[0].Root != "album" {
		t.Fatalf("the trash holds %+v, want the one deletion of album", batches)
	}
	if got := batches[0].Files; got != 2 {
		t.Errorf("the deletion is %d files, want the two -- the folders are in it too but are not files", got)
	}
	rows, err := meta.TrashedIn(t.Context(), owner, batches[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	var dirs int
	for _, row := range rows {
		if row.IsDir {
			dirs++
		}
	}
	if len(rows) != 4 || dirs != 2 {
		t.Errorf("the deletion holds %d rows of which %d are folders, want four and two", len(rows), dirs)
	}
}

func TestMove(t *testing.T) {
	t.Parallel()
	s, _ := service(t)
	if _, err := s.Mkdir(t.Context(), owner, "album"); err != nil {
		t.Fatal(err)
	}
	before := write(t, s, "photo.jpg", "bytes")

	if err := s.Move(t.Context(), owner, "photo.jpg", "album/photo.jpg"); err != nil {
		t.Fatalf("Move: %v", err)
	}
	after, err := s.Stat(t.Context(), owner, "album/photo.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if after.BlobKey != before.BlobKey {
		t.Error("the move rewrote the blob key; only the row should have moved")
	}
	if got := read(t, s, "album/photo.jpg"); got != "bytes" {
		t.Errorf("read %q", got)
	}
	if err := s.Move(t.Context(), owner, "album/photo.jpg", "missing/photo.jpg"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("Move into a missing directory = %v, want ErrNotFound", err)
	}
}

// TestOpenSeeks covers the shape http.ServeContent depends on: it asks for the
// size by seeking to the end, then seeks back and reads. Getting this wrong
// means no range requests, which means no video seeking.
func TestOpenSeeks(t *testing.T) {
	t.Parallel()
	s, _ := service(t)
	const alphabet = "abcdefghijklmnopqrstuvwxyz"
	write(t, s, "alphabet", alphabet)

	body, _, err := s.Open(t.Context(), owner, "alphabet")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = body.Close() }()

	size, err := body.Seek(0, io.SeekEnd)
	if err != nil {
		t.Fatalf("Seek to the end: %v", err)
	}
	if size != int64(len(alphabet)) {
		t.Errorf("Seek(0, End) = %d, want %d", size, len(alphabet))
	}

	if _, serr := body.Seek(2, io.SeekStart); serr != nil {
		t.Fatalf("Seek back: %v", serr)
	}
	got := make([]byte, 3)
	if _, rerr := io.ReadFull(body, got); rerr != nil {
		t.Fatalf("ReadFull: %v", rerr)
	}
	if string(got) != "cde" {
		t.Errorf("read %q, want cde", got)
	}

	// Seeking backwards has to reopen rather than keep reading forwards.
	if _, serr := body.Seek(0, io.SeekStart); serr != nil {
		t.Fatal(serr)
	}
	all, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	if string(all) != alphabet {
		t.Errorf("read %q after seeking back", all)
	}
}

var _ = context.Background

func TestOpenMissing(t *testing.T) {
	t.Parallel()
	s, _ := service(t)
	if _, _, err := s.Open(t.Context(), owner, "nothing.txt"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("Open = %v, want ErrNotFound", err)
	}
}

// TestWriteReadsWhatItIsStoring is the other half of #146: the row a write
// leaves behind says what the bytes are, not what the name claimed.
//
// Two rules, and the second is the one that keeps this honest: what a client
// declared is kept, because being told beats guessing, and only silence or the
// application/octet-stream every WebDAV client sends is replaced.
func TestWriteReadsWhatItIsStoring(t *testing.T) {
	t.Parallel()

	jpeg := readTestdata(t, "cover.jpg")
	tests := []struct {
		name     string
		path     string
		body     []byte
		declared string
		wantMIME string
		wantKind string
	}{
		{
			name: "a client that said nothing", path: "one", body: jpeg,
			declared: "", wantMIME: "image/jpeg", wantKind: "image",
		},
		{
			name: "a client that shrugged", path: "two.bin", body: jpeg,
			declared: "application/octet-stream", wantMIME: "image/jpeg", wantKind: "image",
		},
		{
			name: "a client that was specific", path: "three.txt", body: jpeg,
			declared: "text/plain; charset=utf-8", wantMIME: "text/plain; charset=utf-8", wantKind: "image",
		},
		{
			name: "bytes that say nothing", path: "four.bin", body: []byte{0x00, 0xA5, 0x00, 0x5A},
			declared: "application/octet-stream", wantMIME: "application/octet-stream", wantKind: "other",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, _ := service(t)

			f, err := s.Write(t.Context(), owner, tt.path, bytes.NewReader(tt.body), int64(len(tt.body)), tt.declared)
			if err != nil {
				t.Fatal(err)
			}
			if f.MIMEType != tt.wantMIME {
				t.Errorf("MIMEType = %q, want %q", f.MIMEType, tt.wantMIME)
			}
			// And the blob is filed where somebody with no database would look
			// for it, which for a photograph with no extension is not other/.
			if kind, _, _ := strings.Cut(f.BlobKey, "/"); kind != tt.wantKind {
				t.Errorf("blob key = %q, want it filed under %q", f.BlobKey, tt.wantKind)
			}

			// The bytes are still all there: the head is read before the store
			// sees them and handed back in front of the rest.
			if got := read(t, s, tt.path); got != string(tt.body) {
				t.Errorf("%d bytes came back, want %d", len(got), len(tt.body))
			}
		})
	}
}

// readTestdata reads one of the fixtures the smoke suite uses, which are real
// files rather than bytes invented here.
func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "scripts", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// TestRestoreAFile puts one back where it came from.
func TestRestoreAFile(t *testing.T) {
	t.Parallel()
	s, blobs := service(t)
	before := write(t, s, "notes.txt", "hello")
	if err := s.Remove(t.Context(), owner, "notes.txt"); err != nil {
		t.Fatal(err)
	}

	back, err := s.Restore(t.Context(), owner, onlyBatch(t, s))
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if back.Path != "notes.txt" {
		t.Errorf("it came back at %q", back.Path)
	}
	// The row as it was, pointing at the bytes that never moved.
	if back.BlobKey != before.BlobKey || back.ETag != before.ETag || back.Size != before.Size {
		t.Errorf("it came back as %+v, want %+v", back, before)
	}
	if got := read(t, s, "notes.txt"); got != "hello" {
		t.Errorf("the restored file reads %q", got)
	}
	if _, err := blobs.Stat(t.Context(), before.BlobKey); err != nil {
		t.Errorf("the bytes are gone: %v", err)
	}
	// And it is out of the trash.
	if batches, _ := s.Trash(t.Context(), owner, db.TrashCursor{}, 10); len(batches) != 0 {
		t.Errorf("the deletion is still in the trash: %+v", batches)
	}
}

// TestRestoreLandsBesideWhatTookItsName: restoring is not a PUT, and whatever
// has the name now is not a mistake. A folder takes its contents with it.
func TestRestoreLandsBesideWhatTookItsName(t *testing.T) {
	t.Parallel()
	s, _ := service(t)
	if _, err := s.Mkdir(t.Context(), owner, "album"); err != nil {
		t.Fatal(err)
	}
	write(t, s, "album/one.jpg", "the first one")
	if err := s.Remove(t.Context(), owner, "album"); err != nil {
		t.Fatal(err)
	}

	// Somebody makes a new album with the same name and puts something in it.
	if _, err := s.Mkdir(t.Context(), owner, "album"); err != nil {
		t.Fatal(err)
	}
	write(t, s, "album/one.jpg", "a different one")

	back, err := s.Restore(t.Context(), owner, onlyBatch(t, s))
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if back.Path != "album (2)" {
		t.Fatalf("it came back at %q, want beside the one that took the name", back.Path)
	}
	if got := read(t, s, "album (2)/one.jpg"); got != "the first one" {
		t.Errorf("the restored file reads %q", got)
	}
	if got := read(t, s, "album/one.jpg"); got != "a different one" {
		t.Errorf("the file that was in the way reads %q", got)
	}
}

// TestRestoreRebuildsTheWayBack: the folders above it were part of the same
// tree until somebody deleted those too.
func TestRestoreRebuildsTheWayBack(t *testing.T) {
	t.Parallel()
	s, _ := service(t)
	for _, dir := range []string{"holiday", "holiday/album"} {
		if _, err := s.Mkdir(t.Context(), owner, dir); err != nil {
			t.Fatal(err)
		}
	}
	write(t, s, "holiday/album/one.jpg", "one")

	if err := s.Remove(t.Context(), owner, "holiday/album"); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(t.Context(), owner, "holiday"); err != nil {
		t.Fatal(err)
	}

	// The older of the two deletions is the album, whose parent is now gone.
	batches, err := s.Trash(t.Context(), owner, db.TrashCursor{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	var album string
	for _, b := range batches {
		if b.Root == "holiday/album" {
			album = b.ID
		}
	}
	if album == "" {
		t.Fatalf("the album is not in the trash: %+v", batches)
	}

	if _, err := s.Restore(t.Context(), owner, album); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if dir, err := s.Stat(t.Context(), owner, "holiday"); err != nil || !dir.IsDir {
		t.Errorf("the folder on the way back was not recreated: %+v %v", dir, err)
	}
	if got := read(t, s, "holiday/album/one.jpg"); got != "one" {
		t.Errorf("the restored file reads %q", got)
	}
}

// TestRestoreRefusesToDisplaceAFile: making room would mean deleting
// somebody's file, and this is the function that puts things back.
func TestRestoreRefusesToDisplaceAFile(t *testing.T) {
	t.Parallel()
	s, _ := service(t)
	if _, err := s.Mkdir(t.Context(), owner, "holiday"); err != nil {
		t.Fatal(err)
	}
	write(t, s, "holiday/one.jpg", "one")
	if err := s.Remove(t.Context(), owner, "holiday"); err != nil {
		t.Fatal(err)
	}
	// A file where the folder was.
	write(t, s, "holiday", "not a folder any more")

	// The root itself is taken by a file, so it lands beside it.
	back, err := s.Restore(t.Context(), owner, onlyBatch(t, s))
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if back.Path != "holiday (2)" {
		t.Errorf("it came back at %q", back.Path)
	}
	if got := read(t, s, "holiday"); got != "not a folder any more" {
		t.Errorf("the file in the way reads %q", got)
	}

	// But an ancestor that is a file now is a conflict: there is nowhere to
	// put the tree, and nothing here will delete a file to make room.
	write(t, s, "trip", "a file")
	if _, err := s.Mkdir(t.Context(), owner, "away"); err != nil {
		t.Fatal(err)
	}
	write(t, s, "away/photo.jpg", "x")
	if err := s.Remove(t.Context(), owner, "away/photo.jpg"); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(t.Context(), owner, "away"); err != nil {
		t.Fatal(err)
	}
	// Move the file into the way by renaming it over where "away" was.
	if err := s.Move(t.Context(), owner, "trip", "away"); err != nil {
		t.Fatal(err)
	}

	var photo string
	batches, _ := s.Trash(t.Context(), owner, db.TrashCursor{}, 10)
	for _, b := range batches {
		if b.Root == "away/photo.jpg" {
			photo = b.ID
		}
	}
	if _, err := s.Restore(t.Context(), owner, photo); !errors.Is(err, db.ErrConflict) {
		t.Errorf("Restore under a file = %v, want ErrConflict", err)
	}
	if got := read(t, s, "away"); got != "a file" {
		t.Errorf("the file in the way reads %q", got)
	}
}

// TestRestoreOfNothing: a deletion that is not there is not found, and a
// button pressed twice says so rather than restoring something twice.
func TestRestoreOfNothing(t *testing.T) {
	t.Parallel()
	s, _ := service(t)
	if _, err := s.Restore(t.Context(), owner, "never-existed"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("Restore of nothing = %v, want ErrNotFound", err)
	}
}

// onlyBatch is the one deletion in the trash, for the cases that made exactly
// one.
func onlyBatch(t *testing.T, s *files.Service) string {
	t.Helper()
	batches, err := s.Trash(t.Context(), owner, db.TrashCursor{}, 10)
	if err != nil {
		t.Fatalf("Trash: %v", err)
	}
	if len(batches) != 1 {
		t.Fatalf("the trash holds %d deletions, want one", len(batches))
	}
	return batches[0].ID
}

// TestFreeNameNumbersRatherThanReplaces is the rule the import folder and the
// share target both read: a write that must not destroy what is there lands
// beside it, and the search for somewhere to land is bounded.
func TestFreeNameNumbersRatherThanReplaces(t *testing.T) {
	t.Parallel()
	s, _ := service(t)

	free, err := s.FreeName(t.Context(), owner, "scan.pdf")
	if err != nil || free != "scan.pdf" {
		t.Fatalf("FreeName of a name nobody has = %q, %v", free, err)
	}

	write(t, s, "scan.pdf", "the first one")
	switch free, err = s.FreeName(t.Context(), owner, "scan.pdf"); {
	case err != nil:
		t.Fatal(err)
	case free != "scan (2).pdf":
		t.Errorf("FreeName = %q, want the number before the extension", free)
	}

	// The bound: a folder holding a hundred of one name is a loop somewhere
	// else, and this says so rather than walking it on every pass.
	for n := 2; n <= files.MaxCopies; n++ {
		write(t, s, db.CopyName("scan.pdf", n), "another")
	}
	if _, err := s.FreeName(t.Context(), owner, "scan.pdf"); err == nil ||
		!strings.Contains(err.Error(), "is taken") {
		t.Errorf("FreeName with every name taken = %v, want a refusal", err)
	}
}

// TestMkdirAllMakesWhatIsMissing: the walk two doors share, which is why it is
// here rather than in either of them — the import folder mirroring a tree, and
// a share landing under a dated folder.
func TestMkdirAllMakesWhatIsMissing(t *testing.T) {
	t.Parallel()
	s, _ := service(t)

	if err := s.MkdirAll(t.Context(), owner, "shared/2026/10"); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"shared", "shared/2026", "shared/2026/10"} {
		switch f, err := s.Stat(t.Context(), owner, dir); {
		case err != nil:
			t.Errorf("%q was not made: %v", dir, err)
		case !f.IsDir:
			t.Errorf("%q is not a directory", dir)
		}
	}

	// Again, because a second share in the same month arrives at a folder that
	// is already there and that is not a conflict.
	if err := s.MkdirAll(t.Context(), owner, "shared/2026/10"); err != nil {
		t.Errorf("making a folder that is already there: %v", err)
	}
	// The root is not a row and there is nothing to make.
	if err := s.MkdirAll(t.Context(), owner, ""); err != nil {
		t.Errorf("MkdirAll of nothing: %v", err)
	}

	// A file where a directory has to be is said plainly, because the generic
	// conflict sends somebody looking for a thing that is not there.
	write(t, s, "notes.txt", "hello")
	switch err := s.MkdirAll(t.Context(), owner, "notes.txt/inner"); {
	case err == nil:
		t.Error("a file was walked into as though it were a folder")
	case !strings.Contains(err.Error(), "notes.txt"):
		t.Errorf("MkdirAll through a file = %v, want the path named", err)
	}
}

// TestTheWalkAndTheNameNeedTheStore: both ask what is already there, so a
// database that will not answer stops them rather than guessing.
func TestTheWalkAndTheNameNeedTheStore(t *testing.T) {
	t.Parallel()
	blobs, meta := breakable(t)
	s := files.New(blobs, dbtest.FailOn(t, meta, "FileByPath"))

	if err := s.MkdirAll(t.Context(), owner, "shared/2026"); err == nil {
		t.Error("MkdirAll over a store that cannot answer succeeded")
	}
	if _, err := s.FreeName(t.Context(), owner, "scan.pdf"); err == nil {
		t.Error("FreeName over a store that cannot answer succeeded")
	}
}
