package disk_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/C0piIot/stratus-backend/internal/storage"
	"github.com/C0piIot/stratus-backend/internal/storage/disk"
	"github.com/C0piIot/stratus-backend/internal/storage/storagetest"
)

// TestConformance is the point of this package: the disk backend has to satisfy
// the same contract as every other one.
func TestConformance(t *testing.T) {
	t.Parallel()
	storagetest.Run(t, func(t *testing.T) storage.Storage {
		s, _ := newStore(t)
		return s
	})
}

// newStore returns a store and the directory behind it, for the tests that need
// to look at the tree the backend is managing.
func newStore(t *testing.T) (*disk.Store, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "blobs")
	s, err := disk.New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s, dir
}

func TestNewCreatesTheTree(t *testing.T) {
	t.Parallel()
	_, dir := newStore(t)

	if fi, err := os.Stat(filepath.Join(dir, ".tmp")); err != nil || !fi.IsDir() {
		t.Errorf("Stat(.tmp) = %v, %v; want a directory", fi, err)
	}
	// Opening the same tree twice must work: a restart is not a special case.
	s2, err := disk.New(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if err := s2.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// TestSymlinkCannotEscapeTheRoot is the reason this backend is built on
// os.Root. Without it, a symlink planted in the tree -- by a sync client, a
// restore from a tarball, or a bug elsewhere -- would turn every Get into an
// arbitrary file read.
func TestSymlinkCannotEscapeTheRoot(t *testing.T) {
	t.Parallel()
	s, dir := newStore(t)

	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("classified"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(dir, "escape")); err != nil {
		t.Fatal(err)
	}

	if r, _, err := s.Get(t.Context(), "escape", storage.All()); err == nil {
		_ = r.Close()
		t.Error("Get followed a symlink out of the root")
	}
	if _, err := s.Stat(t.Context(), "escape"); err == nil {
		t.Error("Stat followed a symlink out of the root")
	}
	if got := keys(t, s, ""); len(got) != 0 {
		t.Errorf("List = %v, want nothing: a symlink is not an object", got)
	}
}

func TestSymlinkIsNotListed(t *testing.T) {
	t.Parallel()
	s, dir := newStore(t)

	if _, err := s.Put(t.Context(), "real", strings.NewReader("bytes"), 5); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(dir, "alias")); err != nil {
		t.Fatal(err)
	}

	if got := keys(t, s, ""); len(got) != 1 || got[0] != "real" {
		t.Errorf("List = %v, want [real]: an alias must not become a second object", got)
	}
}

// TestTempFilesAreInvisible covers the crash case: a temp file left behind by a
// process that died mid-upload is garbage, not an object.
func TestTempFilesAreInvisible(t *testing.T) {
	t.Parallel()
	s, dir := newStore(t)

	if err := os.WriteFile(filepath.Join(dir, ".tmp", "orphan"), []byte("half an upload"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := keys(t, s, ""); len(got) != 0 {
		t.Errorf("List = %v, want nothing", got)
	}

	// And the reserved directory is not reachable through the port at all.
	if _, err := s.Stat(t.Context(), ".tmp/orphan"); !errors.Is(err, storage.ErrInvalidKey) {
		t.Errorf("Stat(.tmp/orphan) = %v, want ErrInvalidKey", err)
	}
}

func TestDeletePrunesEmptyParents(t *testing.T) {
	t.Parallel()
	s, dir := newStore(t)

	for _, key := range []string{"photos/2024/06/a.jpg", "photos/2024/07/b.jpg"} {
		if _, err := s.Put(t.Context(), key, strings.NewReader("x"), 1); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Delete(t.Context(), "photos/2024/06/a.jpg"); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(dir, "photos", "2024", "06")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Stat(photos/2024/06) = %v, want it pruned", err)
	}
	// The prune stops as soon as a directory still holds something.
	if _, err := os.Stat(filepath.Join(dir, "photos", "2024", "07")); err != nil {
		t.Errorf("Stat(photos/2024/07) = %v, want it kept", err)
	}
}

// TestKeyCannotBeBothObjectAndPrefix documents a real divergence from S3, which
// is happy to hold "a/b" and "a" at once. A filesystem cannot, and pretending
// otherwise here would only move the failure somewhere less obvious.
func TestKeyCannotBeBothObjectAndPrefix(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)

	if _, err := s.Put(t.Context(), "a/b", strings.NewReader("x"), 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(t.Context(), "a", strings.NewReader("x"), 1); err == nil {
		t.Error("Put over a directory succeeded, want an error")
	}
	if _, err := s.Put(t.Context(), "a/b/c", strings.NewReader("x"), 1); err == nil {
		t.Error("Put under an object succeeded, want an error")
	}
}

func keys(t *testing.T, s storage.Storage, prefix string) []string {
	t.Helper()
	var out []string
	for info, err := range s.List(t.Context(), prefix) {
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		out = append(out, info.Key)
	}
	return out
}

// TestReopeningSweepsTemporaries covers the crash case. A Put that finishes
// renames its file out of the reserved directory, so anything still in there
// when the store opens belongs to a process that is no longer running.
func TestReopeningSweepsTemporaries(t *testing.T) {
	t.Parallel()
	_, dir := newStore(t)

	leftover := filepath.Join(dir, ".tmp", "half-an-upload")
	if err := os.WriteFile(leftover, []byte("interrupted"), 0o600); err != nil {
		t.Fatal(err)
	}

	reopened, err := disk.New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = reopened.Close() }()

	if _, err := os.Stat(leftover); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the temporary survived reopening: %v", err)
	}
	// And the real objects are untouched.
	if _, err := reopened.Put(t.Context(), "kept", strings.NewReader("x"), 1); err != nil {
		t.Fatal(err)
	}
	if got := keys(t, reopened, ""); len(got) != 1 || got[0] != "kept" {
		t.Errorf("the store holds %v", got)
	}
}
