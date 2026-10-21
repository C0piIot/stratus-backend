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

	"github.com/C0piIot/stratus-backend/internal/calendar"
	"github.com/C0piIot/stratus-backend/internal/config"
	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/media"
	"github.com/C0piIot/stratus-backend/internal/media/thumb"
	"github.com/C0piIot/stratus-backend/internal/web"
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
	for {
		changed := changes(deps)
		d := a.values(deps).IndexInterval
		if d <= 0 {
			// Off, notices included: they pile up past what the channel
			// holds, and LostTrack is what makes the query find them the day
			// the setting comes back.
			slog.Info("media indexing off until a setting changes", "reason", "STRATUS_INDEX_INTERVAL is zero")
			select {
			case <-ctx.Done():
				return
			case <-changed:
				continue
			}
		}
		if !a.indexEvery(ctx, deps, changed, d) {
			return
		}
	}
}

// indexEvery is the indexer with its safety net at d, until ctx ends (false)
// or the interval is changed (true).
func (a *App) indexEvery(ctx context.Context, deps Deps, changed <-chan struct{}, d time.Duration) bool {
	slog.Info("indexing media", "safety_net", d, "version", media.Version)

	// A ticker and not a timer armed after each pass: with a steady trickle of
	// uploads an interval that restarted every time would never arrive, and
	// what waits behind it -- a version bump, rows an import inserted, a file
	// deferred until its retry time -- would never be looked at.
	ticker := time.NewTicker(d)
	defer ticker.Stop()

	// Whatever landed while this pass was not running is nobody's notice.
	if !a.indexUntilDry(ctx, deps) {
		return false
	}

	// again is armed when the pass has said when to come back, so the next
	// batch happens then rather than on the hour -- and through the select, so
	// a file that has just landed is indexed before the next eight are
	// decoded.
	var again <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return false

		case <-changed:
			changed = changes(deps)
			if a.values(deps).IndexInterval != d {
				return true
			}
			continue

		case f := <-deps.Indexer.Noticed():
			switch err := deps.Indexer.IndexFile(ctx, time.Now(), f); {
			case errors.Is(err, context.Canceled):
				return false
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
			return false
		}
		again = after(a.thumbsAhead(ctx, deps))
	}
}

