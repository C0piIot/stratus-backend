package web

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/stratus-private-cloud/stratus-backend/internal/db"
	"github.com/stratus-private-cloud/stratus-backend/internal/media"
	"github.com/stratus-private-cloud/stratus-backend/internal/media/stream"
)

// A track transcoded at its own URL (#433): /files/<path>?format=mp3 is the
// same file in another format, decided exactly as OpenSubsonic's stream
// decides it, under the same parameter names. It is what a client casting to
// a screen that cannot decode the original asks for -- a DLNA renderer that
// lists MP3 and not Opus -- and it is HLS's twin: a query on the file's own
// address, behind the same gate, so a share link's signature covers it and
// nothing new is a surface.
//
// **What cannot be done answers the original**: a format nobody transcodes
// to, the one the file already is, a lossless copy of a lossy file, a track
// nothing has read yet. A client knows which it got from the Content-Type,
// which is how it already tells HLS from a server that ignored the query.
//
// **What could be done and cannot right now is a 503**, unlike stream's
// fallback to the original. That fallback exists because a Subsonic player
// does not read the XML an error would be; here the client asked for another
// format because the original is no use to it, so sending the original would
// be the same failure without the status that says it.

// transcoded serves a track as ?format= asks for it.
func (h *handler) transcoded(w http.ResponseWriter, r *http.Request, user string, f db.File) {
	m, err := h.playback.Media.MediaByFile(r.Context(), f.ID)
	switch {
	case errors.Is(err, db.ErrNotFound) || (err == nil && h.playback.Transcode == nil):
		h.download(w, r, user, f)
		return
	case err != nil:
		slog.ErrorContext(r.Context(), "web: cannot read what a track is", "path", f.Path, "err", err)
		http.Error(w, "the track could not be read", http.StatusInternalServerError)
		return
	}

	q := r.URL.Query()
	maxBitrate, _ := strconv.Atoi(q.Get(bitrateParam))
	plan := stream.Decide(m, stream.Request{Format: q.Get(formatParam), MaxBitrate: maxBitrate * 1000})
	if plan.Direct {
		h.download(w, r, user, f)
		return
	}
	length := int64(-1)
	if q.Get(estimateParam) == "true" {
		length = plan.EstimatedLength(m.DurationMS, 0)
	}

	// Readable from any origin and never cached, for the reasons hls gives:
	// what fetches this may be a receiver on somebody else's page.
	header := w.Header()
	header.Set("Access-Control-Allow-Origin", "*")
	header.Set("Cache-Control", "private, no-cache")
	header.Set("Content-Security-Policy", filePolicy(plan.MIME))
	if r.Method == http.MethodHead {
		// What a client checks before it hands the URL on, so it starts
		// nothing.
		plan.Headers(header, length)
		return
	}

	out, err := h.playback.Transcode.Transcode(r.Context(), f, plan, 0)
	switch {
	case errors.Is(err, media.ErrBusy):
		header.Set("Retry-After", "2")
		http.Error(w, "every transcode this server allows is running", http.StatusServiceUnavailable)
		return
	case err != nil:
		slog.ErrorContext(r.Context(), "web: cannot transcode a track", "path", f.Path, "format", plan.Format, "err", err)
		http.Error(w, "the track could not be transcoded", http.StatusInternalServerError)
		return
	}
	defer func() { _ = out.Close() }()

	plan.Headers(header, length)
	if length >= 0 {
		_, _ = io.CopyN(w, out, length)
		return
	}
	_, _ = io.Copy(w, out)
}
