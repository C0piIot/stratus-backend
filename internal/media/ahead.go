package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/storage"
)

// Pictures made before anybody asks for them, which is the indexer's second
// half rather than a loop of its own (#341).
//
// What was rejected, and stays rejected, is generating in the upload path with
// the client waiting on a decode per PUT -- see the top of thumb.go. This is
// the other shape: announced by the write, a query for everything else, a
// verdict per file, and a number somebody can watch. A thumbnail is the same
// file opened for the same reason a second time, so it is the same pass.
//
// Two rules make it safe to run at all, and both are in Prepare. It **loses to
// whoever is looking**: a decoder is taken only when nothing else wants one,
// because a picture made with an idle CPU is worth having and the same picture
// made with the slot a grid is waiting for is the problem this was built to
// fix. And it **writes down what it finds**, so the second pass is not the
// first one again.

// passSizes are what a pass makes: the listing's box and the grid's cell, out
// of one decode. The largest stays lazy -- 1200 is one photograph at a time in
// the viewer, where a wait is a wait for the one picture somebody is looking
// at, and making it for a library would be most of the bytes for the rarest
// ask.
var passSizes = []thumbSize{thumbSmall, thumbMedium}

// ThumbBatch is how many files one pass looks at before the loop driving it
// goes back to what else has arrived.
//
// Small, and it is the indexer's number divided by what a file costs here: a
// batch of metadata is a few ranged reads and a batch of pictures is a decode
// each, so eight is about the longest this may hold the loop while somebody's
// upload waits to be indexed.
const ThumbBatch = 8

// Refused reports whether the index says this build has already tried to draw
// a file and could not, and that the verdict still stands.
//
// It is the one rule every reader of the column shares -- a listing, a grid
// and the `has-preview` a WebDAV client reads -- so it is here, beside the
// pass that writes the verdict, rather than copied into each of them.
//
// **The week is what makes a page safe to believe it.** It is the same
// files.CacheRetention the pass gives a refusal and the sweep gives the note
// beside the picture: what this build cannot draw today an upgraded one may,
// and a page that stopped offering for ever would be the reason nothing ever
// tried again.
func Refused(state db.MediaState, now time.Time) bool {
	return state.Thumb == db.ThumbRefused && now.Sub(state.ThumbAt) < files.CacheRetention
}

// Prepare makes the pictures a grid and a listing will ask for, before either
// does, and writes down what became of them.
//
// Three ways it does not make one, and only the last is the file's fault:
// ErrBusy, which is the pass losing to whoever is looking; an error reaching
// the bytes, which is nobody's verdict and leaves the row untouched so the
// next pass tries again (#157); and a file this build cannot read, which is
// remembered.
func (t *Thumbs) Prepare(ctx context.Context, f db.File) (db.ThumbState, error) {
	v := verdict{note: verdictKey(f.BlobKey), says: ErrNoThumbnail, file: f.ID, blobKey: f.BlobKey}

	if !CanThumbnail(f.Path, f.Size) {
		// Written down here although the lazy path writes nothing for the same
		// answer, and the difference is that this one has a queue: a pass
		// ordered by date would otherwise offer the same unreadable file at
		// the head of every batch for ever. What makes it safe to record a
		// property of the build rather than of the file is that the verdict
		// goes stale -- see PendingThumbs.
		t.record(ctx, v, db.ThumbRefused)
		return db.ThumbRefused, nil
	}

	missing, err := t.missingSizes(ctx, f)
	switch {
	case err != nil:
		return 0, err
	case len(missing) == 0:
		// Made already, by a request that got here first or by a pass before
		// the column existed. The row is the only thing that did not know.
		t.record(ctx, v, db.ThumbMade)
		return db.ThumbMade, nil
	}
	if t.refusedBefore(ctx, v.note) {
		t.record(ctx, v, db.ThumbRefused)
		return db.ThumbRefused, nil
	}

	release, taken := t.decodeIfIdle()
	if !taken {
		return 0, ErrBusy
	}
	defer release()

	made := make(map[thumbSize][]byte, len(missing))
	if err := t.decodeInto(ctx, f, missing, made); err != nil {
		t.remember(ctx, v, err)
		if isVerdict(ctx, err) {
			return db.ThumbRefused, nil
		}
		return 0, err
	}

	for size, picture := range made {
		if err := t.keep(ctx, f, size, picture); err != nil {
			// The decode happened and the bytes are right; what failed is the
			// store. Saying so and leaving the row alone is what brings the
			// file back on the next pass.
			return 0, err
		}
	}
	t.record(ctx, v, db.ThumbMade)
	return db.ThumbMade, nil
}

