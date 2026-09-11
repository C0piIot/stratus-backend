package disk_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/C0piIot/stratus-backend/internal/storage"
	"github.com/C0piIot/stratus-backend/internal/storage/disk"
)

// What this backend does when the filesystem says no.
//
// None of it needs a fake. A disk backend fails because the operating system
// refuses, so the tests make it refuse: a path component that is a file, a
// directory with no write bit, an object that is really a directory. That is
// the same technique scripts/smoke.sh uses on the data directory, and it is why
// #53's proposed fault injector is the wrong tool here -- a wrapper around
// storage.Storage cannot help the package that *is* storage.Storage.

// requireNotRoot fails rather than skips.
//
// Root ignores permission bits, so every case below would pass without checking
// anything -- coverage of a branch that never ran. A skip would say so in a line
// nobody reads; a failure says it where it cannot be missed. The Makefile
// already runs the toolchain as the invoking user (-u $(id -u)), and
// scripts/smoke.sh has the same note about its own unwritable-directory cases,
// so in practice this never fires.
func requireNotRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Fatal("this test asserts that the filesystem refuses, and root is never refused: " +
			"run the suite as an ordinary user, which is what the Makefile does")
	}
}

func TestNewRefusesWhatItCannotCreate(t *testing.T) {
	t.Parallel()
	requireNotRoot(t)

	t.Run("a path component that is a file", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		blocker := filepath.Join(dir, "blocker")
		if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		// MkdirAll cannot make a directory underneath a regular file.
		if _, err := disk.New(filepath.Join(blocker, "blobs")); err == nil {
			t.Error("New under a regular file returned no error")
		}
	})

	t.Run("a parent with no write bit", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

		if _, err := disk.New(filepath.Join(dir, "blobs")); err == nil {
			t.Error("New in a read-only directory returned no error")
		}
	})

	t.Run("the reserved directory is a file", func(t *testing.T) {
		t.Parallel()
		dir := filepath.Join(t.TempDir(), "blobs")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		// Something took the name the backend reserves for uploads in flight.
		if err := os.WriteFile(filepath.Join(dir, ".tmp"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}

		if _, err := disk.New(dir); err == nil {
			t.Error("New over a reserved name that is a file returned no error")
		}
	})
}

// TestNewRefusesAnUnsweepableTemp is the other half of opening: the sweep of
// interrupted uploads runs before the store is handed back, so a sweep that
// cannot finish has to stop the process rather than leave it running over a
// directory it could not read.
func TestNewRefusesAnUnsweepableTemp(t *testing.T) {
	t.Parallel()
	requireNotRoot(t)

	t.Run("a temp directory that cannot be read", func(t *testing.T) {
		t.Parallel()
		dir := filepath.Join(t.TempDir(), "blobs")
		tmp := filepath.Join(dir, ".tmp")
		if err := os.MkdirAll(tmp, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(tmp, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(tmp, 0o700) })

		if _, err := disk.New(dir); err == nil {
			t.Error("New over an unreadable reserved directory returned no error")
		}
	})

	t.Run("something in it that will not delete", func(t *testing.T) {
		t.Parallel()
		dir := filepath.Join(t.TempDir(), "blobs")
		// A non-empty directory inside the reserved one: Remove refuses it, and
		// the sweep expects to be removing files an interrupted Put left.
		if err := os.MkdirAll(filepath.Join(dir, ".tmp", "stuck", "inner"), 0o700); err != nil {
			t.Fatal(err)
		}

		if _, err := disk.New(dir); err == nil {
			t.Error("New with an undeletable entry in the reserved directory returned no error")
		}
	})
}

// TestGetOfSomethingThatIsNotAFile: a key naming a directory is not an object,
// and answering ErrNotFound is what keeps a caller from reading a directory as
// if it were bytes.
func TestGetOfSomethingThatIsNotAFile(t *testing.T) {
	t.Parallel()
	s, dir := newStore(t)

	if err := os.MkdirAll(filepath.Join(dir, "a", "b"), 0o700); err != nil {
		t.Fatal(err)
	}

	if _, _, err := s.Get(t.Context(), "a/b", storage.All()); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Get of a directory = %v, want ErrNotFound", err)
	}
	if _, err := s.Stat(t.Context(), "a/b"); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Stat of a directory = %v, want ErrNotFound", err)
	}
}

// TestListStopsWhenItCannotDescend: a listing that met a directory it may not
// read reports it rather than answering a short list, because a short list is
// what the collector would act on -- and acting on one means deleting live
// blobs.
func TestListStopsWhenItCannotDescend(t *testing.T) {
	t.Parallel()
	requireNotRoot(t)
	s, dir := newStore(t)

	if _, err := s.Put(t.Context(), "a/one", strings.NewReader("one"), -1); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "a"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dir, "a"), 0o700) })

	var failed bool
	for _, err := range s.List(t.Context(), "") {
		if err != nil {
			failed = true
		}
	}
	if !failed {
		t.Error("List walked past a directory it could not read without saying so")
	}
}
