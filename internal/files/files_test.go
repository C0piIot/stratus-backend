package files_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/db/sqlite"
	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/storage"
	"github.com/C0piIot/stratus-backend/internal/storage/disk"
)

const owner = "edu"

// service wires the real backends rather than fakes. Both are in-process, and
// the invariants under test are precisely the ones that only appear when two
// real seams are involved.
func service(t *testing.T) (*files.Service, storage.Storage) {
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
	return files.New(blobs, meta), blobs
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

// TestOverwriteKeepsTheOldBlobUntouched is the ordering rule made visible: a
// new key every time means an overwrite cannot destroy the previous content
// before the row that replaces it has committed.
func TestOverwriteKeepsTheOldBlobUntouched(t *testing.T) {
	t.Parallel()
	s, blobs := service(t)

	first := write(t, s, "notes.txt", "version one")
	second := write(t, s, "notes.txt", "version two")

	if first.BlobKey == second.BlobKey {
		t.Error("the overwrite reused the blob key, so the old content was destroyed in place")
	}
	if got := read(t, s, "notes.txt"); got != "version two" {
		t.Errorf("read %q, want the second version", got)
	}
	// The old blob is now garbage with nothing pointing at it, which is #17's
	// job and not an error here.
	if _, err := blobs.Stat(t.Context(), first.BlobKey); err != nil {
		t.Errorf("the previous blob is gone already: %v", err)
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
	// Row first, blob second: both are gone by the time Remove returns.
	if _, err := blobs.Stat(t.Context(), f.BlobKey); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("the blob survived the delete: %v", err)
	}
}

// TestRemoveTree is what DELETE on a collection means, and the reason the
// deletion runs inside a transaction: depth first, so every directory is empty
// by the time it is removed.
func TestRemoveTree(t *testing.T) {
	t.Parallel()
	s, blobs := service(t)

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
	for _, key := range []string{one.BlobKey, two.BlobKey} {
		if _, err := blobs.Stat(t.Context(), key); !errors.Is(err, storage.ErrNotFound) {
			t.Errorf("blob %q survived: %v", key, err)
		}
	}
	if _, err := blobs.Stat(t.Context(), survivor.BlobKey); err != nil {
		t.Errorf("an unrelated blob was deleted: %v", err)
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

// TestWalk is what PROPFIND with infinite depth reads: everything under a
// directory, with each directory followed immediately by its own contents.
func TestWalk(t *testing.T) {
	t.Parallel()
	s, _ := service(t)
	for _, dir := range []string{"album", "album/raw"} {
		if _, err := s.Mkdir(t.Context(), owner, dir); err != nil {
			t.Fatal(err)
		}
	}
	write(t, s, "album/one.jpg", "one")
	write(t, s, "album/raw/two.dng", "two")
	write(t, s, "top.txt", "top")

	got, err := s.Walk(t.Context(), owner, "album")
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	var paths []string
	for _, f := range got {
		paths = append(paths, f.Path)
	}
	want := []string{"album/one.jpg", "album/raw", "album/raw/two.dng"}
	if len(paths) != len(want) {
		t.Fatalf("Walk = %v, want %v", paths, want)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Errorf("Walk = %v, want %v", paths, want)
			break
		}
	}

	// From the root it reaches everything, and nothing twice.
	all, err := s.Walk(t.Context(), owner, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 5 {
		t.Errorf("Walk from the root returned %d entries, want 5", len(all))
	}
}

func TestOpenMissing(t *testing.T) {
	t.Parallel()
	s, _ := service(t)
	if _, _, err := s.Open(t.Context(), owner, "nothing.txt"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("Open = %v, want ErrNotFound", err)
	}
}
