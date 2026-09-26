// The goroutines that keep running after the server is up.
//
// They live in the composition root and not in the features they drive, because
// scheduling is lifecycle: when a pass happens, how often, and that it stops
// with the process are this package's business, while what a pass does belongs
// to internal/media and internal/files.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/media"
)

// indexPeriodically extracts metadata from files that have none.
//
// A write hands over the file it just wrote, so the ordinary case is one
// extraction and no query at all. The query is the safety net behind it, and it
// runs on the interval -- or sooner, when the indexer says it dropped a notice
// and therefore does not know what landed.
//
// One worker: ffprobe on four cores that are also serving requests does not
// want company.
func (a *App) indexPeriodically(ctx context.Context, deps Deps) {
	slog.Info("indexing media", "safety_net", a.cfg.IndexInterval, "version", media.Version)

	// A ticker and not a timer armed after each pass: with a steady trickle of
	// uploads an interval that restarted every time would never arrive, and
	// what waits behind it -- a version bump, rows an import inserted, a file
	// deferred until its retry time -- would never be looked at.
	ticker := time.NewTicker(a.cfg.IndexInterval)
	defer ticker.Stop()

	// Whatever landed while this process was not running is nobody's notice.
	a.indexUntilDry(ctx, deps)

	for {
		select {
		case <-ctx.Done():
			return

		case f := <-deps.Indexer.Noticed():
			switch err := deps.Indexer.IndexFile(ctx, time.Now(), f); {
			case errors.Is(err, context.Canceled):
				return
			case err != nil:
				slog.Error("indexing media", "path", f.Path, "err", err)
			}
			continue

		case <-deps.Indexer.LostTrack():
			// More arrived at once than could be held, so what was dropped is
			// only a row now -- and a row is what the query finds.
		case <-ticker.C:
		}

		if !a.indexUntilDry(ctx, deps) {
			return
		}
	}
}

// indexUntilDry runs the query until it stops filling a batch, and reports
// whether the process is still meant to be running.
func (a *App) indexUntilDry(ctx context.Context, deps Deps) bool {
	for {
		indexed, err := deps.Indexer.IndexBatch(ctx, time.Now())
		switch {
		case errors.Is(err, context.Canceled):
			return false
		case err != nil:
			slog.Error("indexing media", "err", err)
		case indexed > 0:
			slog.Info("indexed media", "files", indexed)
		}

		// A full batch means there is probably more, which is what makes a
		// first run over an existing library go as fast as the extractors do.
		if indexed != media.BatchSize || err != nil {
			return ctx.Err() == nil
		}
	}
}

// collectPeriodically sweeps blobs no row points at, for as long as ctx lives.
//
// Every write takes a fresh blob key so that a failed overwrite cannot destroy
// what it was replacing, which means every overwrite leaves one behind. This is
// what reclaims them.
func (a *App) collectPeriodically(ctx context.Context, deps Deps) {
	service := deps.Files
	ticker := time.NewTicker(a.cfg.GCInterval)
	defer ticker.Stop()

	slog.Info("collecting orphan blobs and abandoned uploads",
		"every", a.cfg.GCInterval, "grace", a.cfg.GCGrace, "upload_ttl", files.DefaultUploadTTL)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		// Uploads first: an abandoned one holds bytes that are invisible to a
		// listing, so the blob sweep below can neither see them nor free them.
		// Nothing else collects them at all.
		switch done, err := service.CollectUploads(ctx, time.Now()); {
		case errors.Is(err, context.Canceled):
			return
		case err != nil:
			slog.Error("collecting abandoned uploads", "err", err)
		case done > 0:
			slog.Info("collected abandoned uploads", "count", done)
		}

		switch done, err := service.Collect(ctx, a.cfg.GCGrace); {
		case errors.Is(err, context.Canceled):
			return
		case errors.Is(err, files.ErrEmptyIndex):
			// Almost certainly a database pointed somewhere new rather than a
			// library somebody emptied, so nothing is deleted and it says so.
			slog.Warn("skipped collecting orphan blobs",
				"reason", "the database references no blobs and the store is not empty")
		case err != nil:
			slog.Error("collecting orphan blobs", "err", err)
		case done.Deleted > 0:
			slog.Info("collected orphan blobs",
				"scanned", done.Scanned, "deleted", done.Deleted, "bytes", done.Bytes)
		}
	}
}

// restartPause is how long a background loop that panicked waits before it is
// started again, so that one which panics on every pass does not spin.
const restartPause = time.Minute

// keepRunning runs fn and starts it again when it panics, until ctx ends or fn
// returns.
//
// Nothing else recovers a panic in a goroutine: without this, one file that
// trips the indexer would take every surface down with it, and the restart
// would find the same file and do it again.
func keepRunning(ctx context.Context, task string, pause time.Duration, fn func()) {
	for panicked(task, fn) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(pause):
		}
	}
}

// panicked runs fn and reports whether it panicked, logging the panic if so.
func panicked(task string, fn func()) (did bool) {
	defer func() {
		if p := recover(); p != nil {
			did = true
			slog.Error("panic in the background",
				"task", task, "err", fmt.Sprint(p), "stack", string(debug.Stack()))
		}
	}()
	fn()
	return false
}
