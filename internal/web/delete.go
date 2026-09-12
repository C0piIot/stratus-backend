package web

import (
	"net/http"
	"path"
)

// deletePrefix is where a delete is confirmed. There is no trash bin and the
// blob is swept up afterwards, so the page in between is the only chance to
// have not meant it.
const deletePrefix = "/delete/"

func (h *handler) deleteForm(w http.ResponseWriter, r *http.Request, user string) {
	target, f, ok := h.editing(w, r, user)
	if !ok {
		return
	}
	h.render(w, http.StatusOK, pageDelete, view{
		Title: "Delete", User: user,
		Name: path.Base(target), IsDir: f.IsDir,
		Action: link(deletePrefix, target), Back: href(parentOf(target)),
	})
}

// remove deletes a file, or a directory and everything under it.
func (h *handler) remove(w http.ResponseWriter, r *http.Request, user string) {
	target, _, ok := h.editing(w, r, user)
	if !ok {
		return
	}
	if err := h.files.Remove(r.Context(), user, target); err != nil {
		h.fail(w, r, user, err)
		return
	}
	//nolint:gosec // G710: href builds a path under /files/ out of what toPath already tidied.
	http.Redirect(w, r, href(parentOf(target)), http.StatusSeeOther)
}
