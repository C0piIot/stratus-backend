package dav_test

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/dav"
	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/db/sqlite"
	"github.com/C0piIot/stratus-backend/internal/files"
)

const trashPrefix = "/trash/"

// trashServer is the mount over the real file layer, so that what it lists is
// what deleting actually wrote.
func trashServer(t *testing.T) (http.Handler, *files.Service, *sqlite.Store) {
	t.Helper()
	svc, meta := service(t)
	return withUser(dav.Trash(trashPrefix, svc), "edu"), svc, meta
}

func put(t *testing.T, svc *files.Service, path, body string) {
	t.Helper()
	if _, err := svc.Write(t.Context(), "edu", path, strings.NewReader(body),
		int64(len(body)), "text/plain"); err != nil {
		t.Fatal(err)
	}
}

func remove(t *testing.T, svc *files.Service, path string) {
	t.Helper()
	if err := svc.Remove(t.Context(), "edu", path); err != nil {
		t.Fatal(err)
	}
}

// TestTheTrashIsACollectionOfDeletions is #323: a client that mounts this
// server sees what was deleted, one collection per accident, with the tree
// inside it exactly as it was.
func TestTheTrashIsACollectionOfDeletions(t *testing.T) {
	t.Parallel()
	h, svc, _ := trashServer(t)
	if _, err := svc.Mkdir(t.Context(), "edu", "album"); err != nil {
		t.Fatal(err)
	}
	put(t, svc, "album/one.txt", "one")
	put(t, svc, "album/two.txt", "two")
	put(t, svc, "notes.txt", "notes")
	remove(t, svc, "album")
	remove(t, svc, "notes.txt")

	// A deletion of a folder is a collection; a deletion of a file is the file.
	wantHrefs(t, hrefs(t, do(t, h, "PROPFIND", trashPrefix, "", "Depth", "1")),
		"/trash/", "/trash/album/", "/trash/notes.txt")

	// And the tree under it is the tree that was deleted, under its own names.
	wantHrefs(t, hrefs(t, do(t, h, "PROPFIND", "/trash/album", "", "Depth", "1")),
		"/trash/album/", "/trash/album/one.txt", "/trash/album/two.txt")

	// What the rows say about a deleted file is what the file said: nothing
	// about it changed when the row moved tables.
	props := propsOf(t, do(t, h, "PROPFIND", "/trash/album", "", "Depth", "1").Body.String())
	one := props["/trash/album/one.txt"]
	if one["getcontentlength"] != strconv.Itoa(len("one")) {
		t.Errorf("getcontentlength = %q, want 3", one["getcontentlength"])
	}
	if one["getcontenttype"] != "text/plain" {
		t.Errorf("getcontenttype = %q", one["getcontenttype"])
	}
	if one["getetag"] == "" {
		t.Error("a deleted file has no validator")
	}
}

// TestTwoDeletionsOfOneName: deleting "album" twice is what a trash is for,
// and the older keeps the name so that today's accident does not rename last
// week's under somebody reading it.
func TestTwoDeletionsOfOneName(t *testing.T) {
	t.Parallel()
	h, svc, _ := trashServer(t)
	for range 3 {
		put(t, svc, "album", "whatever")
		remove(t, svc, "album")
	}

	wantHrefs(t, hrefs(t, do(t, h, "PROPFIND", trashPrefix, "", "Depth", "1")),
		"/trash/", "/trash/album", "/trash/album (2)", "/trash/album (3)")
}

// TestTheSweepsFindsAreNotInTheCollection: a blob nobody can account for
// (#276) has no path, so there is nothing to call it here. It stays on the
// page, which can say so in a sentence.
func TestTheSweepsFindsAreNotInTheCollection(t *testing.T) {
	t.Parallel()
	h, svc, meta := trashServer(t)
	put(t, svc, "notes.txt", "notes")
	remove(t, svc, "notes.txt")
	// What the sweep parks: a key, a size, and no owner and no path, because
	// there was none to record.
	if err := meta.Trash(t.Context(), "swept", []db.File{{
		BlobKey: "an orphan", Size: 11, MTime: time.Now().UTC().Truncate(db.TimePrecision),
	}}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	// It belongs to nobody, and this tree is always somebody's.
	wantHrefs(t, hrefs(t, do(t, h, "PROPFIND", trashPrefix, "", "Depth", "1")),
		"/trash/", "/trash/notes.txt")
}

// TestTheTrashRefusesEveryWrite: restoring and destroying are the page's, and
// a write here would have nothing to mean.
func TestTheTrashRefusesEveryWrite(t *testing.T) {
	t.Parallel()
	h, svc, _ := trashServer(t)
	put(t, svc, "notes.txt", "notes")
	remove(t, svc, "notes.txt")

	for _, m := range []string{http.MethodPut, http.MethodDelete, "MKCOL", "MOVE", "COPY", "PROPPATCH", "LOCK"} {
		rec := do(t, h, m, "/trash/notes.txt", "")
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s = %d, want 405", m, rec.Code)
		}
		if got := rec.Header().Get("Allow"); got == "" {
			t.Errorf("%s: no Allow header on the refusal", m)
		}
	}

	// Class 1 and no LOCK: there is nothing here to hold.
	opts := do(t, h, http.MethodOptions, trashPrefix, "")
	if got := opts.Header().Get("DAV"); got != "1" {
		t.Errorf("DAV = %q, want 1", got)
	}
	if got := opts.Header().Get("Allow"); strings.Contains(got, "LOCK") {
		t.Errorf("Allow = %q, a read-only mount offers no locks", got)
	}
}

// TestNothingIsAtAnInventedName: the trash answers for what was deleted and
// not for everything asked of it.
func TestNothingIsAtAnInventedName(t *testing.T) {
	t.Parallel()
	h, svc, _ := trashServer(t)
	put(t, svc, "notes.txt", "notes")
	remove(t, svc, "notes.txt")

	for _, target := range []string{"/trash/album", "/trash/notes.txt/deeper", "/trash/album%20(2)"} {
		if got := do(t, h, "PROPFIND", target, "", "Depth", "1").Code; got != http.StatusNotFound {
			t.Errorf("PROPFIND %s = %d, want 404", target, got)
		}
	}
}

// TestTheTrashRefusesInfiniteDepth, like every other collection here: a client
// that meant the whole tree is told, rather than quietly given one level.
func TestTheTrashRefusesInfiniteDepth(t *testing.T) {
	t.Parallel()
	h, _, _ := trashServer(t)
	if got := do(t, h, "PROPFIND", trashPrefix, "", "Depth", "infinity").Code; got != http.StatusForbidden {
		t.Errorf("Depth: infinity = %d, want 403", got)
	}
}