// after is the timer a pass asks for, and nothing at all for a wait of zero:
// a nil channel never fires, which is this loop waiting for its own tick.
func after(wait time.Duration) <-chan time.Time {
	if wait <= 0 {
		return nil
	}
	return time.After(wait)
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
// answers how long to wait before the next: thumbPause while a batch keeps
// filling, thumbBusyPause when somebody is looking, and zero for "whenever the
// loop comes round anyway".
//
// A duration rather than the timer itself, because what is worth testing here
// is the number and not the channel.
func (a *App) thumbsAhead(ctx context.Context, deps Deps) time.Duration {
	if !a.values(deps).ThumbsAhead || deps.Thumbs == nil {
		return 0
	}

	made, err := deps.Thumbs.PrepareBatch(ctx, time.Now())
	switch {
	case errors.Is(err, context.Canceled):
		return 0
	case errors.Is(err, media.ErrBusy):
		// Which is not a failure: the decoders are for whoever is looking, and
		// this is the pass getting out of the way.
		return thumbBusyPause
	case err != nil:
		slog.Error("making thumbnails ahead", "err", err)
		return 0
	case made > 0:
		slog.Info("made thumbnails ahead", "files", made)
	}

	if made < thumb.Batch {
		return 0
	}
	return thumbPause
}

// thumbFor makes the pictures of one file that has just been written.
//
// Only for what a grid draws, by the name: a track's cover is a second picture
// per blob and answers a different surface, and everything else has none.
func (a *App) thumbFor(ctx context.Context, deps Deps, f db.File) {
	if !a.values(deps).ThumbsAhead || deps.Thumbs == nil {
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
	v := a.values(deps)
	slog.Info("collecting orphan blobs, abandoned uploads and the trash",
		"every", v.GCInterval, "grace", v.GCGrace,
		"upload_ttl", files.DefaultUploadTTL, "trash_retention", retention(v.TrashRetention))

	a.every(ctx, deps, "collecting orphan blobs", func(v config.Values) time.Duration { return v.GCInterval }, func() bool {
		v := a.values(deps)
		swept := web.Sweep{}
		defer func() {
			swept.At = time.Now()
			a.swept.Store(&swept)
		}()
		// The trash first, because what leaves it becomes garbage the sweep
		// below can then take in the same pass rather than a day later: the
		// rows go, and the blobs with them.
		if !a.emptyTrash(ctx, service, v.TrashRetention, &swept) {
			return false
		}

		// Uploads next: an abandoned one holds bytes that are invisible to a
		// listing, so the blob sweep below can neither see them nor free them.
		// Nothing else collects them at all.
		switch done, err := service.CollectUploads(ctx, time.Now()); {
		case errors.Is(err, context.Canceled):
			return false
		case err != nil:
			slog.Error("collecting abandoned uploads", "err", err)
			failed(&swept, "collecting abandoned uploads", err)
		case done > 0:
			slog.Info("collected abandoned uploads", "count", done)
			swept.Uploads = done
		}

		if !pruneCalendars(ctx, deps.Database) {
			return false
		}

		switch done, err := service.Collect(ctx, v.GCGrace); {
		case errors.Is(err, context.Canceled):
			return false
		case errors.Is(err, files.ErrEmptyIndex):
			// Almost certainly a database pointed somewhere new rather than a
			// library somebody emptied, so nothing is deleted and it says so.
			slog.Warn("skipped collecting orphan blobs",
				"reason", "the database references no blobs and the store is not empty")
			failed(&swept, "collecting orphan blobs", err)
		case err != nil:
			slog.Error("collecting orphan blobs", "err", err)
			failed(&swept, "collecting orphan blobs", err)
		case done.Deleted > 0 || done.Trashed > 0:
			swept.Deleted, swept.Unaccounted = done.Deleted, done.Trashed
			swept.Freed += done.Bytes
			// Two fates and two numbers: what was destroyed is gone, and what
			// was moved is in the trash and still costs the same room (#276).
			slog.Info("swept the blob store",
				"scanned", done.Scanned, "deleted", done.Deleted,
				"freed", done.Bytes, "unaccounted", done.Trashed)
		}
		return true
	})
}

// importPeriodically moves what has landed in the import folder into the
// library, for as long as ctx lives.
//
// A ticker, like the two above, and for the same reason: the interval is also
// what decides a file has finished arriving, so a pass that restarted the clock
// after a slow import would keep moving the moment the next file qualifies.
func (a *App) importPeriodically(ctx context.Context, deps Deps) {
	slog.Info("importing what arrives on disk",
		"dir", deps.Incoming.Dir(), "every", a.values(deps).IncomingInterval)

	a.every(ctx, deps, "importing what arrived on disk", func(v config.Values) time.Duration { return v.IncomingInterval }, func() bool {
		switch imported, err := deps.Incoming.Pass(ctx); {
		case errors.Is(err, context.Canceled):
			return false
		case err != nil:
			// Logged and carried on with: the files that did import are
			// imported, and the ones that did not are still on disk.
			slog.Error("importing what arrived on disk", "imported", imported, "err", err)
		case imported > 0:
			slog.Info("imported what arrived on disk", "files", imported)
		}
		return true
	})
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

// lookupInterval is how often the pass asks what is left to look up when the
// last pass found nothing. An hour, like the indexer's safety net and for the
// same reasons: what waits behind it is a row whose retry time has come, and
// a library that arrived while nothing was running.
const lookupInterval = time.Hour

// lookupPause is the breath between two passes that both filled a batch. A
// few seconds and not nothing, because a pass is already paced a second per
// request and the pause is what lets a shutdown in between.
const lookupPause = 5 * time.Second

// lookupBackoff is how long the loop stays away after a source turned the
// client away. A minute: long enough to be a backoff -- MusicBrainz's limit
// is per address, and on a platform whose egress address is shared with
// other tenants a 503 says nothing about this server's own pace -- and short
// enough that the rest of the batch is not an hour behind one refusal.
const lookupBackoff = time.Minute

// lookupPeriodically asks outside sources about the music library, for as
// long as ctx lives (#217).
//
// The pass paces itself -- one request a second to each service -- so this
// loop only decides when to run one: again soon while a batch keeps filling,
// a minute away after a source turned the client away, and on the hour
// otherwise. The refused rows themselves wait the hour their retry time
// says; the minute is for the rest of the batch behind them.
func (a *App) lookupPeriodically(ctx context.Context, deps Deps) {
	slog.Info("looking up music metadata", "safety_net", lookupInterval)

	ticker := time.NewTicker(lookupInterval)
	defer ticker.Stop()

	// Armed at once: whatever is pending was pending while nothing ran.
	again := time.After(0)
	for {
		changed := changes(deps)
		if !a.values(deps).MusicLookup {
			// Asks nothing while the setting is off, and asks at once when
			// it is turned on: what is pending was pending all along.
			select {
			case <-ctx.Done():
				return
			case <-changed:
				again = time.After(0)
				continue
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-changed:
			continue
		case <-ticker.C:
		case <-again:
		}
		again = nil

		done, err := deps.Lookup.Pass(ctx)
		switch {
		case errors.Is(err, context.Canceled):
			return
		case err != nil:
			slog.Error("looking up music metadata", "err", err)
			continue
		case done.Albums > 0 || done.Lyrics > 0 || done.Deferred > 0:
			slog.Info("looked up music metadata",
				"albums", done.Albums, "covers", done.Covers, "lyrics", done.Lyrics, "deferred", done.Deferred)
		}
		switch {
		case done.Refused:
			again = time.After(lookupBackoff)
		case done.More:
			again = time.After(a.lookupPause)
		}
	}
}

// emptyTrash is the trash's third of a collector pass, and false when the
// context has gone.
//
// With the retention at never it does nothing at all, and only this: the
// uploads and the blob sweep behind it still run, and the trashed keys are
// still what Collect counts as referenced, so holding the trash is not a
// reason for the sweep to eat it (#296).
func (a *App) emptyTrash(ctx context.Context, service *files.Service, kept time.Duration, swept *web.Sweep) bool {
	if kept == 0 {
		return true
	}
	switch done, err := service.EmptyTrash(ctx, time.Now().Add(-kept)); {
	case errors.Is(err, context.Canceled):
		return false
	case err != nil:
		slog.Error("emptying the trash", "err", err)
		failed(swept, "emptying the trash", err)
	case done.Batches > 0:
		swept.Emptied, swept.Freed = done.Files, swept.Freed+done.Bytes
		slog.Info("emptied the trash",
			"deletions", done.Batches, "files", done.Files, "bytes", done.Bytes)
	}
	return true
}

// pruneCalendars forgets old calendar deletions, and is false when the context
// has gone. What it costs a client that synced before them is one listing of
// its calendar: see calendar.TombstoneRetention.
func pruneCalendars(ctx context.Context, meta db.Store) bool {
	if meta == nil {
		return true
	}
	switch done, err := calendar.New(meta).Prune(ctx); {
	case errors.Is(err, context.Canceled):
		return false
	case err != nil:
		slog.Error("forgetting old calendar deletions", "err", err)
	case done > 0:
		slog.Info("forgot old calendar deletions", "count", done)
	}
	return true
}

// retention is the setting as a log value: zero is never, and a log line
// saying "0s" would say the opposite.
func retention(d time.Duration) any {
	if d == 0 {
		return "never"
	}
	return d
}

// failed keeps the first thing that went wrong in a pass, for the status
// page: the parts after it still ran, and the log has the rest.
func failed(s *web.Sweep, what string, err error) {
	if s.Err == "" {
		s.Err = what + ": " + err.Error()
	}
}

// lastSweep is what the collector's last pass did, and a zero Sweep before
// the first.
func (a *App) lastSweep() web.Sweep {
	if s := a.swept.Load(); s != nil {
		return *s
	}
	return web.Sweep{}
}
