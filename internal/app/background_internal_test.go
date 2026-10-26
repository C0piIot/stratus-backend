package app

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stratus-private-cloud/stratus-backend/internal/calendar"
	"github.com/stratus-private-cloud/stratus-backend/internal/config"
	"github.com/stratus-private-cloud/stratus-backend/internal/contacts"
	"github.com/stratus-private-cloud/stratus-backend/internal/db"
	"github.com/stratus-private-cloud/stratus-backend/internal/db/dbtest"
	"github.com/stratus-private-cloud/stratus-backend/internal/files"
	"github.com/stratus-private-cloud/stratus-backend/internal/files/filestest"
	"github.com/stratus-private-cloud/stratus-backend/internal/incoming"
	"github.com/stratus-private-cloud/stratus-backend/internal/media"
	"github.com/stratus-private-cloud/stratus-backend/internal/media/thumb"
	"github.com/stratus-private-cloud/stratus-backend/internal/storage"
	"github.com/stratus-private-cloud/stratus-backend/internal/web"
)

// TestCollectorSurvivesAFailedPass is the property the whole loop exists to
// have: a sweep that cannot do its work logs and waits for the next tick. A
// background goroutine that took the process down with it would turn a full
// disk into an outage.
//
// Internal because it drives the loop directly: what a pass decides belongs to
// internal/files, and what is under test here is only that a refusal is
// survivable.
func TestCollectorSurvivesAFailedPass(t *testing.T) {
	t.Parallel()
	blobs, meta := filestest.Backends(t)

	a := New(config.Config{Values: config.Values{GCInterval: time.Millisecond, GCGrace: time.Hour}}, "test", "2026-01-01T09:30:00Z")
	broken := dbtest.FailOn(t, meta, "ExpiredUploads")
	deps := Deps{Storage: blobs, Database: broken, Files: files.New(blobs, broken)}

	// Long enough for several passes to fail, short enough that the test is not
	// waiting on anything: the loop ends when the context does.
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		a.collectPeriodically(ctx, deps)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("the collector did not stop with its context")
	}
}

// TestTheTrashCanBeHeld is #296 from the collector's side: the same
// deletion that a retention empties survives the pass with the retention at
// never. The sweep behind it is not this function's, and runs either way.
func TestTheTrashCanBeHeld(t *testing.T) {
	t.Parallel()
	blobs, meta := filestest.Backends(t)
	service := files.New(blobs, meta)
	if _, err := service.Write(t.Context(), "u", "old.txt", strings.NewReader("x"), 1, "text/plain"); err != nil {
		t.Fatal(err)
	}
	if err := service.Remove(t.Context(), "u", "old.txt"); err != nil {
		t.Fatal(err)
	}
	held := func() int64 {
		totals, err := service.TrashTotals(t.Context(), "u")
		if err != nil {
			t.Fatal(err)
		}
		return totals.Files
	}

	never := New(config.Config{}, "test", "2026-01-01T09:30:00Z")
	if !never.emptyTrash(t.Context(), service, never.cfg.TrashRetention, &web.Sweep{}) || held() != 1 {
		t.Errorf("with the trash held the pass left %d files in it, want 1", held())
	}
	// A retention so short that the deletion is already past it, which only
	// a test can set: config refuses anything under a day.
	brief := New(config.Config{Values: config.Values{TrashRetention: time.Nanosecond}}, "test", "2026-01-01T09:30:00Z")
	if !brief.emptyTrash(t.Context(), service, brief.cfg.TrashRetention, &web.Sweep{}) || held() != 0 {
		t.Errorf("with a retention the pass left %d files in the trash, want 0", held())
	}

	gone, cancel := context.WithCancel(t.Context())
	cancel()
	if brief.emptyTrash(gone, service, brief.cfg.TrashRetention, &web.Sweep{}) {
		t.Error("a pass whose context had gone did not say so")
	}
	if retention(0) != "never" || retention(time.Hour) != time.Hour {
		t.Error("the log value of the retention is wrong")
	}
}

