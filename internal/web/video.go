package web

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/media"
)

// Films: a player page, and the HLS a film is remuxed into when the player,
// a Chromecast or a television cannot take it as it is (#50).
//
// **HLS lives at the file's own URL**, with ?hls= naming the playlist or a
// segment, behind the same gate as the file: a session, or the signature of a
// share link. That is what lets the app cast a film -- a Chromecast fetches
// the playlist and every segment itself and can send no credentials, so the
// playlist writes the signature onto each segment's address -- and it adds no
// surface: HLS is a protocol every player there is already speaks.
//
// **The player is a page with a video element, and it works without script.**
// Its src is the file itself, which a browser plays when it can. When the
// film needs HLS to play -- a Matroska file, AC-3 sound -- two scripts are
// added: hls.js, vendored like htmx, and this project's own play.js, which
// hands it the playlist. Safari takes HLS natively and never loads hls.js at
// all.

const (
	// playParam asks for the player page instead of the file.
	playParam = "play"
	// hlsParam names the playlist or a segment.
	hlsParam = "hls"
	// playlistName is what a player is given: the one playlist when there is
	// one way to stream a film, the master over both when there are two.
	// copyName and encodedName are the two media playlists behind it.
	playlistName = "index.m3u8"
	copyName     = "copy.m3u8"
	encodedName  = "h264.m3u8"
)

// Video is what films need from the rest of the process: the media row that
// says what a film is, and the transcoder that remuxes its segments. The zero
// value offers no player and no HLS.
type Video struct {
	Media interface {
		MediaByFile(ctx context.Context, fileID int64) (db.Media, error)
	}
	Segments interface {
		Segment(ctx context.Context, f db.File, r media.Remux, s media.Segment) (io.ReadCloser, error)
	}
	// Encoded re-encodes a film to H.264, and is nil where this machine is
	// not to (STRATUS_VIDEO_TRANSCODE): then only the remux is offered.
	Encoded interface {
		Segment(ctx context.Context, f db.File, e media.Encode, s media.Segment) (io.ReadCloser, error)
	}
}

// filmView is the player page.
type filmView struct {
	// Direct is the file itself, which the video element plays when the
	// browser can, and HLS the playlist, empty when the film does not need it
	// or cannot have it.
	Direct, HLS string
}

// playerPolicy is the pages' policy plus what a video element needs: media
// from this origin, and blob: for the MediaSource hls.js feeds it through.
// hls.js is started with its worker off, so no worker-src is needed.
const playerPolicy = contentSecurityPolicy + "; media-src 'self' blob:"

// file serves a file, its player or its HLS, by what the query asks for.
func (h *handler) file(w http.ResponseWriter, r *http.Request, user string, f db.File) {
	q := r.URL.Query()
	switch {
	case h.video.Media == nil || !media.IsVideo(f.Path):
		h.download(w, r, user, f)
	case q.Has(hlsParam):
		h.hls(w, r, f, q.Get(hlsParam))
	case q.Has(playParam):
		h.play(w, r, user, f)
	default:
		h.download(w, r, user, f)
	}
}

func (h *handler) play(w http.ResponseWriter, r *http.Request, user string, f db.File) {
	token := r.URL.Query().Get(shareParam)
	film := &filmView{Direct: shared(href(f.Path), token)}

	// A film the browser takes as it is needs nothing more; one it does not,
	// and that can be remuxed, gets the playlist. A row nobody has read yet
	// is neither, and the file itself is offered.
	if m, err := h.video.Media.MediaByFile(r.Context(), f.ID); err == nil && !media.PlaysInBrowser(f, m) {
		_, remuxErr := media.RemuxFor(m)
		if _, ok := h.encoding(m); remuxErr == nil || ok {
			film.HLS = shared(href(f.Path)+"?"+hlsParam+"="+playlistName, token)
		}
	}

	v := view{Title: path.Base(f.Path), User: user, Shared: token, Name: path.Base(f.Path), Film: film,
		Back: shared(href(db.ParentOf(f.Path)), token)}
	if film.HLS != "" {
		v.PlayerScripts = []string{
			hlsPrefix + "/hls.light.min.js",
			ownPrefix + "/play.js?v=" + url.QueryEscape(h.version),
		}
	}
	w.Header().Set("Content-Security-Policy", playerPolicy)
	h.render(w, http.StatusOK, pagePlay, v)
}

