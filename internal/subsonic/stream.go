package subsonic

import (
	"net/http"
	"path"
)

// stream and download both serve the file that was stored, unchanged.
//
// **Nothing is transcoded.** maxBitRate, format and estimateContentLength are
// read by nobody here: the original is what format=raw asks for explicitly, and
// it is what every client on a local network should want anyway. #50 is the
// issue that changes that, and it also brings the transcodeOffset extension,
// without which seeking inside a transcoded track does not work.
//
// **A stream is not a play.** The specification is explicit that play counts
// come from scrobble alone, so this counts nothing -- which is easy here,
// because there is nowhere to count it.
//
// Ranges, conditional requests and seeking are http.ServeContent's, over the
// seekable reader internal/files returns. That is the same path WebDAV serves a
// video from, and the reason neither surface parses a Range header.
func (h *handler) stream(w http.ResponseWriter, r *http.Request, username string) {
	h.serve(w, r, username, false)
}

func (h *handler) download(w http.ResponseWriter, r *http.Request, username string) {
	h.serve(w, r, username, true)
}

func (h *handler) serve(w http.ResponseWriter, r *http.Request, username string, asAttachment bool) {
	id, apiErr := requiredID(r)
	if apiErr != nil {
		h.failXML(w, r, *apiErr)
		return
	}
	t, apiErr := h.audioTrack(r, username, id)
	if apiErr != nil {
		h.failXML(w, r, *apiErr)
		return
	}

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