// TestCalendarDeletionsAreForgotten is the collector's half of #75's third
// step: a deletion older than calendar.TombstoneRetention goes, a failure is
// logged and survived, and a pass whose context has gone says so.
func TestCalendarDeletionsAreForgotten(t *testing.T) {
	t.Parallel()
	_, meta := filestest.Backends(t)
	ctx := t.Context()
	c, err := meta.CreateCalendar(ctx, db.Calendar{OwnerID: "u", Name: "default", Created: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = meta.PutCalendarObject(ctx, db.CalendarObject{
		CalendarID: c.ID, Name: "a.ics", UID: "a", Component: "VEVENT", Data: "x", Changed: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err = meta.DeleteCalendarObject(ctx, c.ID, "a.ics", time.Now().Add(-calendar.TombstoneRetention-time.Hour)); err != nil {
		t.Fatal(err)
	}

	if !pruneCalendars(ctx, meta) {
		t.Fatal("a pass with a live context said it had gone")
	}
	changes, err := meta.CalendarChanges(ctx, c.ID, 0)
	if err != nil || len(changes) != 0 {
		t.Errorf("CalendarChanges after the pass = %+v, %v; want the deletion forgotten", changes, err)
	}

	if !pruneCalendars(ctx, dbtest.FailOn(t, meta, "PruneCalendarTombstones")) {
		t.Error("a failed pass stopped the loop")
	}
	if !pruneCalendars(ctx, nil) {
		t.Error("no database stopped the loop")
	}
	gone, cancel := context.WithCancel(ctx)
	cancel()
	if pruneCalendars(gone, meta) {
		t.Error("a pass whose context had gone did not say so")
	}
}

// TestThePicturePassPacesItself is what thumbsAhead decides, which is not
// whether to make a picture -- that is internal/media's -- but when to come
// back: now while a batch keeps filling, and otherwise on the hour the loop
// ticks on anyway. A pass that came straight back for one file would hold a
// decoder for as long as there is work, which is the opposite of what a
// background pass is for.
//
// Internal because it drives that one decision directly.
func TestThePicturePassPacesItself(t *testing.T) {
	t.Parallel()
	blobs, meta := filestest.Backends(t)

	service := files.New(blobs, meta)
	a := New(config.Config{Values: config.Values{ThumbsAhead: true}}, "test", "2026-01-01T09:30:00Z")
	deps := Deps{
		Storage: blobs, Database: meta, Files: service,
		Thumbs: thumb.New(blobs, service, meta, "ffmpeg", t.TempDir()),
	}
	// One more than a batch, so the first pass fills and the second does not.
	for i := range thumb.Batch + 1 {
		writePhoto(t, service, meta, fmt.Sprintf("%d.jpg", i))
	}

	if wait := a.thumbsAhead(t.Context(), deps); wait != thumbPause {
		t.Errorf("after a full batch the pass waits %v, want %v", wait, thumbPause)
	}
	if wait := a.thumbsAhead(t.Context(), deps); wait != 0 {
		t.Errorf("after the last file the pass waits %v, want the loop's own tick", wait)
	}

	// A context that has gone says nothing about the library: the process is
	// stopping, and the log line every other failure gets would be noise at
	// every shutdown.
	gone, cancel := context.WithCancel(t.Context())
	cancel()
	if wait := a.thumbsAhead(gone, deps); wait != 0 {
		t.Errorf("a pass whose context had gone waits %v, want nothing", wait)
	}

	// And a pass that cannot ask the database waits for the tick like every
	// other loop here: a background goroutine is not where an outage starts.
	deps.Thumbs = thumb.New(blobs, service, dbtest.FailOn(t, meta, "PendingThumbs"), "ffmpeg", t.TempDir())
	if wait := a.thumbsAhead(t.Context(), deps); wait != 0 {
		t.Errorf("a pass that could not read the queue waits %v, want the tick", wait)
	}

	// The same of the half that takes the notice: a file whose bytes are not
	// in the store is a log line, because what this does is extra and the
	// write it followed has already succeeded.
	a.thumbFor(t.Context(), deps, db.File{ID: 1, Path: "gone.jpg", BlobKey: "blobs/gone"})

	// And nothing at all for a file a grid does not draw: a track's cover is
	// a different picture answering a different surface, and everything else
	// has none.
	a.thumbFor(t.Context(), deps, db.File{ID: 1, Path: "notes.txt", BlobKey: "blobs/gone"})

	// Zero is the loop's own tick and nothing else: a nil channel never fires.
	if again := after(0); again != nil {
		t.Error("a pass that asked for no wait armed a timer")
	}
	if again := after(thumbPause); again == nil {
		t.Error("a pass that asked to come back armed nothing")
	}

	// And with the pass turned off neither half does anything at all, which is
	// the whole of what the switch is.
	off := New(config.Config{}, "test", "2026-01-01T09:30:00Z")
	if wait := off.thumbsAhead(t.Context(), deps); wait != 0 {
		t.Errorf("the pass ran with the switch off and waits %v", wait)
	}
	off.thumbFor(t.Context(), deps, db.File{ID: 1, Path: "gone.jpg", BlobKey: "blobs/gone"})
}

// TestThePicturePassStandsAside is the rule the whole pass hangs on, read from
// this side: while a request is decoding, the pass makes nothing and comes
// back in a minute rather than at the hour -- short enough that a library is
// still filled in while somebody browses it, long enough to be out of the way.
func TestThePicturePassStandsAside(t *testing.T) {
	t.Parallel()
	blobs, meta := filestest.Backends(t)

	// A store that holds the read of one file open, which is what a decode
	// somebody is waiting for looks like from here.
	held := &holdingStore{Storage: blobs, reading: make(chan struct{}), release: make(chan struct{})}
	service := files.New(held, meta)
	writePhoto(t, files.New(blobs, meta), meta, "holiday.jpg")

	a := New(config.Config{Values: config.Values{ThumbsAhead: true}}, "test", "2026-01-01T09:30:00Z")
	thumbs := thumb.New(blobs, service, meta, "ffmpeg", t.TempDir())
	deps := Deps{Storage: blobs, Database: meta, Files: service, Thumbs: thumbs}

	go func() { _, _, _ = thumbs.File(t.Context(), "edu", "holiday.jpg", 96) }()
	<-held.reading
	defer close(held.release)

	if wait := a.thumbsAhead(t.Context(), deps); wait != thumbBusyPause {
		t.Errorf("with a request decoding the pass waits %v, want %v", wait, thumbBusyPause)
	}
}

// holdingStore keeps one read open until it is let go, so that a test can hold
// a decoder the way a slow file does.
type holdingStore struct {
	storage.Storage
	reading chan struct{}
	release chan struct{}
	once    sync.Once
}

func (h *holdingStore) Get(ctx context.Context, key string, r storage.Range) (io.ReadCloser, storage.ObjectInfo, error) {
	h.once.Do(func() {
		close(h.reading)
		<-h.release
	})
	return h.Storage.Get(ctx, key, r)
}

// writePhoto puts a real photograph in the library and gives it the media row
// a pass works from: the extractor's half comes first, always.
func writePhoto(t *testing.T, s *files.Service, meta db.MediaIndex, path string) {
	t.Helper()

	var body bytes.Buffer
	if err := jpeg.Encode(&body, image.NewRGBA(image.Rect(0, 0, 16, 16)), nil); err != nil {
		t.Fatal(err)
	}
	f, err := s.Write(t.Context(), "edu", path, bytes.NewReader(body.Bytes()), int64(body.Len()), "image/jpeg")
	if err != nil {
		t.Fatalf("writing %q: %v", path, err)
	}
	row := db.Media{FileID: f.ID, Kind: db.KindImage, IndexedAt: time.Now(), Version: media.Version, ETag: f.ETag}
	if err := meta.PutMedia(t.Context(), row); err != nil {
		t.Fatalf("indexing %q: %v", path, err)
	}
}

// TestImporterKeepsGoingAndStopsWithItsContext: the same property as the
// collector above, for the loop that moves what lands on disk. A folder it
// cannot read is a log line and another tick, not an outage -- and the file
// dropped into one it can read is in the library a pass later.
func TestImporterKeepsGoingAndStopsWithItsContext(t *testing.T) {
	t.Parallel()
	blobs, meta := filestest.Backends(t)

	from := filepath.Join(t.TempDir(), "incoming")
	if err := os.MkdirAll(from, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(from, "notes.txt"), []byte("dropped in"), 0o600); err != nil {
		t.Fatal(err)
	}

	service := files.New(blobs, meta)
	a := New(config.Config{Values: config.Values{IncomingInterval: time.Millisecond}}, "test", "2026-01-01T09:30:00Z")
	deps := Deps{Storage: blobs, Database: meta, Files: service,
		Incoming: incoming.New(from, "edu", service)}

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		a.importPeriodically(ctx, deps)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the importer did not stop with its context")
	}

	if _, err := service.Stat(context.Background(), "edu", "notes.txt"); err != nil {
		t.Errorf("the file dropped in was not imported: %v", err)
	}
	if imports(deps) == nil {
		t.Error("a configured folder is not reported to the status page")
	}
	if imports(Deps{}) != nil {
		t.Error("a server with no folder reports one")
	}
}

// A loop that panics is started again rather than taking the process down, and
// stops being started once the process is stopping.
func TestKeepRunningRestartsAfterAPanic(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	runs := 0
	keepRunning(ctx, "test", time.Millisecond, func() {
		runs++
		if runs < 3 {
			panic("boom")
		}
	})
	if runs != 3 {
		t.Errorf("ran %d times, want 3: twice panicking and once returning", runs)
	}

	cancel()
	runs = 0
	keepRunning(ctx, "test", time.Hour, func() { runs++; panic("boom") })
	if runs != 1 {
		t.Errorf("ran %d times after the context ended, want 1", runs)
	}
}

func TestOldContactDeletionsAreForgotten(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	_, meta := filestest.Backends(t)
	b, err := meta.CreateAddressBook(ctx, db.AddressBook{OwnerID: "edu", Name: "contacts", Created: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = meta.PutContact(ctx, db.Contact{AddressBookID: b.ID, Name: "a.vcf", UID: "a", Data: "x", Changed: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err = meta.DeleteContact(ctx, b.ID, "a.vcf", time.Now().Add(-contacts.TombstoneRetention-time.Hour)); err != nil {
		t.Fatal(err)
	}

	if !pruneContacts(ctx, meta) {
		t.Fatal("a pass with a live context said it had gone")
	}
	if changes, err := meta.ContactChanges(ctx, b.ID, 0); err != nil || len(changes) != 0 {
		t.Errorf("ContactChanges after the pass = %+v, %v; want the deletion forgotten", changes, err)
	}
	if !pruneContacts(ctx, dbtest.FailOn(t, meta, "PruneContactTombstones")) {
		t.Error("a failed pass stopped the loop")
	}
	if !pruneContacts(ctx, nil) {
		t.Error("no database stopped the loop")
	}
	gone, cancel := context.WithCancel(ctx)
	cancel()
	if pruneContacts(gone, meta) {
		t.Error("a pass whose context had gone did not say so")
	}
}
