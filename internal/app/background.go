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

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/media"
)

// indexPeriodically extracts metadata from files that have none, and makes the
// pictures a grid will ask for out of what it has just read.
//
// A write hands over the file it just wrote, so the ordinary case is one
// extraction and no query at all. The query is the safety net behind it, and it
// runs on the interval -- or sooner, when the indexer says it dropped a notice
// and therefore does not know what landed.
//
// **The thumbnails are this loop's second half rather than a loop of their
// own** (#341), because they are the same file opened for the same reason a
// second time: the write announces it, a query finds the rest, and a verdict
// goes on the row either way. Sharing the goroutine is also what keeps the two
// from bidding against each other for the one CPU a small instance has.
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

	// again is armed when there is more to do than one batch of pictures, so
	// the next one happens now rather than on the hour -- and through the
	// select, so a file that has just landed is indexed before the next eight
	// are decoded.
	var again <-chan time.Time
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
				continue
			}
			// The picture of a file that has just been written, while it is
			// still in hand: the phone that uploaded it is the one about to
			// open the grid, and this costs no query at all.
			a.thumbFor(ctx, deps, f)
			continue

		case <-deps.Indexer.LostTrack():
			// More arrived at once than could be held, so what was dropped is
			// only a row now -- and a row is what the query finds.
		case <-ticker.C:
		case <-again:
		}

		if !a.indexUntilDry(ctx, deps) {
			return
		}
		again = a.thumbsAhead(ctx, deps)
	}
}

// thumbPause is the breath between two batches of pictures, and thumbBusyPause
// how long the pass stands aside when somebody is looking at a page.
//
// Both are about being a background pass rather than a loop that holds a
// decoder for as long as there is work: a minute of being out of the way costs
// a library nothing, and the alternative -- waiting for the hour's tick --
// would mean a grid opened at the wrong moment stopped the pass until then.
const (
	thumbPause     = time.Second
	thumbBusyPause = time.Minute
)

// thumbsAhead makes one batch of the pictures nobody has asked for yet, and
// answers when to come back: now while a batch keeps filling, in a minute if
// the machine is busy, and otherwise on the hour's own tick.
func (a *App) thumbsAhead(ctx context.Context, deps Deps) <-chan time.Time {
	if !a.cfg.ThumbsAhead || deps.Thumbs == nil {
		return nil
	}

	made, err := deps.Thumbs.PrepareBatch(ctx, time.Now())
	switch {
	case errors.Is(err, context.Canceled):
		return nil
	case errors.Is(err, media.ErrBusy):
		// Which is not a failure: the decoders are for whoever is looking, and
		// this is the pass getting out of the way.
		return time.After(thumbBusyPause)
	case err != nil:
		slog.Error("making thumbnails ahead", "err", err)
		return nil
	case made > 0:
		slog.Info("made thumbnails ahead", "files", made)
	}

	if made < media.ThumbBatch {
		return nil
	}
	return time.After(thumbPause)
}

// thumbFor makes the pictures of one file that has just been written.
//
// Only for what a grid draws, by the name: a track's cover is a second picture
// per blob and answers a different surface, and everything else has none.
func (a *App) thumbFor(ctx context.Context, deps Deps, f db.File) {
	if !a.cfg.ThumbsAhead || deps.Thumbs == nil {
		return
	}
	if !media.IsImage(f.Path) && !media.IsVideo(f.Path) {
		return
	}
	switch _, err := deps.Thumbs.Prepare(ctx, f); {
	case err == nil, errors.Is(err, media.ErrBusy), errors.Is(err, context.Canceled):
	default:
		slog.Info("making a thumbnail ahead", "path", f.Path, "err", err)
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
// A daily tick, because this is the net rather than the collector: what gives
// a blob up on purpose gives it up at the moment it does (#272), and what
// reaches here is what nobody was left to announce -- a process killed between
// the blob and its row, a tidy-up the store refused, a thumbnail whose
// generator has moved on.
func (a *App) collectPeriodically(ctx context.Context, deps Deps) {
	service := deps.Files
	ticker := time.NewTicker(a.cfg.GCInterval)
	defer ticker.Stop()

	slog.Info("collecting orphan blobs, abandoned uploads and the trash",
		"every", a.cfg.GCInterval, "grace", a.cfg.GCGrace,
		"upload_ttl", files.DefaultUploadTTL, "trash_retention", files.DefaultTrashRetention)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		// The trash first, because what leaves it becomes garbage the sweep
		// below can then take in the same pass rather than a day later: the
		// rows go, and the blobs with them.
		switch done, err := service.EmptyTrash(ctx, time.Now().Add(-files.DefaultTrashRetention)); {
		case errors.Is(err, context.Canceled):
			return
		case err != nil:
			slog.Error("emptying the trash", "err", err)
		case done.Batches > 0:
			slog.Info("emptied the trash",
				"deletions", done.Batches, "files", done.Files, "bytes", done.Bytes)
		}

		// Uploads next: an abandoned one holds bytes that are invisible to a
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
		case done.Deleted > 0 || done.Trashed > 0:
			// Two fates and two numbers: what was destroyed is gone, and what
			// was moved is in the trash and still costs the same room (#276).
			slog.Info("swept the blob store",
				"scanned", done.Scanned, "deleted", done.Deleted,
				"freed", done.Bytes, "unaccounted", done.Trashed)
		}
	}
}

// importPeriodically moves what has landed in the import folder into the
// library, for as long as ctx lives.
//
// A ticker, like the two above, and for the same reason: the interval is also
// what decides a file has finished arriving, so a pass that restarted the clock
// after a slow import would keep moving the moment the next file qualifies.
func (a *App) importPeriodically(ctx context.Context, deps Deps) {
	slog.Info("importing what arrives on disk",
		"dir", deps.Incoming.Dir(), "every", a.cfg.IncomingInterval)

	ticker := time.NewTicker(a.cfg.IncomingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		switch imported, err := deps.Incoming.Pass(ctx); {
		case errors.Is(err, context.Canceled):
			return
		case err != nil:
			// Logged and carried on with: the files that did import are
			// imported, and the ones that did not are still on disk.
			slog.Error("importing what arrived on disk", "imported", imported, "err", err)
		case imported > 0:
			slog.Info("imported what arrived on disk", "files", imported)
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
