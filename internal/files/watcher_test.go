package files_test

import (
	"strings"
	"sync"
	"testing"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/db/dbtest"
	"github.com/C0piIot/stratus-backend/internal/files"
)

// heard collects what the watcher was told. Guarded because the point of the
// watcher is that somebody else acts on it, and a test that raced here would be
// testing its own bookkeeping.
type heard struct {
	mu    sync.Mutex
	files []db.File
}

func (h *heard) watch(f db.File) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.files = append(h.files, f)
}

func (h *heard) seen() []db.File {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]db.File(nil), h.files...)
}

// TestAWriteIsAnnounced is what turns "indexed within a minute" into "indexed
// now": the row that has just been committed, handed to whoever is listening.
//
// The overwrite is the half that matters most. A replaced file keeps its row
// and its id, so the second announcement carries the same id and a different
// validator -- which is exactly what the media queue compares.
func TestAWriteIsAnnounced(t *testing.T) {
	t.Parallel()
	var listener heard
	s, _ := service(t, files.WithWatcher(listener.watch))

	first := write(t, s, "holiday.mp4", "some bytes")
	second := write(t, s, "holiday.mp4", "different bytes")

	seen := listener.seen()
	if len(seen) != 2 {
		t.Fatalf("the watcher heard %d writes, want both", len(seen))
	}
	if seen[0].ID != first.ID || seen[1].ID != second.ID {
		t.Errorf("heard %d and %d, want the rows that were stored", seen[0].ID, seen[1].ID)
	}
	if seen[0].ETag == "" || seen[0].ETag == seen[1].ETag {
		t.Errorf("validators %q and %q: the overwrite has to be distinguishable from what it replaced",
			seen[0].ETag, seen[1].ETag)
	}
	if seen[1].Path != "holiday.mp4" || seen[1].BlobKey == "" {
		t.Errorf("heard %+v, want the row as committed", seen[1])
	}
}

// TestAFailedWriteIsNotAnnounced: the watcher is told about what happened, not
// about what was attempted. A blob with no row is garbage the sweep collects,
// and telling the indexer to go and look at it would be telling it a lie.
func TestAFailedWriteIsNotAnnounced(t *testing.T) {
	t.Parallel()
	_, blobs, meta := serviceOver(t)
	var listener heard
	broken := files.New(blobs, dbtest.FailOn(t, meta, "PutFile"), files.WithWatcher(listener.watch))

	_, err := broken.Write(t.Context(), owner, "doomed.txt", strings.NewReader("bytes"), 5, "text/plain")
	if err == nil {
		t.Fatal("the write was supposed to fail")
	}
	if seen := listener.seen(); len(seen) != 0 {
		t.Errorf("the watcher heard %+v about a write whose row never landed", seen)
	}
}

// TestAResumedUploadIsAnnounced: the other door into the tree. A phone that
// backs up over tus writes rows exactly like a PUT does, and an indexer that
// only heard about one of the two would leave half a library alone.
func TestAResumedUploadIsAnnounced(t *testing.T) {
	t.Parallel()
	var listener heard
	s, _ := service(t, files.WithWatcher(listener.watch))

	const body = "a resumable film"
	u, err := s.BeginUpload(t.Context(), owner, "clip.mp4", int64(len(body)), "video/mp4")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AppendUpload(t.Context(), owner, u.ID, 0, strings.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	stored, err := s.CompleteUpload(t.Context(), owner, u.ID)
	if err != nil {
		t.Fatal(err)
	}

	seen := listener.seen()
	if len(seen) != 1 || seen[0].ID != stored.ID {
		t.Errorf("the watcher heard %+v, want the completed upload", seen)
	}
}

// TestNobodyListening is the ordinary case: every caller that does not care
// passes no option, and nothing here checks for a watcher twice.
func TestNobodyListening(t *testing.T) {
	t.Parallel()
	s, _ := service(t)
	write(t, s, "quiet.txt", "bytes")
}
