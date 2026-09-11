package subsonic

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/media"
	"github.com/C0piIot/stratus-backend/internal/storage"
)

// Art is what this adapter needs to answer getCoverArt: a folder's picture, at
// a size.
//
// One method, because where the picture comes from is not this package's
// business: beside the tracks or inside one of them is a question about media
// and not about the protocol. And the size is a number of pixels rather than one
// of a named set, so that deciding which sizes exist stays where they are made.
type Art interface {
	Cover(ctx context.Context, owner, dir string, px int) (io.ReadCloser, int64, error)
}

// defaultCoverSize is what a client that does not say gets. The specification
// makes size optional and reads its absence as "the original", which here would
// mean serving a 4000-pixel scan to fill a 300-pixel square. The largest
// thumbnail is the honest reading: this endpoint serves cover art, and the
// original is what WebDAV is for.
const defaultCoverSize = 1200

// coverArt answers the picture for an album, a song or a folder.
//
// An artist has no cover: nothing here knows which of an artist's albums should
// stand for it, and inventing a rule would be worse than the placeholder a
// client already draws. That is why artistRef declares no coverArt field --
// declaring one and never filling it is a claim, not an omission.
func (h *handler) coverArt(w http.ResponseWriter, r *http.Request, username string) {
	id, apiErr := requiredID(r)
	if apiErr != nil {
		h.failXML(w, r, *apiErr)
		return
	}

	dir, apiErr := h.coverDirectory(r, username, id)
	if apiErr != nil {
		h.failXML(w, r, *apiErr)
		return
	}

	body, size, err := h.art.Cover(r.Context(), username, dir, coverSize(r))
	switch {
	case errors.Is(err, storage.ErrNotFound), errors.Is(err, media.ErrNoEmbeddedCover):
		// Not an error worth a code of its own: an album without a picture is
		// the normal state of half a library, and a client draws a placeholder.
		h.failXML(w, r, notFound("cover art"))
		return
	case err != nil:
		// A picture that is there and will not decode, or a backend that failed.
		// Worth telling apart in a log even though a client draws the same
		// placeholder either way.
		h.failXML(w, r, h.internal(r, "read cover art", err))
		return
	}
	defer func() { _ = body.Close() }()

	// image/jpeg because that is what a thumbnail is encoded as, whatever the
	// cover was stored in.
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	if _, err := io.Copy(w, body); err != nil {
		slog.WarnContext(r.Context(), "writing cover art", "err", err)
	}
}

// coverDirectory turns an id into the folder whose picture is being asked for.
//
// Three of the four id kinds resolve: a song sits in a folder, an album's first
// track does, and a folder is one. It is the album's *first track* rather than
// the album because an album is a pair of tags and not a place -- if its tracks
// are spread over two folders, the picture comes from where the first one is.
func (h *handler) coverDirectory(r *http.Request, username, id string) (string, *apiError) {
	switch {
	case isSongID(id):
		t, apiErr := h.audioTrack(r, username, id)
		if apiErr != nil {
			return "", apiErr
		}
		return db.ParentOf(t.File.Path), nil

	case isAlbumID(id):
		artist, name, ok := parseAlbumID(id)
		if !ok {
			return "", ptr(notFound("album"))
		}
		tracks, err := h.lib.Tracks(r.Context(), username, artist, name)
		if err != nil {
			return "", ptr(h.internal(r, "list tracks", err))
		}
		if len(tracks) == 0 {
			return "", ptr(notFound("album"))
		}
		return db.ParentOf(tracks[0].File.Path), nil

	default:
		dir, ok := parseDirID(id)
		if !ok {
			return "", ptr(notFound("cover art"))
		}
		return dir, nil
	}
}

// coverSize reads the size a client asked for. Nonsense falls back to the
// default rather than refusing, for the reason every other size parameter does:
// the specification makes it optional, so a client may leave it out and a
// misspelling should not cost it the picture.
func coverSize(r *http.Request) int {
	return intParam(r.URL.Query(), "size", defaultCoverSize)
}
