package dav

import (
	"errors"
	"io/fs"
	"os"
	"testing"
	"time"

	xnet "golang.org/x/net/webdav"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// TestReadOnlyFSRefusesEverythingThatWrites is the claim propfind.go makes
// about itself: only PROPFIND is routed to this filesystem, so the rest of the
// interface is unreachable. "Unreachable" is a claim, and this is the only way
// to hold it -- if one of these ever starts being called, it fails loudly here
// rather than half-writing through a path nobody meant to open.
func TestReadOnlyFSRefusesEverythingThatWrites(t *testing.T) {
	t.Parallel()
	roFS := &readOnlyFS{listed: map[string]db.File{}}

	if err := roFS.Mkdir(t.Context(), "album", 0o755); !errors.Is(err, errReadOnly) {
		t.Errorf("Mkdir = %v", err)
	}
	if err := roFS.RemoveAll(t.Context(), "album"); !errors.Is(err, errReadOnly) {
		t.Errorf("RemoveAll = %v", err)
	}
	if err := roFS.Rename(t.Context(), "a", "b"); !errors.Is(err, errReadOnly) {
		t.Errorf("Rename = %v", err)
	}
	if _, err := roFS.OpenFile(t.Context(), "a", os.O_RDWR, 0); !errors.Is(err, errReadOnly) {
		t.Errorf("OpenFile for writing = %v", err)
	}

	f := &readOnlyFile{fs: roFS, row: db.File{Path: "album/one.txt"}}
	if _, err := f.Read(make([]byte, 4)); !errors.Is(err, errReadOnly) {
		t.Errorf("Read = %v", err)
	}
	if _, err := f.Write([]byte("x")); !errors.Is(err, errReadOnly) {
		t.Errorf("Write = %v", err)
	}
	if _, err := f.Seek(0, 0); !errors.Is(err, errReadOnly) {
		t.Errorf("Seek = %v", err)
	}
	if _, err := f.Readdir(0); !errors.Is(err, errReadOnly) {
		t.Errorf("Readdir of a file = %v", err)
	}
	// Properties are facts about a file, not notes a client leaves on it.
	if _, err := f.Patch([]xnet.Proppatch{}); !errors.Is(err, errReadOnly) {
		t.Errorf("Patch = %v", err)
	}
	if err := f.Close(); err != nil {
		t.Errorf("Close = %v", err)
	}
}

// TestTheOtherLibraryNoLongerLists is the other half of the split, from the
// side that gave the method up: emersion's ReadDir had one caller and it was
// its own PROPFIND.
func TestTheOtherLibraryNoLongerLists(t *testing.T) {
	t.Parallel()
	fs := &fileSystem{}
	if _, err := fs.ReadDir(t.Context(), "/album", false); !errors.Is(err, errNotThisLibrary) {
		t.Errorf("ReadDir = %v, want a refusal", err)
	}
}

// TestRowInfo is the shape x/net reads a row through, and the two fields it
// asks for that a db.File has no opinion about.
func TestRowInfo(t *testing.T) {
	t.Parallel()

	when := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	file := rowInfo{db.File{Path: "album/one.txt", Size: 12, MTime: when}}
	if file.Name() != "one.txt" || file.Size() != 12 || file.IsDir() || !file.ModTime().Equal(when) {
		t.Errorf("a file reads as %+v", file)
	}
	if file.Mode()&fs.ModeDir != 0 {
		t.Errorf("a file has a directory bit: %v", file.Mode())
	}
	// Sys carries the row itself, which is the escape hatch os.FileInfo has for
	// exactly this: nothing here needs it, and a caller that did would get the
	// truth rather than a reconstruction.
	if row, ok := file.Sys().(db.File); !ok || row.Path != "album/one.txt" {
		t.Errorf("Sys = %v", file.Sys())
	}

	dir := rowInfo{db.File{Path: "album", IsDir: true}}
	if !dir.IsDir() || dir.Mode()&fs.ModeDir == 0 {
		t.Errorf("a directory reads as %v", dir.Mode())
	}
}
