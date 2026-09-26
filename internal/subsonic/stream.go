package subsonic

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path"
	"strconv"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/media"
)

// Transcoder is what stream needs to turn a track into what a client asked for:
// internal/media's, which starts ffmpeg over the file and hands back what it
// writes.
type Transcoder interface {
	Transcode(ctx context.Context, f db.File, p media.Plan, offset time.Duration) (io.ReadCloser, error)
}

// stream serves a track as it is stored or as the client asked for it (#50);
// download always as it is stored, since that is what it means.
//
// **What is sent is internal/media's decision**, from format, maxBitRate and
// the facts the indexer read: the original whenever it will do, format=raw
// always, and a transcode that never claims more than the file has. A phone on
// mobile data asking for format=mp3&maxBitRate=128 gets 128 kbps MP3; the same
// phone at home asking for nothing gets the FLAC.
//
// **A transcode cannot be ranged.** Its length is not known until it is over,
// so it goes out with Accept-Ranges: none and no Content-Length, and a client
// seeks inside it with timeOffset instead -- which is the transcodeOffset
// extension, advertised for exactly that. estimateContentLength asks for a
// length anyway, from the bitrate and what is left of the duration: the body
// is cut to it if it runs long and ends early if it runs short, which is the
// price of a number the client asked to be told in advance.
//
// **When every transcode slot is taken, the original goes out instead.** A
// client that asked for a smaller file gets a larger one and plays it, where
// an error would have been silence -- and this endpoint's errors are XML a
// player does not read.
//
// **A stream is not a play.** The specification is explicit that play counts
// come from scrobble alone, so this counts nothing.
func (h *handler) stream(w http.ResponseWriter, r *http.Request, username string) {
	t, ok := h.track(w, r, username)
	if !ok {
		return
	}

	q := r.URL.Query()
	plan := media.Decide(t.Media, media.Request{
		Format:     q.Get("format"),
		MaxBitrate: intParam(q, "maxBitRate", 0) * 1000,
	})
	if plan.Direct || h.transcoder == nil {
		h.original(w, r, t, false)
		return
	}

	offset := time.Duration(intParam(q, "timeOffset", 0)) * time.Second
	length := int64(-1)
	if q.Get("estimateContentLength") == "true" {
		length = estimatedLength(plan, t.Media.DurationMS, offset)
	}

	header := w.Header()
	if r.Method == http.MethodHead {
		transcodeHeaders(header, plan, length)
		return
	}

	out, err := h.transcoder.Transcode(r.Context(), t.File, plan, offset)
	switch {
	case errors.Is(err, media.ErrBusy):
		slog.WarnContext(r.Context(), "subsonic: every transcode slot is taken, sending the original",
			"path", t.File.Path, "asked", plan.Format)
		h.original(w, r, t, false)
		return
	case err != nil:
		h.failXML(w, r, h.internal(r, "transcode a track", err))
		return
	}
	defer func() { _ = out.Close() }()

	transcodeHeaders(header, plan, length)
	if length >= 0 {
		_, _ = io.CopyN(w, out, length)
		return
	}
	_, _ = io.Copy(w, out)
}

func (h *handler) download(w http.ResponseWriter, r *http.Request, username string) {
	if t, ok := h.track(w, r, username); ok {
		h.original(w, r, t, true)
	}
}

// track finds the track a byte request names, and answers for it when there
// is none.
func (h *handler) track(w http.ResponseWriter, r *http.Request, username string) (db.Track, bool) {
	id, apiErr := requiredID(r)
	if apiErr != nil {
		h.failXML(w, r, *apiErr)
		return db.Track{}, false
	}
	t, apiErr := h.audioTrack(r, username, id)
	if apiErr != nil {
		h.failXML(w, r, *apiErr)
		return db.Track{}, false
	}
	return t, true
}

// original serves the file that was stored, unchanged.
//
// Ranges, conditional requests and seeking are http.ServeContent's, over the
// seekable reader internal/files returns. That is the same path WebDAV serves a
// video from, and the reason neither surface parses a Range header.
func (h *handler) original(w http.ResponseWriter, r *http.Request, t db.Track, asAttachment bool) {
	body, err := h.tree.OpenFile(r.Context(), t.File)
	if err != nil {
		h.failXML(w, r, h.internal(r, "open a track", err))
		return
	}
	defer func() { _ = body.Close() }()

	name := path.Base(t.File.Path)
	if asAttachment {
		w.Header().Set("Content-Disposition", attachment(name))
	}
	// Set rather than sniffed: ServeContent guesses from the extension and then
	// from the first bytes, and the type the file was uploaded with is better
	// evidence than either.
	if t.File.MIMEType != "" {
		w.Header().Set("Content-Type", t.File.MIMEType)
	}
	http.ServeContent(w, r, name, t.File.MTime, body)
}

func transcodeHeaders(header http.Header, plan media.Plan, length int64) {
	header.Set("Content-Type", plan.MIME)
	header.Set("Accept-Ranges", "none")
	if length >= 0 {
		header.Set("Content-Length", strconv.FormatInt(length, 10))
	}
}

// estimatedLength is the bitrate times what is left of the track, or -1 when
// either is unknown -- a lossless target has no bitrate to multiply.
func estimatedLength(plan media.Plan, durationMS int64, offset time.Duration) int64 {
	left := durationMS - offset.Milliseconds()
	if plan.Bitrate <= 0 || left <= 0 {
		return -1
	}
	return int64(plan.Bitrate) / 8 * left / 1000
}
