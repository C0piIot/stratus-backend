package stream

import (
	"net/http"
	"strconv"
	"time"
)

// What a transcode is told to a client as, for the two surfaces that serve
// one: OpenSubsonic's stream and a file's own URL with ?format= (#433).
//
// **A transcode cannot be ranged.** Its length is not known until it is over,
// so it goes out with Accept-Ranges: none and no Content-Length -- unless the
// client asked for an estimate, which is the bitrate times what is left: the
// body is cut to it if it runs long and ends early if it runs short, which is
// the price of a number the client asked to be told in advance.

// Headers sets what a response carrying p is, with length -1 for unknown.
func (p Plan) Headers(h http.Header, length int64) {
	h.Set("Content-Type", p.MIME)
	h.Set("Accept-Ranges", "none")
	if length >= 0 {
		h.Set("Content-Length", strconv.FormatInt(length, 10))
	}
}

// EstimatedLength is the bitrate times what is left of the track, or -1 when
// either is unknown -- a lossless target has no bitrate to multiply.
func (p Plan) EstimatedLength(durationMS int64, offset time.Duration) int64 {
	left := durationMS - offset.Milliseconds()
	if p.Bitrate <= 0 || left <= 0 {
		return -1
	}
	return int64(p.Bitrate) / 8 * left / 1000
}
