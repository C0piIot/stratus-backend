package media

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/db/sqlite"
	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/storage/disk"
)

const owner = "edu"

// indexer wires the real backends. The extractors are what have unit tests;
// this is about the loop that feeds them and the rows it writes.
//
// ffprobe is deliberately absent: the toolchain container has no media tools,
// and a file that needs one has to fail in a way that does not block the queue.
func indexer(t *testing.T) (*Indexer, *files.Service, db.Store) {
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
	if merr := meta.Migrate(t.Context()); merr != nil {
		t.Fatal(merr)
	}

	tmp, err := TempDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	service := files.New(blobs, meta)
	return NewIndexer(service, meta, tmp, "ffprobe-does-not-exist"), service, meta
}

func write(t *testing.T, s *files.Service, path string, body []byte, mimeType string) db.File {
	t.Helper()
	// The tree invariant applies here too: a file needs its directory.
	if dir := db.ParentOf(path); dir != "" {
		if _, err := s.Mkdir(t.Context(), owner, dir); err != nil {
			t.Fatalf("Mkdir(%q): %v", dir, err)
		}
	}
	f, err := s.Write(t.Context(), owner, path, bytes.NewReader(body), int64(len(body)), mimeType)
	if err != nil {
		t.Fatalf("Write(%q): %v", path, err)
	}
	return f
}

func TestIndexAPhoto(t *testing.T) {
	t.Parallel()
	indexer, service, meta := indexer(t)
	f := write(t, service, "photos/IMG_0001.jpg", exifJPEG(t), "image/jpeg")

	n, err := indexer.IndexBatch(t.Context())
	if err != nil {
		t.Fatalf("IndexBatch: %v", err)
	}
	if n != 1 {
		t.Fatalf("IndexBatch indexed %d files, want 1", n)
	}

	got, err := meta.MediaByFile(t.Context(), f.ID)
	if err != nil {
		t.Fatalf("MediaByFile: %v", err)
	}
	if !got.Indexed() {
		t.Fatalf("extraction failed: %s", got.Error)
	}
	if got.Kind != db.KindImage || got.Width != 4032 || got.Camera != "Apple iPhone 15 Pro" {
		t.Errorf("got %+v", got)
	}
	if got.Version != Version {
		t.Errorf("Version = %d, want %d", got.Version, Version)
	}

	// And it does not come back.
	if n, err := indexer.IndexBatch(t.Context()); err != nil || n != 0 {
		t.Errorf("a second pass indexed %d files, %v", n, err)
	}
}

// TestIndexSkipsWhatHasNoMetadata still writes a row. Without one, every text
// file in the library would be offered again on every pass forever.
func TestIndexSkipsWhatHasNoMetadata(t *testing.T) {
	t.Parallel()
	indexer, service, meta := indexer(t)
	f := write(t, service, "notes.txt", []byte("nothing to extract"), "text/plain")

	if _, err := indexer.IndexBatch(t.Context()); err != nil {
		t.Fatal(err)
	}

	got, err := meta.MediaByFile(t.Context(), f.ID)
	if err != nil {
		t.Fatalf("MediaByFile: %v", err)
	}
	if got.Kind != db.KindOther || !got.Indexed() {
		t.Errorf("got %+v, want a plain row with no error", got)
	}
	if n, _ := indexer.IndexBatch(t.Context()); n != 0 {
		t.Error("the text file came back")
	}
}

// TestIndexRecordsAFailure is the rule that keeps one bad file from being read
// on every pass for the rest of time.
func TestIndexRecordsAFailure(t *testing.T) {
	t.Parallel()
	indexer, service, meta := indexer(t)
	// Named like a photo and full of something else, which is what a truncated
	// upload or a renamed file looks like.
	f := write(t, service, "broken.jpg", []byte("this is not a jpeg"), "image/jpeg")

	if _, err := indexer.IndexBatch(t.Context()); err != nil {
		t.Fatalf("IndexBatch must not fail because one file did: %v", err)
	}

	got, err := meta.MediaByFile(t.Context(), f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Indexed() {
		t.Error("a file that is not an image was indexed without complaint")
	}
	if got.Kind != db.KindImage {
		t.Errorf("Kind = %q, want what it claimed to be", got.Kind)
	}
	if n, _ := indexer.IndexBatch(t.Context()); n != 0 {
		t.Error("the broken file came back")
	}
}

// TestIndexWithoutFFprobe covers the media the toolchain cannot read here: it
// has to fail into a row like anything else rather than stop the pass.
func TestIndexWithoutFFprobe(t *testing.T) {
	t.Parallel()
	indexer, service, meta := indexer(t)
	track := write(t, service, "music/song.mp3", []byte("ID3 and then some"), "audio/mpeg")
	photo := write(t, service, "photo.jpg", exifJPEG(t), "image/jpeg")

	if _, err := indexer.IndexBatch(t.Context()); err != nil {
		t.Fatalf("IndexBatch: %v", err)
	}

	got, err := meta.MediaByFile(t.Context(), track.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Indexed() {
		t.Error("an mp3 was indexed with no ffprobe available")
	}
	if got.Kind != db.KindAudio {
		t.Errorf("Kind = %q", got.Kind)
	}
	// And the photo beside it went through, because one file failing is not the
	// batch failing.
	if photoMedia, err := meta.MediaByFile(t.Context(), photo.ID); err != nil || !photoMedia.Indexed() {
		t.Errorf("the photo was not indexed: %+v %v", photoMedia, err)
	}
}

// TestTempDirSweepsWhatWasLeftBehind: a spooled video from a process that died
// is the same kind of garbage as an interrupted upload, and is dealt with the
// same way.
func TestTempDirSweepsWhatWasLeftBehind(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	first, err := TempDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	leftover := filepath.Join(first, "probe-interrupted")
	if err := os.WriteFile(leftover, []byte("half a video"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := TempDir(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(leftover); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a spooled file survived a restart: %v", err)
	}
}
