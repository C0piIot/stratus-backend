package sqlite_test

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/db/dbtest"
	"github.com/C0piIot/stratus-backend/internal/db/sqlite"

	_ "modernc.org/sqlite" // the raw handle the downgrade test needs
)

func TestConformance(t *testing.T) {
	t.Parallel()
	dbtest.Run(t, func(t *testing.T) db.Store { return newStore(t) })
}

func newStore(t *testing.T) *sqlite.Store {
	t.Helper()
	store, err := sqlite.New(t.Context(), filepath.Join(t.TempDir(), "stratus.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	if err := store.Migrate(t.Context()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return store
}

func TestNewRejectsAnEmptyPath(t *testing.T) {
	t.Parallel()
	if _, err := sqlite.New(t.Context(), ""); err == nil {
		t.Error("New = nil, want an error")
	}
}

// TestPathWithSpaces guards the DSN construction: the pragmas travel as query
// parameters, so a path that needs escaping is where a hand-built DSN breaks.
func TestPathWithSpaces(t *testing.T) {
	t.Parallel()
	store, err := sqlite.New(t.Context(), filepath.Join(t.TempDir(), "my library.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = store.Close() }()

	if err := store.Migrate(t.Context()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
}

// TestMigrateRefusesANewerSchema is the rollback case: downgrading the image is
// the first thing a self-hoster does when something breaks, and running against
// a schema written by a newer build has to stop the process rather than corrupt
// it quietly.
func TestMigrateRefusesANewerSchema(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "stratus.db")

	store, err := sqlite.New(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}

	// Pretend a newer Stratus has been here.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ierr := raw.ExecContext(t.Context(),
		"INSERT INTO schema_migrations (version, applied_at) VALUES (9999, CURRENT_TIMESTAMP)"); ierr != nil {
		t.Fatal(ierr)
	}
	if cerr := raw.Close(); cerr != nil {
		t.Fatal(cerr)
	}

	reopened, rerr := sqlite.New(t.Context(), path)
	if rerr != nil {
		t.Fatal(rerr)
	}
	defer func() { _ = reopened.Close() }()

	err = reopened.Migrate(t.Context())
	if err == nil {
		t.Fatal("Migrate = nil, want a refusal to run against a newer schema")
	}
	if !strings.Contains(err.Error(), "newer") {
		t.Errorf("the error should say the schema is newer, got %v", err)
	}
}

// TestBlobKeysOnAClosedStore covers the error path the collector would hit if
// it kept iterating through a shutdown: it reports rather than panics.
func TestBlobKeysOnAClosedStore(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	var got error
	for _, err := range store.BlobKeys(t.Context()) {
		got = err
		break
	}
	if got == nil {
		t.Error("iterating a closed store reported no error")
	}
}

// TestMusicOnAClosedStore covers the error path of every browse query at once.
// A working database does not fail a GROUP BY on request, so a store that has
// been shut under the query is the only way to reach them.
func TestMusicOnAClosedStore(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	calls := map[string]func() error{
		"Artists": func() error { _, err := store.Artists(t.Context(), "edu"); return err },
		"Albums":  func() error { _, err := store.Albums(t.Context(), "edu", ""); return err },
		"Tracks":  func() error { _, err := store.Tracks(t.Context(), "edu", "a", "b"); return err },
		"TracksIn": func() error {
			_, err := store.TracksIn(t.Context(), "edu", "music")
			return err
		},
		"TrackByFile": func() error {
			_, err := store.TrackByFile(t.Context(), "edu", 1)
			return err
		},
		"AlbumList": func() error {
			_, err := store.AlbumList(t.Context(), "edu",
				db.AlbumFilter{Order: db.AlbumsByName, Page: db.Page{Limit: 1}})
			return err
		},
		"TrackList": func() error {
			_, err := store.TrackList(t.Context(), "edu",
				db.TrackFilter{Order: db.TracksByPath, Page: db.Page{Limit: 1}})
			return err
		},
		"Genres": func() error { _, err := store.Genres(t.Context(), "edu"); return err },
		"Search": func() error {
			_, err := store.Search(t.Context(), "edu",
				db.SearchFilter{Text: "x", Artists: db.Page{Limit: 1}})
			return err
		},
	}
	for name, call := range calls {
		if err := call(); err == nil {
			t.Errorf("%s against a closed store reported no error", name)
		}
	}
}

// TestUploadsOnAClosedStore covers the error paths of the upload repository the
// same way TestBlobKeysOnAClosedStore covers the collector's: a shutdown under
// a request has to report rather than panic, and an upload is the longest-lived
// thing a client can be holding when one happens.
func TestUploadsOnAClosedStore(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	u := db.Upload{ID: "one", OwnerID: "edu", Path: "clip.mp4", Digest: []byte{}}
	if err := store.PutUpload(t.Context(), u); err == nil {
		t.Error("PutUpload on a closed store reported no error")
	}
	if _, err := store.UploadByID(t.Context(), "edu", "one"); err == nil {
		t.Error("UploadByID on a closed store reported no error")
	}
	if err := store.DeleteUpload(t.Context(), "edu", "one"); err == nil {
		t.Error("DeleteUpload on a closed store reported no error")
	}

	var got error
	for _, err := range store.ExpiredUploads(t.Context(), time.Now()) {
		got = err
		break
	}
	if got == nil {
		t.Error("iterating the expired uploads of a closed store reported no error")
	}
}
