package sqlite_test

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stratus-private-cloud/stratus-backend/internal/db"
	"github.com/stratus-private-cloud/stratus-backend/internal/db/dbtest"
	"github.com/stratus-private-cloud/stratus-backend/internal/db/sqlite"

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
// TestSQLiteTakesNoMigrationLock pins the nil the other two drivers do not
// pass (#242). A SQLite database is a file on one machine, so there is no
// second instance to exclude -- and saying so in a test is what keeps the
// absence from looking like an oversight the day a third engine arrives.
func TestSQLiteTakesNoMigrationLock(t *testing.T) {
	t.Parallel()
	if _, ok := any(newStore(t)).(db.MigrationLocker); ok {
		t.Error("the SQLite driver took a migration lock; its Migrate says why it should not")
	}
}

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

// TestAnnotationsOnAClosedStore covers the error paths of stars and ratings the
// way TestMusicOnAClosedStore covers browsing.
func TestAnnotationsOnAClosedStore(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	track, album := db.TrackSubject(1), db.AlbumSubject("a", "b")
	calls := map[string]func() error{
		"Star":       func() error { return store.Star(t.Context(), "edu", album, time.Now()) },
		"Unstar":     func() error { return store.Unstar(t.Context(), "edu", album) },
		"SetRating":  func() error { return store.SetRating(t.Context(), "edu", track, 3) },
		"RecordPlay": func() error { return store.RecordPlay(t.Context(), "edu", 1, time.Now()) },
		"AnnotationsOf tracks": func() error {
			_, err := store.AnnotationsOf(t.Context(), "edu", []db.Subject{track})
			return err
		},
		"AnnotationsOf tags": func() error {
			_, err := store.AnnotationsOf(t.Context(), "edu", []db.Subject{db.ArtistSubject("a")})
			return err
		},
		"AnnotationsOf albums": func() error {
			_, err := store.AnnotationsOf(t.Context(), "edu", []db.Subject{album})
			return err
		},
		"Starred": func() error { _, err := store.Starred(t.Context(), "edu"); return err },
	}
	for name, call := range calls {
		if err := call(); err == nil {
			t.Errorf("%s against a closed store reported no error", name)
		}
	}
}

// TestPlaylistsOnAClosedStore covers the error paths of the playlist
// repository the way TestMusicOnAClosedStore covers browsing.
func TestPlaylistsOnAClosedStore(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	calls := map[string]func() error{
		"CreatePlaylist": func() error {
			_, err := store.CreatePlaylist(t.Context(), db.Playlist{OwnerID: "edu", Name: "x", Created: now, Changed: now})
			return err
		},
		"Playlists":      func() error { _, err := store.Playlists(t.Context(), "edu"); return err },
		"PlaylistByID":   func() error { _, err := store.PlaylistByID(t.Context(), "edu", 1); return err },
		"LockPlaylist":   func() error { return store.LockPlaylist(t.Context(), "edu", 1) },
		"PlaylistTracks": func() error { _, err := store.PlaylistTracks(t.Context(), "edu", 1); return err },
		"UpdatePlaylist": func() error {
			return store.UpdatePlaylist(t.Context(), db.Playlist{ID: 1, OwnerID: "edu", Created: now, Changed: now})
		},
		"SetPlaylistTracks": func() error { return store.SetPlaylistTracks(t.Context(), "edu", 1, []int64{1}, now) },
		"DeletePlaylist":    func() error { return store.DeletePlaylist(t.Context(), "edu", 1) },
	}
	for name, call := range calls {
		if err := call(); err == nil {
			t.Errorf("%s against a closed store reported no error", name)
		}
	}
}

// TestCalendarsOnAClosedStore covers the error paths of the calendar
// repository the way TestPlaylistsOnAClosedStore covers the playlists'.
func TestCalendarsOnAClosedStore(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	object := db.CalendarObject{CalendarID: 1, Name: "a.ics", UID: "a", Component: "VEVENT", Data: "x", Changed: now}
	calls := map[string]func() error{
		"CreateCalendar": func() error {
			_, err := store.CreateCalendar(t.Context(), db.Calendar{OwnerID: "edu", Name: "x", Created: now})
			return err
		},
		"Calendars":       func() error { _, err := store.Calendars(t.Context(), "edu"); return err },
		"CalendarByName":  func() error { _, err := store.CalendarByName(t.Context(), "edu", "x"); return err },
		"UpdateCalendar":  func() error { return store.UpdateCalendar(t.Context(), db.Calendar{OwnerID: "edu", Name: "x"}) },
		"DeleteCalendar":  func() error { return store.DeleteCalendar(t.Context(), "edu", "x") },
		"CalendarObjects": func() error { _, err := store.CalendarObjects(t.Context(), 1); return err },
		"CalendarObject":  func() error { _, err := store.CalendarObject(t.Context(), 1, "a.ics"); return err },
		"PutCalendarObject": func() error {
			_, err := store.PutCalendarObject(t.Context(), object)
			return err
		},
		"DeleteCalendarObject": func() error { return store.DeleteCalendarObject(t.Context(), 1, "a.ics", now) },
		"CalendarChanges":      func() error { _, err := store.CalendarChanges(t.Context(), 1, 0); return err },
		"PruneCalendarTombstones": func() error {
			_, err := store.PruneCalendarTombstones(t.Context(), now)
			return err
		},
		"Settings":      func() error { _, err := store.Settings(t.Context()); return err },
		"PutSetting":    func() error { return store.PutSetting(t.Context(), "A", "x", now) },
		"DeleteSetting": func() error { return store.DeleteSetting(t.Context(), "A") },
	}
	for name, call := range calls {
		if err := call(); err == nil {
			t.Errorf("%s against a closed store reported no error", name)
		}
	}
}

// TestContactsOnAClosedStore covers the error paths of the address book
// repository the way TestCalendarsOnAClosedStore covers the calendars'.
func TestContactsOnAClosedStore(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	calls := map[string]func() error{
		"CreateAddressBook": func() error {
			_, err := store.CreateAddressBook(t.Context(), db.AddressBook{OwnerID: "edu", Name: "x", Created: now})
			return err
		},
		"AddressBooks":      func() error { _, err := store.AddressBooks(t.Context(), "edu"); return err },
		"AddressBookByName": func() error { _, err := store.AddressBookByName(t.Context(), "edu", "x"); return err },
		"UpdateAddressBook": func() error {
			return store.UpdateAddressBook(t.Context(), db.AddressBook{OwnerID: "edu", Name: "x"})
		},
		"DeleteAddressBook": func() error { return store.DeleteAddressBook(t.Context(), "edu", "x") },
		"Contacts":          func() error { _, err := store.Contacts(t.Context(), 1); return err },
		"Contact":           func() error { _, err := store.Contact(t.Context(), 1, "a.vcf"); return err },
		"PutContact": func() error {
			_, err := store.PutContact(t.Context(), db.Contact{AddressBookID: 1, Name: "a.vcf", UID: "a", Data: "x", Changed: now})
			return err
		},
		"DeleteContact":  func() error { return store.DeleteContact(t.Context(), 1, "a.vcf", now) },
		"ContactChanges": func() error { _, err := store.ContactChanges(t.Context(), 1, 0); return err },
		"PruneContactTombstones": func() error {
			_, err := store.PruneContactTombstones(t.Context(), now)
			return err
		},
	}
	for name, call := range calls {
		if err := call(); err == nil {
			t.Errorf("%s against a closed store reported no error", name)
		}
	}
}
