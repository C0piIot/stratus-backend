package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/config"
	"github.com/C0piIot/stratus-backend/internal/db/dbtest"
	"github.com/C0piIot/stratus-backend/internal/db/sqlite"
	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/incoming"
	"github.com/C0piIot/stratus-backend/internal/storage/disk"
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
	if err := meta.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}

	a := New(config.Config{GCInterval: time.Millisecond, GCGrace: time.Hour}, "test", "2026-01-01T09:30:00Z")
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

// TestImporterKeepsGoingAndStopsWithItsContext: the same property as the
// collector above, for the loop that moves what lands on disk. A folder it
// cannot read is a log line and another tick, not an outage -- and the file
// dropped into one it can read is in the library a pass later.
func TestImporterKeepsGoingAndStopsWithItsContext(t *testing.T) {
	t.Parallel()
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
	if err := meta.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}

	from := filepath.Join(dir, "incoming")
	if err := os.MkdirAll(from, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(from, "notes.txt"), []byte("dropped in"), 0o600); err != nil {
		t.Fatal(err)
	}

	service := files.New(blobs, meta)
	a := New(config.Config{IncomingInterval: time.Millisecond}, "test", "2026-01-01T09:30:00Z")
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
