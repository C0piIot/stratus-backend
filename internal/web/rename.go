package web

import (
	"net/http"
	"path"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// renamePrefix is where a new name is asked for. A page of its own rather than
// a field in every row: a listing carrying one form per file would be a lot of
// markup for the rarest thing on it.
const renamePrefix = "/rename/"

func (h *handler) renameForm(w http.ResponseWriter, r *http.Request, user string) {
	target, f, ok := h.editing(w, r, user)
	if !ok {
		return
	}
	h.render(w, http.StatusOK, pageRename, view{
		Title: "Rename", User: user,
		Name: path.Base(target), IsDir: f.IsDir,
		Action: link(renamePrefix, target), Back: href(db.ParentOf(target)),
	})
}

// rename moves a thing within the directory it is already in. Only the name
// changes: moving something elsewhere is a different gesture, and a text field
// that quietly accepted a path would be that gesture in disguise.
func (h *handler) rename(w http.ResponseWriter, r *http.Request, user string) {
	target, f, ok := h.editing(w, r, user)
	if !ok {
		return
	}

	name := baseName(r.PostFormValue("name"))
	switch name {
	case ".", "..", "/":
		h.badRequest(w, user, "A new name is needed.")
		return
	}

	parent := db.ParentOf(target)
	to := path.Join(parent, name)
	if to == target {
		// Nothing asked for, so nothing done -- and back to where the form was
		// opened from, which is what a Cancel would have done anyway.
		redirectLocal(w, r, href(parent))
		return
	}

	// The metadata port refuses to move a directory that still has anything in
	// it: rewriting every descendant's path is a feature that arrives with the
	// protocol needing it. Asked here rather than left to the error, because
	// "something is already in the way" is not what happened and a person who
	// reads that will go looking for the thing in the way.
	if f.IsDir {
		children, err := h.files.List(r.Context(), user, target)
		if err != nil {
			h.fail(w, r, user, err)
			return
		}
		if len(children) > 0 {
			// Issue #101: rewriting every descendant's path is the feature
			// this is waiting for, on every surface and not only here.
			h.conflict(w, user, "A folder with anything in it cannot be renamed yet.")
			return
		}
	}

	if err := h.files.Move(r.Context(), user, target, to); err != nil {
		h.fail(w, r, user, err)
		return
	}
	redirectLocal(w, r, href(parent))
}
