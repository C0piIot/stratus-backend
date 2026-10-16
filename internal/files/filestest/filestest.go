// Package filestest is the throwaway stack a test puts the file layer over:
// blobs on disk in a temporary directory, a SQLite database beside them,
// migrated, and the service that binds the two.
//
// It lives beside internal/files because that is the package that binds the
// two seams, and not in dbtest or storagetest, which are the conformance
// suites every driver's own tests import and which must not know that
// sqlite or disk exist. Before #363 this stack was written out by hand in
// twenty-seven test files, identical to the line.
//
// internal/files's own tests cannot use it -- a package may not import a
// test helper that imports it -- and keep the hand-written copy.
package filestest

import (
	"path/filepath"
	"testing"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/db/sqlite"
	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/storage"
	"github.com/C0piIot/stratus-backend/internal/storage/disk"
)

// Backends is the two seams on their own, each in a directory of its own
// under t.TempDir and closed when the test ends, for a test that puts a fault
// injector in front of one of them before building the service.
func Backends(t *testing.T) (storage.Storage, db.Store) {
	t.Helper()
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
	if err := meta.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return blobs, meta
}

// Stack is Backends with the service over them, which is what most tests
// want.
func Stack(t *testing.T) (storage.Storage, db.Store, *files.Service) {
	t.Helper()
	blobs, meta := Backends(t)
	return blobs, meta, files.New(blobs, meta)
}
