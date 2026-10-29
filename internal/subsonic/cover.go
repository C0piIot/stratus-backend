package subsonic

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/stratus-private-cloud/stratus-backend/internal/db"
	"github.com/stratus-private-cloud/stratus-backend/internal/media"
	"github.com/stratus-private-cloud/stratus-backend/internal/media/thumb"
	"github.com/stratus-private-cloud/stratus-backend/internal/music"
	"github.com/stratus-private-cloud/stratus-backend/internal/storage"
)

// Art is what this adapter needs to answer getCoverArt: a folder's picture, at
// a size.
//
// One method, because where the picture comes from is not this package's
// business: beside the tracks or inside one of them is a question about media
// and not about the protocol. And the size is a number of pixels rather than one
// of a named set, so that deciding which sizes exist stays where they are made.
type Art interface {
	// Cover is a folder's picture, and CoverOf an album's by one of its own
	// tracks: the two differ in a folder holding several albums.
	Cover(ctx context.Context, owner, dir string, px int) (io.ReadCloser, int64, error)
	CoverOf(ctx context.Context, owner string, f db.File, px int) (io.ReadCloser, int64, error)
}

// defaultCoverSize is what a client that does not say gets. The specification
// makes size optional and reads its absence as "the original", which here would
// mean serving a 4000-pixel scan to fill a 300-pixel square. The largest
// thumbnail is the honest reading: this endpoint serves cover art, and the
// original is what WebDAV is for.
const defaultCoverSize = 1200

// coverArt answers the picture for an album, a song, a folder or an artist.
//
// An artist's is its newest album's. It used to have none, on the argument
// that no rule for choosing an album would be right, and what that bought was
// a home screen of placeholders in every client that draws artists -- which is
// most of them. Newest because it is what the artist looks like now, and the
// rule is the year tag, so an album without one loses to any album with one.
func (h *handler) coverArt(w http.ResponseWriter, r *http.Request, username string) {
	id, apiErr := requiredID(r)
	if apiErr != nil {
		h.failXML(w, r, *apiErr)
		return
	}

	source, apiErr := h.coverSource(r, username, id)
	if apiErr != nil {
		h.failXML(w, r, *apiErr)
		return
	}

	body, size, err := source(r.Context(), coverSize(r))
	switch {
	case errors.Is(err, media.ErrBusy):
		// The protocol has no code for "not now", so it is the generic one
		// with a message that says what happened. Not logged as a failure:
		// this is the server doing what it was configured to do.
		h.failXML(w, r, apiError{errGeneric, "too many pictures are being made at once"})
		return
	case errors.Is(err, storage.ErrNotFound), errors.Is(err, thumb.ErrNoEmbeddedCover), errors.Is(err, thumb.ErrNoThumbnail):
		// Not an error worth a code of its own: an album without a picture is
		// the normal state of half a library, and a client draws a placeholder.
		// A picture beside the tracks that will not turn into pixels is the
		// same answer -- the one the album page gives, since #362 -- and a
		// line in the log, because a cover.jpg that is not a JPEG is worth
		// somebody's attention even though no client can tell it apart.
		if errors.Is(err, thumb.ErrNoThumbnail) {
			slog.WarnContext(r.Context(), "cover art that cannot be drawn", "id", id, "err", err)
		}
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

// coverSource turns an id into the question to ask the generator.
//
// Every id kind resolves: a song is asked by itself, an album by its first
// track, an artist by its newest album's, and a folder as a folder. An album is a pair of tags and
// not a place, which is why it is asked through a track of its own and not
// through the folder that track is in: a flat library keeps every album in
// one folder, and that folder's first track belongs to one of them.
func (h *handler) coverSource(r *http.Request, username, id string) (func(context.Context, int) (io.ReadCloser, int64, error), *apiError) {
	byTrack := func(f db.File) func(context.Context, int) (io.ReadCloser, int64, error) {
		return func(ctx context.Context, px int) (io.ReadCloser, int64, error) {
			return h.art.CoverOf(ctx, username, f, px)
		}
	}
	switch {
	case isSongID(id):
		t, apiErr := h.audioTrack(r, username, id)
		if apiErr != nil {
			return nil, apiErr
		}
		return byTrack(t.File), nil

	case isAlbumID(id):
		artist, name, ok := parseAlbumID(id)
		if !ok {
			return nil, ptr(notFound("album"))
		}
		first, err := music.FirstTrack(r.Context(), h.lib, username, artist, name)
		switch {
		case errors.Is(err, db.ErrNotFound):
			return nil, ptr(notFound("album"))
		case err != nil:
			return nil, ptr(h.internal(r, "list tracks", err))
		}
		return byTrack(first), nil

	case isArtistID(id):
		name, ok := parseArtistID(id)
		if !ok {
			return nil, ptr(notFound("artist"))
		}
		albums, err := h.lib.Albums(r.Context(), username, name)
		if err != nil {
			return nil, ptr(h.internal(r, "list albums", err))
		}
		if len(albums) == 0 {
			return nil, ptr(notFound("artist"))
		}
		newest := albums[0]
		for _, a := range albums[1:] {
			if a.Year > newest.Year {
				newest = a
			}
		}
		// An album is listed because it has a track, so not finding one here
		// is the library failing rather than an answer.
		first, err := music.FirstTrack(r.Context(), h.lib, username, newest.Artist, newest.Name)
		if err != nil {
			return nil, ptr(h.internal(r, "list tracks", err))
		}
		return byTrack(first), nil

	default:
		dir, ok := parseDirID(id)
		if !ok {
			return nil, ptr(notFound("cover art"))
		}
		return func(ctx context.Context, px int) (io.ReadCloser, int64, error) {
			return h.art.Cover(ctx, username, dir, px)
		}, nil
	}
}

// coverSize reads the size a client asked for. Nonsense falls back to the
// default rather than refusing, for the reason every other size parameter does:
// the specification makes it optional, so a client may leave it out and a
// misspelling should not cost it the picture.
func coverSize(r *http.Request) int {
	return intParam(r.URL.Query(), "size", defaultCoverSize)
}
