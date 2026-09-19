package web_test

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/db/dbtest"
	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/media"
	"github.com/C0piIot/stratus-backend/internal/storage"
	"github.com/C0piIot/stratus-backend/internal/web"
)

// TestStatusCountsTheLibrary: a library is indexed in the background and a
// first pass over an adopted bucket takes hours, so there has to be somewhere
// to see how far it has got.
func TestStatusCountsTheLibrary(t *testing.T) {
	t.Parallel()
	h, s, meta := browserOver(t)
	write(t, s, "one.txt", "x")
	write(t, s, "two.txt", "y")
	indexOne(t, s, meta, "one.txt")

	body := get(t, h, "/status", signIn(t, h)).Body.String()

	for _, want := range []string{">2<", ">1<", "50%", "version " + strconv.Itoa(media.Version)} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not say %q", want)
		}
	}
	// And it is reachable from where somebody is: the listing.
	listing := get(t, h, "/files/", signIn(t, h)).Body.String()
	if !strings.Contains(listing, `href="/status"`) {
		t.Error("nothing links to the status page")
	}
}

// TestStatusRefreshesItself is the same page answering htmx with the numbers
// alone, which is what lets it be watched without reloading.
func TestStatusRefreshesItself(t *testing.T) {
	t.Parallel()
	h, s, _ := browserOver(t)
	write(t, s, "one.txt", "x")

	rec := getHTMX(t, h, "/status", signIn(t, h))
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("the fragment = %d, want 200", rec.Code)
	}
	if strings.Contains(body, "<html") || strings.Contains(body, "navbar") {
		t.Error("htmx was sent the whole page")
	}
	if !strings.Contains(body, "<progress") {
		t.Error("the fragment does not carry the numbers it was asked for")
	}
}

// TestStatusWithIndexingOff: zero is a decision an operator made, not a library
// that never finishes, and the page has to tell them apart.
func TestStatusWithIndexingOff(t *testing.T) {
	t.Parallel()
	blobs, meta := backends(t)
	s := files.New(blobs, meta)
	h := handlerIndexing(t, s, blobs, web.Indexing{Index: meta})

	body := get(t, h, "/status", signIn(t, h)).Body.String()
	if !strings.Contains(body, "Indexing is off") {
		t.Error("the page does not say that indexing is turned off")
	}
	if !strings.Contains(body, "STRATUS_INDEX_INTERVAL") {
		t.Error("the page does not say which setting turned it off")
	}
}

func TestStatusOverABackendThatWillNotAnswer(t *testing.T) {
	t.Parallel()
	blobs, meta := backends(t)
	broken := dbtest.FailOn(t, meta, "MediaCounts")
	h := handlerIndexing(t, files.New(blobs, broken), blobs, web.Indexing{Index: broken, Interval: time.Minute})

	rec := get(t, h, "/status", signIn(t, h))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("GET /status over a database that refuses = %d, want 500", rec.Code)
	}
}

// TestTheListingMarksWhatIsNotIndexedYet is the same three conditions the queue
// asks about, read from the other end: a file nobody has looked at yet, and one
// that has been read.
func TestTheListingMarksWhatIsNotIndexedYet(t *testing.T) {
	t.Parallel()
	h, s, meta := browserOver(t)
	write(t, s, "waiting.txt", "x")
	write(t, s, "done.txt", "y")
	indexOne(t, s, meta, "done.txt")

	body := get(t, h, "/files/", signIn(t, h)).Body.String()
	waiting := strings.Index(body, ">waiting.txt<")
	done := strings.Index(body, ">done.txt<")
	if waiting < 0 || done < 0 {
		t.Fatal("the listing is missing a file")
	}
	// The badge follows the name in the same cell, so the row it belongs to is
	// the one whose name comes before it.
	badge := strings.Index(body, ">waiting<")
	if badge < waiting {
		t.Error("the file nobody has read is not marked as waiting")
	}
	if strings.Count(body, ">waiting<") != 1 {
		t.Error("a file that has been indexed is marked too, which would mark every row of every listing")
	}
}

// TestTheListingSurvivesAFailedMark: the mark is decoration. A folder that will
// not open because the decoration could not be fetched is a worse page than one
// without it.
func TestTheListingSurvivesAFailedMark(t *testing.T) {
	t.Parallel()
	blobs, meta := backends(t)
	broken := dbtest.FailOn(t, meta, "MediaStates")
	s := files.New(blobs, meta)
	h := handlerIndexing(t, s, blobs, web.Indexing{Index: broken, Interval: time.Minute})
	write(t, s, "photo.jpg", "x")

	rec := get(t, h, "/files/", signIn(t, h))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /files/ = %d, want the listing anyway", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, ">photo.jpg<") {
		t.Error("the listing lost its file")
	}
	if strings.Contains(body, ">waiting<") {
		t.Error("a mark was invented out of a failed lookup")
	}
}

