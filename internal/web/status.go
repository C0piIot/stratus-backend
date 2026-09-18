package web

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/media"
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
}

// status is the page that answers "is it done yet". A library is indexed in the
// background, on a first run over an adopted bucket that can take hours, and
// before this there was nothing to look at but a log line.
func (h *handler) status(w http.ResponseWriter, r *http.Request, user string) {
	counts, err := h.indexing.Index.MediaCounts(r.Context(), media.Version)
	if err != nil {
		h.fail(w, r, user, err)
		return
	}

	v := view{
		Title:   "Status",
		User:    user,
		Counts:  counts,
		Percent: percent(counts),
		// The version is on the page because it is what a re-index moves: an
		// operator who raised it wants to see the numbers fall and climb again.
		IndexVersion:  media.Version,
		IndexInterval: h.indexing.Interval.String(),
		IndexingOff:   h.indexing.Interval == 0,
	}

	if r.Header.Get("HX-Request") == "true" {
		h.renderTemplate(w, http.StatusOK, pageStatus, countsFragment, v)
		return
	}
	h.render(w, http.StatusOK, pageStatus, v)
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

// indexingOf marks the rows of a listing the indexer has not dealt with yet.
//
// The states are one query for the page being rendered, and a failure here is
// not a failure of the listing: the mark is decoration, and a folder that will
// not open because the decoration could not be fetched would be a worse page
// than one without it. So it is logged and the listing goes out plain.
func (h *handler) indexingOf(r *http.Request, children []db.File) map[int64]string {
	ids := make([]int64, 0, len(children))
	for _, c := range children {
		if !c.IsDir {
			ids = append(ids, c.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}

	states, err := h.indexing.Index.MediaStates(r.Context(), ids)
	if err != nil {
		slog.Error("reading what is indexed", "path", r.URL.Path, "err", err)
		return nil
	}

	marks := make(map[int64]string, len(ids))
	for _, c := range children {
		if c.IsDir {
			continue
		}
		switch state, ok := states[c.ID]; {
		case !ok, state.Version < media.Version, state.ETag != c.ETag:
			// The same three conditions the queue asks about, which is what
			// keeps the page from saying "waiting" about a file nothing will
			// come back to, or "done" about one that is about to be read again.
			marks[c.ID] = "waiting"
		case state.Failed:
			marks[c.ID] = "unreadable"
		}
	}
	return marks
}