// encoding is a film's re-encode, when this machine makes them and the film
// both needs one and can have one.
func (h *handler) encoding(m db.Media) (media.Encode, bool) {
	if h.video.Encoded == nil || !media.NeedsEncode(m) {
		return media.Encode{}, false
	}
	e, err := media.EncodeFor(m)
	return e, err == nil
}

// hls answers a playlist or one segment. Its errors are plain statuses: what
// reads them is a player, not a person.
func (h *handler) hls(w http.ResponseWriter, r *http.Request, f db.File, which string) {
	// Readable from any origin: a Chromecast's receiver is a page on Google's,
	// and it will not play what it may not read. The gate is the signature in
	// the URL, which a cross-origin page cannot forge, and no cookie is ever
	// honoured cross-origin -- a wildcard does not allow credentials.
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "private, no-cache")

	m, err := h.video.Media.MediaByFile(r.Context(), f.ID)
	if err != nil {
		http.Error(w, "this film has not been read yet", http.StatusNotFound)
		return
	}
	remux, remuxErr := media.RemuxFor(m)
	enc, encodes := h.encoding(m)
	token := r.URL.Query().Get(shareParam)
	uri := func(name string) string { return shared("?"+hlsParam+"="+url.QueryEscape(name), token) }

	switch {
	case which == playlistName && remuxErr == nil && encodes:
		// Both, for the player to choose between by the codecs each declares.
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		_, _ = io.WriteString(w, media.MasterPlaylist([]media.Variant{
			media.RemuxVariant(m, uri(copyName)), media.EncodeVariant(m, enc, uri(encodedName)),
		}))
	case (which == playlistName || which == copyName) && remuxErr == nil:
		h.playlist(w, r, f, uri)
	case (which == playlistName || which == encodedName) && encodes:
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		_, _ = io.WriteString(w, media.EncodedPlaylist(media.EncodedSegments(time.Duration(m.DurationMS)*time.Millisecond), uri))
	case which == playlistName || which == copyName || which == encodedName:
		http.Error(w, "this film cannot be streamed here", http.StatusNotFound)
	case strings.HasPrefix(which, "h") && encodes:
		seg, err := media.ParseEncoded(which)
		if err != nil {
			http.Error(w, "that is not a segment of this film", http.StatusBadRequest)
			return
		}
		out, err := h.video.Encoded.Segment(r.Context(), f, enc, seg)
		h.segment(w, r, f, which, out, err)
	case remuxErr == nil:
		seg, err := media.ParseSegment(which)
		if err != nil {
			http.Error(w, "that is not a segment of this film", http.StatusBadRequest)
			return
		}
		out, err := h.video.Segments.Segment(r.Context(), f, remux, seg)
		h.segment(w, r, f, which, out, err)
	default:
		http.Error(w, "this film cannot be streamed here", http.StatusNotFound)
	}
}

// segment sends what a remux or a re-encode produced, or says why it did not.
func (h *handler) segment(w http.ResponseWriter, r *http.Request, f db.File, which string, out io.ReadCloser, err error) {
	switch {
	case errors.Is(err, media.ErrBusy):
		w.Header().Set("Retry-After", "2")
		http.Error(w, "every transcode this server allows is running", http.StatusServiceUnavailable)
		return
	case err != nil:
		slog.ErrorContext(r.Context(), "web: cannot make a segment", "path", f.Path, "segment", which, "err", err)
		http.Error(w, "the segment could not be made", http.StatusInternalServerError)
		return
	}
	defer func() { _ = out.Close() }()
	w.Header().Set("Content-Type", "video/mp2t")
	_, _ = io.Copy(w, out)
}

func (h *handler) playlist(w http.ResponseWriter, r *http.Request, f db.File, uri func(string) string) {
	body, err := h.files.OpenFile(r.Context(), f)
	if err != nil {
		http.Error(w, "the film could not be opened", http.StatusInternalServerError)
		return
	}
	defer func() { _ = body.Close() }()
	ix, err := media.KeyframesOf(body, f)
	segments := media.Segments(ix)
	if err != nil || len(segments) == 0 {
		http.Error(w, "this film does not say where its keyframes are", http.StatusNotFound)
		return
	}
	// Relative addresses, each carrying the signature the playlist was
	// fetched with: a player resolves them against the playlist's own URL,
	// and a Chromecast has no other way to be let in.
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	_, _ = io.WriteString(w, media.Playlist(segments, uri))
}