// TestStatusOfAnEmptyLibrary: nothing to do is finished, not nought percent of
// the way there -- which is what a fresh install sees.
func TestStatusOfAnEmptyLibrary(t *testing.T) {
	t.Parallel()
	h, _, _ := browserOver(t)

	body := get(t, h, "/status", signIn(t, h)).Body.String()
	if !strings.Contains(body, "100%") {
		t.Error("an empty library is not reported as done")
	}
}

// TestTheListingMarksWhatCouldNotBeRead: a file the extractor gave up on is
// neither waiting nor fine, and the row says which.
func TestTheListingMarksWhatCouldNotBeRead(t *testing.T) {
	t.Parallel()
	h, s, meta := browserOver(t)
	write(t, s, "broken.jpg", "not really a jpeg")

	f, err := s.Stat(t.Context(), username, "broken.jpg")
	if err != nil {
		t.Fatal(err)
	}
	err = meta.PutMedia(t.Context(), db.Media{
		FileID: f.ID, Kind: db.KindImage, IndexedAt: time.Now(),
		Version: media.Version, ETag: f.ETag, Error: "no exif segment",
	})
	if err != nil {
		t.Fatal(err)
	}

	body := get(t, h, "/files/", signIn(t, h)).Body.String()
	if !strings.Contains(body, ">unreadable<") {
		t.Error("the file the extractor gave up on is not marked")
	}
	if strings.Contains(body, ">waiting<") {
		t.Error("a file that was read and failed is reported as still waiting")
	}
}

// indexOne writes the media row the indexer would have written, which is what
// makes a file count as done without running an extractor.
func indexOne(t *testing.T, s *files.Service, meta db.Store, path string) {
	t.Helper()
	f, err := s.Stat(t.Context(), username, path)
	if err != nil {
		t.Fatal(err)
	}
	err = meta.PutMedia(t.Context(), db.Media{
		FileID:    f.ID,
		Kind:      db.KindOther,
		IndexedAt: time.Now(),
		Version:   media.Version,
		ETag:      f.ETag,
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestStatusSaysHowMuchRoomIsLeft is the half of #154 that a person sees. A
// self-hosted server on a home machine fills up, and the first symptom used to
// be an upload failing with whatever the filesystem said.
func TestStatusSaysHowMuchRoomIsLeft(t *testing.T) {
	t.Parallel()
	h, _, _ := browserOver(t)

	body := get(t, h, "/status", signIn(t, h)).Body.String()
	if !strings.Contains(body, "data-free-space") {
		t.Fatal("the status page says nothing about how much room is left")
	}
	// A real disk under a temporary directory, so what it says is a size and
	// not a count: the assertion is the shape, since the number is the
	// machine's and moves.
	if !strings.Contains(body, "iB</dd>") {
		t.Errorf("the room left is not rendered as a size: %s", between(body, "data-free-space"))
	}
}

// TestStatusOnAStoreWithNoSize is the other answer, and the reason the port has
// a word for it: an object store has no size to report, and a page that
// rendered Unlimited as nine exabytes would be lying in the one place somebody
// looks to decide whether their upload fits.
func TestStatusOnAStoreWithNoSize(t *testing.T) {
	t.Parallel()
	_, _, meta := pieces(t)
	h := handlerIndexing(t, files.New(unmeasured{}, meta), unmeasured{}, indexing(meta))

	body := get(t, h, "/status", signIn(t, h)).Body.String()
	if !strings.Contains(body, "unlimited") {
		t.Errorf("a store with no size to report rendered as %q", between(body, "data-free-space"))
	}
}

// TestStatusWhenTheStoreWillNotSay: one line of a page about something else,
// so it goes quiet rather than taking the page down with it.
func TestStatusWhenTheStoreWillNotSay(t *testing.T) {
	t.Parallel()
	_, _, meta := pieces(t)
	h := handlerIndexing(t, files.New(silent{}, meta), silent{}, indexing(meta))

	rec := get(t, h, "/status", signIn(t, h))
	if rec.Code != http.StatusOK {
		t.Fatalf("the status page = %d, want it to render anyway", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "data-free-space") {
		t.Error("a store that would not answer still produced a number")
	}
}

// between is the fragment around a marker, for an error message that says what
// was rendered instead.
func between(body, marker string) string {
	i := strings.Index(body, marker)
	if i < 0 {
		return "nothing"
	}
	return body[i:min(i+80, len(body))]
}

type unmeasured struct{ storage.Storage }

func (unmeasured) FreeSpace(context.Context) (int64, error) { return storage.Unlimited, nil }

type silent struct{ storage.Storage }

func (silent) FreeSpace(context.Context) (int64, error) {
	return 0, errors.New("this store will not say")
}
