package app_test

// The two goroutines that outlive the start.
//
// What is under test here is the scheduling and not the work: that a pass
// happens on the configured interval, that turning the interval off stops it,
// and that a failure in one does not take the process down. What a pass decides
// belongs to internal/files and internal/media and is covered there.

import (
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/db/sqlite"
)

// TestCollectorRuns is the wiring, not the collecting: the goroutine starts on
// the configured interval and does a pass. What a pass decides belongs to
// internal/files and is covered there by five cases that call Collect directly.
//
// There is exactly one upload, and the orphan is put in place by hand rather
// than made by a second write. That is what makes this deterministic instead of
// a coin flip (#60): Collect reads the database and then lists the store, and a
// write goes blob first and row second, so a pass landing between a second
// upload's blob and its row finds a live blob unreferenced. The grace period is
// what covers that window, and this test used to set it to zero -- which is how
// it came to delete the live blob rather than the orphan.
//
// The one upload has a window of its own, and something else closes it: with no
// rows at all, Collect refuses rather than treating the whole store as garbage.
func TestCollectorRuns(t *testing.T) {
	t.Parallel()
	const password = "an example password"
	dataDir := filepath.Join(t.TempDir(), "data")
	base, stop := liveServer(t, map[string]string{
		"STRATUS_DATA_DIR":    dataDir,
		"STRATUS_USERNAME":    "edu",
		"STRATUS_PASSWORD":    password,
		"STRATUS_GC_INTERVAL": "50ms",
		"STRATUS_GC_GRACE":    "1h",
	})
	defer stop()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, base+"/dav/notes.txt", strings.NewReader("one"))
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth("edu", password)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	blobDir := filepath.Join(dataDir, "blobs")
	live := blobPaths(t, blobDir)
	if len(live) != 1 {
		t.Fatalf("the store holds %d blobs after one upload, want 1", len(live))
	}

	// What a failed overwrite leaves behind, without racing a live write to get
	// it. Backdated past the grace, or the sweep would rightly leave it alone.
	orphan := filepath.Join(blobDir, "ZZ", "ZZ", "ORPHANOFAFAILEDOVERWRITE")
	if err := os.MkdirAll(filepath.Dir(orphan), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(orphan, []byte("leftover"), 0o600); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(orphan, past, past); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(orphan); errors.Is(err, os.ErrNotExist) {
			// The pass that took the orphan has to have left the live blob,
			// which is the half a blob count cannot tell apart.
			if _, err := os.Stat(live[0]); err != nil {
				t.Errorf("the sweep took the live blob: %v", err)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("the orphan is still there after 10s: the collector never ran")
}

// blobPaths is every object in the store, by path. The reserved directory is
// skipped: an interrupted upload lives there and is nobody's orphan.
func blobPaths(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// Neither reserved directory is an orphan: one holds writes in flight
		// and the other uploads waiting to be resumed.
		if d.IsDir() && (d.Name() == ".tmp" || d.Name() == ".uploads") {
			return filepath.SkipDir
		}
		if !d.IsDir() {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return out
}

func countBlobs(t *testing.T, dir string) int {
	t.Helper()
	return len(blobPaths(t, dir))
}

// TestCollectorTakesAbandonedUploads is the other half of the same sweep, and
// it is the only thing that ever collects one: an upload in flight is invisible
// to a listing, which is what keeps the blob sweep off it and means the blob
// sweep will never tidy it away either.
//
// The expired upload is written by hand for the same reason the orphan above
// is: there is no protocol to create one over yet, and a row put in place
// directly is deterministic where a race is not.
func TestCollectorTakesAbandonedUploads(t *testing.T) {
	t.Parallel()
	dataDir := filepath.Join(t.TempDir(), "data")
	_, stop := liveServer(t, map[string]string{
		"STRATUS_DATA_DIR":    dataDir,
		"STRATUS_USERNAME":    "edu",
		"STRATUS_PASSWORD":    "an example password",
		"STRATUS_GC_INTERVAL": "50ms",
		"STRATUS_GC_GRACE":    "1h",
	})
	defer stop()

	meta, err := sqlite.New(t.Context(), filepath.Join(dataDir, "stratus.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = meta.Close() }()

	abandoned := db.Upload{
		ID:        "an-upload-nobody-came-back-to",
		OwnerID:   "edu",
		Path:      "holiday/clip.mp4",
		Size:      -1,
		BlobKey:   "video/2026/09/16/ABANDONED",
		StoreID:   "whatever-the-store-called-it",
		Digest:    []byte{},
		MIMEType:  "video/mp4",
		ExpiresAt: time.Now().Add(-time.Hour),
	}
	if err := meta.PutUpload(t.Context(), abandoned); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := meta.UploadByID(t.Context(), "edu", abandoned.ID); errors.Is(err, db.ErrNotFound) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("the abandoned upload is still there after 10s: nothing collected it")
}

// TestCollectorDisabled covers the other half of the switch, because a sweep
// that runs when it was turned off is worse than one that never runs.
func TestCollectorDisabled(t *testing.T) {
	t.Parallel()
	const password = "an example password"
	dataDir := filepath.Join(t.TempDir(), "data")
	base, stop := liveServer(t, map[string]string{
		"STRATUS_DATA_DIR":    dataDir,
		"STRATUS_USERNAME":    "edu",
		"STRATUS_PASSWORD":    password,
		"STRATUS_GC_INTERVAL": "0",
	})

	for _, body := range []string{"one", "two"} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, base+"/dav/notes.txt", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.SetBasicAuth("edu", password)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	time.Sleep(200 * time.Millisecond)
	stop()

	if got := countBlobs(t, filepath.Join(dataDir, "blobs")); got != 2 {
		t.Errorf("the store holds %d blobs, want both: collection was disabled", got)
	}
}

// TestRunRefusesWithoutFFprobe is the requirement made visible. ffprobe is not
// an optional extra: without it a track has no duration and a video no
// dimensions, and half a media library is worse than an honest refusal.
func TestRunRefusesWithoutFFprobe(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("ffprobe"); err == nil {
		t.Skip("ffprobe is on the PATH here, so its absence cannot be tested")
	}

	err := runToShutdown(t, runConfig(t, map[string]string{"STRATUS_INDEX_INTERVAL": "1m"}))
	if err == nil {
		t.Fatal("Run = nil, want a refusal to start without ffprobe")
	}
	if !strings.Contains(err.Error(), "ffprobe") {
		t.Errorf("the error should name what is missing, got %v", err)
	}
}

// TestIndexerRuns covers the goroutine and its wiring, with a stub on the PATH
// standing in for ffprobe: the toolchain container has no media tools, and what
// is under test here is the loop rather than the extractors.
//
// The idle interval is ten minutes on purpose. Nothing here waits for it, so
// what this asserts is the other half of the wiring: the write tells the
// indexer and the indexer stops waiting. A file indexed seconds after a PUT,
// with the timer that far away, cannot have got there any other way.
//
// Not parallel, because it changes the process environment.
func TestIndexerRuns(t *testing.T) {
	const password = "an example password"
	stubFFprobe(t)

	dataDir := filepath.Join(t.TempDir(), "data")
	base, stop := liveServer(t, map[string]string{
		"STRATUS_DATA_DIR":       dataDir,
		"STRATUS_USERNAME":       "edu",
		"STRATUS_PASSWORD":       password,
		"STRATUS_INDEX_INTERVAL": "10m",
	})
	defer stop()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, base+"/dav/notes.txt", strings.NewReader("indexed"))
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth("edu", password)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	store, err := sqlite.New(t.Context(), filepath.Join(dataDir, "stratus.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		f, ferr := store.FileByPath(t.Context(), "edu", "notes.txt")
		if ferr == nil {
			if m, merr := store.MediaByFile(t.Context(), f.ID); merr == nil {
				if !m.Indexed() {
					t.Fatalf("the file was indexed with an error: %s", m.Error)
				}
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("the uploaded file was never indexed")
}

// stubFFprobe puts something called ffprobe on the PATH. The indexer refuses to
// start without one, which is the point of the requirement.
func stubFFprobe(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	stub := filepath.Join(dir, "ffprobe")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\necho '{\"streams\":[],\"format\":{}}'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}
