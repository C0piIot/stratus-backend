package incoming_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/db/sqlite"
	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/incoming"
	"github.com/C0piIot/stratus-backend/internal/storage/disk"
)

const owner = "edu"

// watcher is a sweep over a real file layer: real blobs, real rows. An importer
// tested against a fake file layer would be testing the fake, which is the rule
// the rest of this project's adapters are tested by.
func watcher(t *testing.T) (*incoming.Watcher, *files.Service, string) {
	t.Helper()
	root := t.TempDir()

	blobs, err := disk.New(filepath.Join(root, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blobs.Close() })

	meta, err := sqlite.New(t.Context(), filepath.Join(root, "stratus.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = meta.Close() })
	if err := meta.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(root, "incoming")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	service := files.New(blobs, meta)
	return incoming.New(dir, owner, service), service, dir
}

// drop writes a file into the import directory, making the folders above it.
func drop(t *testing.T, dir, name, body string) string {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return full
}

// pass runs one sweep and fails on anything it could not do.
func pass(t *testing.T, w *incoming.Watcher) int {
	t.Helper()
	imported, err := w.Pass(t.Context())
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	return imported
}

// read is what the library holds at path.
func read(t *testing.T, s *files.Service, path string) string {
	t.Helper()
	f, err := s.Stat(t.Context(), owner, path)
	if err != nil {
		t.Fatalf("Stat %q: %v", path, err)
	}
	body, err := s.OpenFile(t.Context(), f)
	if err != nil {
		t.Fatalf("open %q: %v", path, err)
	}
	defer func() { _ = body.Close() }()
	out, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// TestAFileIsTakenOnceItHasStoppedGrowing is the whole contract: two passes
// that agree about the size, and only then is it moved.
func TestAFileIsTakenOnceItHasStoppedGrowing(t *testing.T) {
	t.Parallel()
	w, s, dir := watcher(t)
	on := drop(t, dir, "scan.pdf", "half")

	if imported := pass(t, w); imported != 0 {
		t.Fatalf("the first pass took %d files, want none: nothing has been seen twice yet", imported)
	}
	if _, err := os.Stat(on); err != nil {
		t.Errorf("a file still arriving was moved: %v", err)
	}

	// Still being written, so the second pass measures something else.
	if err := os.WriteFile(on, []byte("the whole thing"), 0o600); err != nil {
		t.Fatal(err)
	}
	if imported := pass(t, w); imported != 0 {
		t.Fatalf("a file that changed size was taken: %d", imported)
	}

	if imported := pass(t, w); imported != 1 {
		t.Fatalf("a file that settled was not taken: %d", imported)
	}
	if got := read(t, s, "scan.pdf"); got != "the whole thing" {
		t.Errorf("the library holds %q", got)
	}
	if _, err := os.Stat(on); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the file is still on disk: %v", err)
	}
}

// TestFoldersAreMirrored: what somebody arranged on disk arrives arranged, and
// the folders they made stay where they are for the next drop.
func TestFoldersAreMirrored(t *testing.T) {
	t.Parallel()
	w, s, dir := watcher(t)
	drop(t, dir, "holiday/2024/photo.jpg", "pixels")

	pass(t, w)
	if imported := pass(t, w); imported != 1 {
		t.Fatalf("imported %d", imported)
	}

	if got := read(t, s, "holiday/2024/photo.jpg"); got != "pixels" {
		t.Errorf("the library holds %q", got)
	}
	for _, d := range []string{"holiday", "holiday/2024"} {
		f, err := s.Stat(t.Context(), owner, d)
		if err != nil || !f.IsDir {
			t.Errorf("%q is not a folder in the library: %+v %v", d, f, err)
		}
	}
	// The folder on disk is left alone: a scanner writing into the same one
	// every day would otherwise find it gone.
	if _, err := os.Stat(filepath.Join(dir, "holiday", "2024")); err != nil {
		t.Errorf("the folder on disk was removed: %v", err)
	}
}

// TestATakenNameIsNotOverwritten: everywhere else a write to a path that exists
// replaces it, because that is what a PUT means. This is a folder two machines
// drop files into, and a scanner reusing a filename must not destroy the last
// one.
func TestATakenNameIsNotOverwritten(t *testing.T) {
	t.Parallel()
	w, s, dir := watcher(t)
	if _, err := s.Write(t.Context(), owner, "scan.pdf", strings.NewReader("the first"), 9, ""); err != nil {
		t.Fatal(err)
	}

	drop(t, dir, "scan.pdf", "the second")
	pass(t, w)
	pass(t, w)

	if got := read(t, s, "scan.pdf"); got != "the first" {
		t.Errorf("the original says %q, and should not have been touched", got)
	}
	if got := read(t, s, "scan (2).pdf"); got != "the second" {
		t.Errorf("the copy says %q", got)
	}

	// And again, which is the third name.
	drop(t, dir, "scan.pdf", "the third")
	pass(t, w)
	pass(t, w)
	if got := read(t, s, "scan (3).pdf"); got != "the third" {
		t.Errorf("the second copy says %q", got)
	}
}

// TestWhatIsNotImported: hidden names are half-written downloads and a Mac's
// litter, and a directory is not a file.
func TestWhatIsNotImported(t *testing.T) {
	t.Parallel()
	w, s, dir := watcher(t)
	drop(t, dir, ".DS_Store", "litter")
	drop(t, dir, "film.mkv.part", "half a film")
	drop(t, dir, ".hidden/secret.txt", "in a hidden folder")
	drop(t, dir, "notes.txt", "a real one")
	if err := os.MkdirAll(filepath.Join(dir, "empty"), 0o750); err != nil {
		t.Fatal(err)
	}

	pass(t, w)
	if imported := pass(t, w); imported != 2 {
		t.Fatalf("imported %d, want the two that are not hidden", imported)
	}
	for _, gone := range []string{".DS_Store", ".hidden/secret.txt", "empty"} {
		if _, err := s.Stat(t.Context(), owner, gone); !errors.Is(err, db.ErrNotFound) {
			t.Errorf("%q was imported", gone)
		}
	}
	// The .part is not hidden by name, only by convention, so it is taken --
	// which is what the size check is for and worth saying out loud.
	if got := read(t, s, "film.mkv.part"); got != "half a film" {
		t.Errorf("film.mkv.part = %q", got)
	}
}

// TestAFailureLeavesTheFileWhereItIs, and the pass carries on through the rest
// of the folder rather than stopping at the first thing it cannot do.
func TestAFailureLeavesTheFileWhereItIs(t *testing.T) {
	t.Parallel()
	w, s, dir := watcher(t)
	// A file in the library where this one needs a folder: nothing can mirror
	// into it, and the file on disk has nowhere to go.
	if _, err := s.Write(t.Context(), owner, "holiday", strings.NewReader("not a folder"), 12, ""); err != nil {
		t.Fatal(err)
	}
	blocked := drop(t, dir, "holiday/photo.jpg", "pixels")
	drop(t, dir, "notes.txt", "fine")

	w.Pass(t.Context()) //nolint:errcheck // the first pass only measures.
	imported, err := w.Pass(t.Context())
	if err == nil {
		t.Fatal("a pass that could not import a file reported no error")
	}
	if imported != 1 {
		t.Errorf("imported %d, want the one that could be", imported)
	}
	if _, err := os.Stat(blocked); err != nil {
		t.Errorf("the file that could not be imported was removed: %v", err)
	}
	if state := w.State(); state.Waiting != 1 || state.LastError == "" || state.Imported != 1 {
		t.Errorf("state = %+v, want one waiting, one imported and something to read", state)
	}

	// And it is tried again on the next pass rather than waiting another
	// interval to be measured first.
	if _, err := w.Pass(t.Context()); err == nil {
		t.Error("the file that failed was not tried again")
	}
}

// TestNothingToDo is the ordinary case on an empty folder: no error, no state
// to report, and nothing said.
func TestNothingToDo(t *testing.T) {
	t.Parallel()
	w, _, _ := watcher(t)

	if imported := pass(t, w); imported != 0 {
		t.Errorf("imported %d from an empty folder", imported)
	}
	if state := w.State(); state.Waiting != 0 || state.LastError != "" || state.LastRun.IsZero() {
		t.Errorf("state = %+v", state)
	}
}

// TestAFolderThatIsNotThere is a configuration somebody got wrong, and it has
// to be visible rather than silent.
func TestAFolderThatIsNotThere(t *testing.T) {
	t.Parallel()
	w := incoming.New(filepath.Join(t.TempDir(), "nowhere"), owner, nil)

	if _, err := w.Pass(t.Context()); err == nil {
		t.Error("sweeping a folder that does not exist reported no error")
	}
	if w.State().LastError == "" {
		t.Error("and said nothing about it on the page")
	}
}

// refusing is a file layer that will not take anything, for the arms a working
// one cannot reach: the store is the half of this that fails in production --
// a full disk, a bucket that will not answer -- and the point is that the file
// stays on disk and the pass carries on.
type refusing struct{ err error }

func (r refusing) Write(context.Context, string, string, io.Reader, int64, string) (db.File, error) {
	return db.File{}, r.err
}
func (r refusing) Stat(context.Context, string, string) (db.File, error) {
	return db.File{}, db.ErrNotFound
}
func (r refusing) Mkdir(context.Context, string, string) (db.File, error) {
	return db.File{}, r.err
}

func TestAStoreThatWillNotTakeAnythingKeepsEverything(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w := incoming.New(dir, owner, refusing{err: errors.New("no room left on device")})
	if w.Dir() != dir {
		t.Errorf("Dir = %q, want %q", w.Dir(), dir)
	}
	drop(t, dir, "one.txt", "a")
	drop(t, dir, "two.txt", "b")

	w.Pass(t.Context()) //nolint:errcheck // the first pass only measures.
	imported, err := w.Pass(t.Context())
	if err == nil || !strings.Contains(err.Error(), "no room left") {
		t.Fatalf("Pass = %v, want the store's own reason", err)
	}
	if imported != 0 {
		t.Errorf("imported %d", imported)
	}
	for _, name := range []string{"one.txt", "two.txt"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s was removed although it was never stored: %v", name, err)
		}
	}
	if state := w.State(); state.Waiting != 2 || state.Imported != 0 {
		t.Errorf("state = %+v, want both still waiting", state)
	}
}

// breaking answers every lookup with a failure, which is the one thing a
// working store cannot be made to do: the folder has to survive a database that
// will not say whether a name is taken.
type breaking struct{ err error }

func (b breaking) Write(context.Context, string, string, io.Reader, int64, string) (db.File, error) {
	return db.File{}, b.err
}
func (b breaking) Stat(context.Context, string, string) (db.File, error) { return db.File{}, b.err }
func (b breaking) Mkdir(context.Context, string, string) (db.File, error) {
	return db.File{}, b.err
}

// TestWhatCannotBeFiled: a name this server could never store, and a database
// that will not answer whether a name is free. Both leave the file alone.
func TestWhatCannotBeFiled(t *testing.T) {
	t.Parallel()

	t.Run("a name no path can hold", func(t *testing.T) {
		t.Parallel()
		w, s, dir := watcher(t)
		// Legal on this filesystem and not in the library, which is the gap
		// this has to refuse rather than mangle.
		named := drop(t, dir, "two\nlines.txt", "whatever")

		w.Pass(t.Context()) //nolint:errcheck // the first pass only measures.
		if _, err := w.Pass(t.Context()); err == nil {
			t.Fatal("a name with a control character in it was accepted")
		}
		if _, err := os.Stat(named); err != nil {
			t.Errorf("the file was removed: %v", err)
		}
		// Nothing of it reached the library, which here means the library has
		// nothing at all: the name it would have been stored under is one the
		// file layer refuses to be asked about.
		if rows, err := s.List(t.Context(), owner, ""); err != nil || len(rows) != 0 {
			t.Errorf("the library holds %+v (%v), want nothing", rows, err)
		}
	})

	t.Run("a database that will not answer", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		w := incoming.New(dir, owner, breaking{err: errors.New("the database is down")})
		named := drop(t, dir, "holiday/photo.jpg", "pixels")

		w.Pass(t.Context()) //nolint:errcheck // the first pass only measures.
		if _, err := w.Pass(t.Context()); err == nil {
			t.Fatal("a pass over a database that will not answer reported no error")
		}
		if _, err := os.Stat(named); err != nil {
			t.Errorf("the file was removed: %v", err)
		}
	})
}
