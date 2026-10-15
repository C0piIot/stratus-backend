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
	Index    db.MediaIndex
	Interval time.Duration
	// Ahead is whether pictures are also made before anything asks (#341).
	// Off is a decision too, and one this page has to say out loud: the
	// number of pictures then only moves when somebody browses, which looks
	// like a pass that is stuck.
	Ahead bool
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
		Incoming:  h.incomingOf(),
		// The version is on the page because it is what a re-index moves: an
		// operator who raised it wants to see the numbers fall and climb again.
		IndexVersion: media.Version,
		IndexingOff:  h.indexing.Interval == 0,
		PicturesOff:  !h.indexing.Ahead,
	}

	if r.Header.Get("HX-Request") == "true" {
		h.renderTemplate(w, http.StatusOK, pageStatus, countsFragment, v)
		return
	}
	h.render(w, http.StatusOK, pageStatus, v)
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

// rowState is what one query about a page of files tells the page: whether the
// indexer has been there, and whether to offer a picture at all.
type rowState struct {
	// Indexing is the badge a listing draws, and empty for a file that has
	// been read.
	Indexing string
	// NoPicture says this build has already tried and could not, so the page
	// draws the mark alone rather than asking for a thumbnail that will
	// answer 404 (#341).
	NoPicture bool
}

// statesOf marks the rows of a page: the ones the indexer has not dealt with
// yet, and the ones nothing here can draw.
//
// The states are one query for the page being rendered, and a failure here is
// not a failure of the page: both marks are decoration, and a folder that will
// not open because the decoration could not be fetched would be a worse page
// than one without it. So it is logged and the page goes out plain -- which
// for the picture means offering one that may not arrive, exactly as it did
// before this was read at all.
//
// where is what the log line names, since this is called from pages that are
// not listings.
func (h *handler) statesOf(ctx context.Context, where string, children []db.File) map[int64]rowState {
	ids := make([]int64, 0, len(children))
	for _, c := range children {
		if !c.IsDir {
			ids = append(ids, c.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}

	states, err := h.indexing.Index.MediaStates(ctx, ids)
	if err != nil {
		slog.Error("reading what is indexed", "path", where, "err", err)
		return nil
	}

	marks := make(map[int64]rowState, len(ids))
	for _, c := range children {
		if c.IsDir {
			continue
		}
		state, known := states[c.ID]
		var row rowState
		switch {
		case !known, state.Version < media.Version, state.ETag != c.ETag:
			// The same three conditions the queue asks about, which is what
			// keeps the page from saying "waiting" about a file nothing will
			// come back to, or "done" about one that is about to be read again.
			row.Indexing = "waiting"
		case state.Failed:
			row.Indexing = "unreadable"
		}
		// Whether this build has already tried and could not, which is the
		// rule media.Refused owns because three surfaces read it now.
		row.NoPicture = media.Refused(state, time.Now())
		if row != (rowState{}) {
			marks[c.ID] = row
		}
	}
	return marks
}