// PrepareBatch makes the pictures of up to ThumbBatch files that have none,
// newest first, and answers how many it reached a verdict about.
//
// Photographs before films, and the batch is shared: a library is mostly
// photographs, the gallery is the grid somebody opens, and a film nobody has
// reached still makes its own picture the moment anybody looks at it. ErrBusy
// is not a failure -- it is the pass standing aside, and the caller decides
// when to come back.
func (t *Thumbs) PrepareBatch(ctx context.Context, now time.Time) (int, error) {
	var done int
	for _, kind := range []db.Kind{db.KindImage, db.KindVideo} {
		// The week is files.CacheRetention, which is the same one the sweep
		// takes a note after: the two halves of a refusal expire together
		// because they are given the same number (#338).
		pending, err := t.meta.PendingThumbs(ctx, kind, files.DerivedGeneration,
			now.Add(-files.CacheRetention), ThumbBatch-done)
		if err != nil {
			return done, fmt.Errorf("find pictures to make: %w", err)
		}

		for _, f := range pending {
			switch _, err := t.Prepare(ctx, f); {
			case errors.Is(err, ErrBusy):
				return done, ErrBusy
			case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
				return done, err
			case err != nil:
				// The bytes could not be reached, which says nothing about the
				// file: the row is untouched and the next pass offers it again.
				slog.InfoContext(ctx, "making a thumbnail ahead", "path", f.Path, "err", err)
			}
			done++
		}
		if done >= ThumbBatch {
			break
		}
	}
	return done, nil
}

// missingSizes is which of the sizes a pass makes are not in the store yet, so
// that a library browsed before this existed is not decoded a second time.
//
// A Stat each, which is a HEAD each on S3 -- paid once per file, because what
// it learns is then on the row and the query does the asking from there.
func (t *Thumbs) missingSizes(ctx context.Context, f db.File) ([]thumbSize, error) {
	var missing []thumbSize
	for _, size := range passSizes {
		switch _, err := t.blobs.Stat(ctx, thumbKey(f.BlobKey, size)); {
		case err == nil:
		case errors.Is(err, storage.ErrNotFound):
			missing = append(missing, size)
		default:
			return nil, fmt.Errorf("%w: look for a thumbnail of %q: %w", errUnreachable, f.Path, err)
		}
	}
	return missing, nil
}

// keep stores one picture a pass made.
func (t *Thumbs) keep(ctx context.Context, f db.File, size thumbSize, picture []byte) error {
	key := thumbKey(f.BlobKey, size)
	if _, err := t.blobs.Put(ctx, key, bytes.NewReader(picture), int64(len(picture))); err != nil {
		return fmt.Errorf("%w: store %q: %w", errUnreachable, key, err)
	}
	return nil
}

// decodeIfIdle takes a decoder only when nothing else wants one.
//
// That is the whole of how a pass loses to whoever is looking, and it is why
// there is no second semaphore: the counter in front of the decoders already
// says how much of this machine thumbnails hold, so a pass that reads it as
// anything but zero has its answer. It counts itself in while it decodes, so a
// request arriving meanwhile queues as it would behind any other decode rather
// than being told the machine is free.
func (t *Thumbs) decodeIfIdle() (func(), bool) {
	if t.inQueue.Load() != 0 {
		return nil, false
	}
	t.inQueue.Add(1)
	select {
	case t.decoding <- struct{}{}:
		return func() { <-t.decoding; t.inQueue.Add(-1) }, true
	default:
		// Somebody took the last slot between the two lines above, which is
		// the race this answers by standing aside rather than by locking.
		t.inQueue.Add(-1)
		return nil, false
	}
}
