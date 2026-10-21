package web

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/incoming"
	"github.com/C0piIot/stratus-backend/internal/media"
	"github.com/C0piIot/stratus-backend/internal/storage"
)

// countsFragment is the part of that page htmx refreshes on its own: the
// numbers, without the page around them.
const countsFragment = "counts"

// Indexing is what the status page reports on. Interval is zero when the
// operator turned indexing off, which is a decision rather than a fault and is
// therefore said out loud rather than shown as a library that never finishes.
type Indexing struct {
	Index    MediaReads
	Interval time.Duration
	// Ahead is whether pictures are also made before anything asks (#341).
	// Off is a decision too, and one this page has to say out loud: the
	// number of pictures then only moves when somebody browses, which looks
	// like a pass that is stuck.
	Ahead bool
	// Lookups is what outside sources said about the music library (#217),
	// and Lookup whether the pass that asks them is on. The rows are read
	// either way: what was fetched is still here with the setting off, and
	// the page says which of the two it is looking at.
	Lookups db.Lookups
	Lookup  bool
}

// MediaReads is the half of the media index a page reads (#365): what is
// known about a file, how much of the library is done, and the state of a
// page of rows. Not db.MediaIndex whole, for the reason internal/dav gives
// for the same narrowing -- an inbound adapter that could write a media row
// is one that one day will.
type MediaReads interface {
	MediaByFile(ctx context.Context, fileID int64) (db.Media, error)
	MediaCounts(ctx context.Context, version, generation int) (db.MediaCounts, error)
	MediaStates(ctx context.Context, fileIDs []int64) (map[int64]db.MediaState, error)
}

// lookupView is the music half of the status page: how much of the library
// has been identified outside, and whether anything is still asking.
type lookupView struct {
	On bool
	db.LookupCounts
}

// Imports is the import folder, and nil when none is configured -- which is the
// default, so the page says nothing about it rather than saying it is empty.
//
// Read, never driven, like the indexer: there is no button here that sweeps the
// folder, for the same reason there is none that reindexes.
type Imports interface {
	Dir() string
	State() incoming.State
}

// status is the page that answers "is it done yet". A library is indexed in the
// background, on a first run over an adopted bucket that can take hours, and
// before this there was nothing to look at but a log line.
func (h *handler) status(w http.ResponseWriter, r *http.Request, user string) {
	counts, err := h.indexing.Index.MediaCounts(r.Context(), media.Version, files.DerivedGeneration)
	if err != nil {
		h.fail(w, r, user, err)
		return
	}

	v := view{
		Title:     "Status",
		User:      user,
		Counts:    counts,
		Percent:   percent(counts),
		FreeSpace: h.freeSpace(r),
		TrashSize: h.trashSize(r, user),
		Unclaimed: h.trashSize(r, ""),
		TrashHeld: h.retention() == 0,
		Incoming:  h.incomingOf(),
		// The version is on the page because it is what a re-index moves: an
		// operator who raised it wants to see the numbers fall and climb again.
		IndexVersion: media.Version,
		IndexingOff:  h.indexingNow().Interval == 0,
		PicturesOff:  !h.indexingNow().Ahead,
		Lookup:       h.lookupOf(r),
	}

	if r.Header.Get("HX-Request") == "true" {
		h.renderTemplate(w, http.StatusOK, pageStatus, countsFragment, v)
		return
	}
	h.render(w, http.StatusOK, pageStatus, v)
}

// lookupOf is what the page says about music metadata from outside, and nil
// when there is nothing to say: the pass is off and nothing was ever fetched.
// Logged rather than failed like the other lines that are not the page's
// point.
func (h *handler) lookupOf(r *http.Request) *lookupView {
	if h.indexing.Lookups == nil {
		return nil
	}
	counts, err := h.indexing.Lookups.LookupCounts(r.Context())
	if err != nil {
		slog.WarnContext(r.Context(), "counting music lookups", "err", err)
		return nil
	}
	if !h.indexingNow().Lookup && counts.AlbumsTried == 0 && counts.TracksTried == 0 {
		return nil
	}
	return &lookupView{On: h.indexingNow().Lookup, LookupCounts: counts}
}

// incomingOf is what the page says about the import folder, and nil when there
// is none to say anything about.
func (h *handler) incomingOf() *importsView {
	if h.imports == nil {
		return nil
	}
	state := h.imports.State()
	v := importsView{
		Dir:      h.imports.Dir(),
		Waiting:  state.Waiting,
		Imported: state.Imported,
		Error:    state.LastError,
	}
	if !state.LastRun.IsZero() {
		v.LastRun = state.LastRun.Format("15:04:05")
	}
	return &v
}

// importsView is that, already rendered.
type importsView struct {
	Dir      string
	Waiting  int
	Imported int64
	LastRun  string
	// Error is why the last pass did not empty the folder. The page shows it
	// because a file that will not import is otherwise a file that sits there
	// silently for ever.
	Error string
}

// trashSize is how much room the trash is still holding, rendered, and empty
// when it holds none. Like freeSpace it logs rather than failing the page:
// this is one line on a page about something else.
//
// The owner is the filter, and "" is what nobody owns: the blobs the sweep
// could not account for. Two numbers rather than one, because they mean
// different things -- what you deleted is a decision, and what the server
// cannot explain is a symptom (#276).
func (h *handler) trashSize(r *http.Request, user string) string {
	totals, err := h.files.TrashTotals(r.Context(), user)
	switch {
	case err != nil:
		slog.WarnContext(r.Context(), "measuring the trash", "err", err)
		return ""
	case totals.Files == 0:
		return ""
	}
	return humanBytes(totals.Bytes)
}

// freeSpace is how much room the blob store says is left, rendered.
//
// A failure is not a failure of the page: this is one line on a page about
// something else, and a server that would not say how much room it has is
// still able to say how much of the library it has read. It logs and renders
// nothing, the same way the listing's indexing marks do.
func (h *handler) freeSpace(r *http.Request) string {
	free, err := h.files.FreeSpace(r.Context())
	switch {
	case err != nil:
		slog.WarnContext(r.Context(), "reading the free space", "err", err)
		return ""
	case free == storage.Unlimited:
		// Not a number, and not an evasion either: a bucket has no size the
		// API will admit to, so this is the whole of what is known.
		return "unlimited"
	}
	return humanBytes(free)
}

// humanBytes renders a byte count the way somebody reads one. Binary units and
// their real names, because what a filesystem reports is what you can write and
// rounding it to a marketing gigabyte would make the number wrong in the
// direction that matters.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for rest := n / unit; rest >= unit; rest /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// percent is how much of the library has been looked at, rounded down so that
// it only says a hundred when there is nothing left. A library with no files in
// it is finished, not zero percent of the way there.
func percent(c db.MediaCounts) int {
	if c.Files == 0 {
		return 100
	}
	done := c.Indexed + c.Failed
	if done >= c.Files {
		return 100
	}
	return int(done * 100 / c.Files)
}
