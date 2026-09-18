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
	"log/slog"
	"time"

	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/media"
)

// indexPeriodically extracts metadata from files that have none.
//
// When a pass finds a full batch it comes straight back for more, so a first
// run over an existing library goes as fast as the extractors allow; when it
// finds nothing it waits -- for the interval, or for a write to say there is
// something to do, whichever comes first. One worker: ffprobe on four cores
// that are also serving requests does not want company.
func (a *App) indexPeriodically(ctx context.Context, deps Deps) {
	slog.Info("indexing media", "idle", a.cfg.IndexInterval, "version", media.Version)

	for {
		indexed, err := deps.Indexer.IndexBatch(ctx, time.Now())
		switch {
		case errors.Is(err, context.Canceled):
			return
		case err != nil:
			slog.Error("indexing media", "err", err)
		case indexed > 0:
			slog.Info("indexed media", "files", indexed)
		}

		if indexed == media.BatchSize && err == nil {
			continue // a full batch means there is probably more
		}
		select {
		case <-ctx.Done():
			return
		case <-deps.Indexer.Woken():
			// Something was just written. The interval is the idle poll and
			// the safety net -- for a version bump, for rows an import
			// inserted, for anything that landed while this process was not
			// running -- and this is the ordinary case arriving on time.
		case <-time.After(a.cfg.IndexInterval):
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
