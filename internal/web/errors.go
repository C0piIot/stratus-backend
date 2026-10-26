package web

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/stratus-private-cloud/stratus-backend/internal/db"
	"github.com/stratus-private-cloud/stratus-backend/internal/storage"
)

// How a page answers when it cannot: the one place a sentinel becomes a
// status and a page, so that two handlers cannot disagree about what a
// missing file looks like (#365).

// fail turns what the layers below return into the page a browser should see.
func (h *handler) fail(w http.ResponseWriter, r *http.Request, user string, err error) {
	switch {
	// Whoever asked has gone, so there is nobody to show a page to and nothing
	// that went wrong here.
	case r.Context().Err() != nil && errors.Is(err, r.Context().Err()):
		return
	case errors.Is(err, db.ErrNotFound), errors.Is(err, storage.ErrNotFound):
		h.render(w, http.StatusNotFound, pageError, view{
			Title: "Not found", User: user,
			Message: "There is nothing at " + r.URL.Path + ".",
		})
	case errors.Is(err, db.ErrInvalidPath), errors.Is(err, storage.ErrInvalidKey):
		h.render(w, http.StatusBadRequest, pageError, view{
			Title: "Bad request", User: user,
			Message: "That is not a path this server can answer.",
		})
	// 409 rather than 400, for the reason WebDAV gives it: the request is
	// inconsistent with the tree rather than malformed.
	case errors.Is(err, db.ErrConflict):
		h.render(w, http.StatusConflict, pageError, view{
			Title: "Not possible here", User: user,
			Message: "Something is already in the way of that.",
		})
	default:
		// Anything else is this server's fault, so it is logged here and
		// described in no detail there.
		slog.Error("serving a page", "path", r.URL.Path, "err", err)
		h.render(w, http.StatusInternalServerError, pageError, view{
			Title: "Something went wrong", User: user,
			Message: "The server could not answer that. The log has the detail.",
		})
	}
}

// redirectLocal sends the browser somewhere else on this server.
//
// One function so there is one line to audit: gosec's taint analysis cannot see
// through safeNext or href -- both of which build a path under this server --
// and seven identical nolint comments scattered about would be seven places for
// a real open redirect to hide.
func redirectLocal(w http.ResponseWriter, r *http.Request, target string) {
	//nolint:gosec // G710: every caller passes href's or safeNext's output.
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// badRequest is the answer to a form that was never going to work. It is the
// client's mistake, so it says what it can and no more.
func (h *handler) badRequest(w http.ResponseWriter, user, message string) {
	h.render(w, http.StatusBadRequest, pageError, view{
		Title: "Bad request", User: user, Message: message,
	})
}

// forbidden is the answer to a link that is not good any more. No User on the
// view, because whoever is reading this has no account here.
func (h *handler) forbidden(w http.ResponseWriter, message string) {
	h.render(w, http.StatusForbidden, pageError, view{
		Title: "Not available", Message: message,
	})
}
