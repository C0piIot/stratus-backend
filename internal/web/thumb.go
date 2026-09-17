package web

import (
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/media"
	"github.com/C0piIot/stratus-backend/internal/storage"
)

// thumbPrefix is where a picture of a file lives. Its own prefix rather than a
// query on the file itself, so that the far-future caching below applies to a
// path and nothing else.
const thumbPrefix = "/thumb/"

// listThumb is the size a row in the listing asks for. It is snapped to the
// generator's ladder either way; asking for the size the page actually renders
// keeps the two from drifting.
const listThumb = 96

// thumbnail serves a picture of a file, making it on the first ask.
//
// The URL carries the file's ETag and the answer is cached for a year: every
// write takes a fresh blob key and therefore a fresh ETag, so the URL changes
// exactly when the picture does and a browser never has to revalidate one.
func (h *handler) thumbnail(w http.ResponseWriter, r *http.Request, user string) {
	p, err := toPath(r.PathValue("path"))
	if err != nil {
		h.fail(w, r, user, err)
		return
	}

	size := listThumb
	if raw := r.URL.Query().Get("size"); raw != "" {
		if px, perr := strconv.Atoi(raw); perr == nil && px > 0 {
			size = px
		}
	}

	body, length, err := h.thumbs.File(r.Context(), user, p, size)
	switch {
	case errors.Is(err, media.ErrNoThumbnail), errors.Is(err, db.ErrNotFound), errors.Is(err, storage.ErrNotFound):
		// One answer for "there is no such file" and "there is no picture of
		// it": from a browser they are the same thing, an image that will not
		// load, and telling them apart would only tell a guesser what exists.
		http.NotFound(w, r)
		return
	case err != nil:
		h.fail(w, r, user, err)
		return
	}
	defer func() { _ = body.Close() }()

	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	// Ignored deliberately: the response has started, so a client that hung up
	// has already been told everything there is to tell it.
	_, _ = io.Copy(w, body)
}
