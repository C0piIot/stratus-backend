package web

import (
	"net/http"
	"path"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/files"
)

// deletePrefix is where a delete is confirmed. The page stays now that there
// is a trash bin behind it (#274): deleting a folder is still worth a question,
// and the answer it gives has changed from "this cannot be undone" to how long
// you have to change your mind.
const deletePrefix = "/delete/"

func (h *handler) deleteForm(w http.ResponseWriter, r *http.Request, user string) {
	target, f, ok := h.targetOf(w, r, user)
	if !ok {
		return
	}
	h.render(w, http.StatusOK, pageDelete, view{
		Title: "Delete", User: user,
		Name: path.Base(target), IsDir: f.IsDir,
		Action: link(deletePrefix, target), Back: href(db.ParentOf(target)),
		Kept: int(files.DefaultTrashRetention / (24 * time.Hour)),
	})
}

// remove deletes a file, or a directory and everything under it.
func (h *handler) remove(w http.ResponseWriter, r *http.Request, user string) {
	target, _, ok := h.targetOf(w, r, user)
	if !ok {
		return
	}
	if err := h.files.Remove(r.Context(), user, target); err != nil {
		h.fail(w, r, user, err)
		return
	}
	redirectLocal(w, r, href(db.ParentOf(target)))
}
