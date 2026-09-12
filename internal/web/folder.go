package web

import (
	"net/http"
	"path"
)

// folderPrefix is where a new directory is asked for. A second path rather than
// a second meaning for POST /files/: the form there sends files and this one
// sends a name, and telling them apart by what happens to be in the body is the
// kind of cleverness that is wrong once and then permanent.
const folderPrefix = "/folders/"

// newFolder makes a directory inside the one the form was shown in, and sends
// the browser into it -- which is where somebody who just made a folder is
// going anyway.
func (h *handler) newFolder(w http.ResponseWriter, r *http.Request, user string) {
	parent, err := toPath(r.PathValue("path"))
	if err != nil {
		h.fail(w, r, user, err)
		return
	}

	// The last element only, for the reason an upload's filename is: what
	// arrives is whatever the client typed, and a name is not a path. What is
	// left can still be nothing, or the dots that mean somewhere else -- and
	// joining those would quietly name a directory that already exists.
	name := uploadName(r.PostFormValue("name"))
	switch name {
	case ".", "..", "/":
		h.badRequest(w, user, "A folder needs a name.")
		return
	}
	target := path.Join(parent, name)

	// Mkdir validates the path, refuses to sit on top of anything, and requires
	// the parent to be there -- so there is nothing left to check here.
	if _, err := h.files.Mkdir(r.Context(), user, target); err != nil {
		h.fail(w, r, user, err)
		return
	}
	//nolint:gosec // G710: href builds a path under /files/ out of what toPath already tidied.
	http.Redirect(w, r, href(target), http.StatusSeeOther)
}
