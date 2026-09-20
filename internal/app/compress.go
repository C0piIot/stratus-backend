package app

import (
	"compress/gzip"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// Compressing what is worth compressing.
//
// A PROPFIND of two hundred files is 138 KB of XML and the same forty tags
// over and over; gzip takes it to 2.9 KB, which is 48 times less for 0.84 ms
// of CPU. The named-property listing the app asks for goes 84.6 KB to 1.3 KB.
// Nothing else this server could do to a listing comes close -- #178 was filed
// about the repeated namespace declaration in it, which is 2% of the document
// and compresses to nothing at all.
//
// It is here rather than in front because there is no in front: principle 1
// says one binary and no separate web server, so the thing that would normally
// do this -- nginx, Caddy, a CDN -- does not exist in this deployment. A
// self-hosted server on somebody's uplink is exactly where the bytes matter.
//
// **Only what compresses, and never a blob.** A photograph and a video are
// already compressed and gzip would spend CPU to make them slightly larger, so
// the gate is the content type rather than the route, which is also what keeps
// a surface added later covered without anybody remembering to.

// compress wraps h so that a client which asked for gzip gets it.
func compress(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A HEAD has no body to save and its Content-Length is the one thing
		// it carries, so it is left exactly as it was.
		if r.Method == http.MethodHead || !acceptsGzip(r.Header.Get("Accept-Encoding")) {
			h.ServeHTTP(w, r)
			return
		}

		// Said before deciding anything, and said even when the answer turns
		// out not to be compressed: a cache keying on the URL alone would
		// otherwise hand the gzipped body to a client that never asked for
		// one.
		w.Header().Add("Vary", "Accept-Encoding")

		cw := &compressingWriter{ResponseWriter: w}
		defer cw.close()
		h.ServeHTTP(cw, r)
	})
}

// acceptsGzip reports whether the client offered it.
//
// The whole of RFC 9110 12.5.3 is not needed here: there is one encoding this
// server can produce, so the question is whether the client listed it and did
// not then say q=0 -- which is how a client asks for something to be left
// alone, and the only reason this looks at the parameters at all.
func acceptsGzip(header string) bool {
	for offer := range strings.SplitSeq(header, ",") {
		name, params, _ := strings.Cut(offer, ";")
		// Case-insensitive, because RFC 9110 7.6.1 says a content coding is.
		if !strings.EqualFold(strings.TrimSpace(name), "gzip") {
			continue
		}
		for param := range strings.SplitSeq(params, ";") {
			key, value, found := strings.Cut(param, "=")
			if !found || strings.TrimSpace(key) != "q" {
				continue
			}
			quality, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
			return err != nil || quality > 0
		}
		return true
	}
	return false
}

// minimumWorth is the size below which compressing costs more than it saves:
// a gzip stream carries about 20 bytes of framing, and a short document full
// of unique bytes comes out longer than it went in. Only applied when the
// handler declared a length -- a streamed response has not, and it is the
// large ones that stream.
const minimumWorth = 1024

// compressingWriter decides, once, whether this response is one to compress,
// and from then on is either a gzip stream or the writer it wraps.
type compressingWriter struct {
	http.ResponseWriter
	gz      *gzip.Writer
	decided bool
}

func (c *compressingWriter) WriteHeader(status int) {
	c.decide(status)
	c.ResponseWriter.WriteHeader(status)
}

func (c *compressingWriter) Write(b []byte) (int, error) {
	// A handler that never called WriteHeader is answering 200.
	c.decide(http.StatusOK)
	if c.gz != nil {
		return c.gz.Write(b)
	}
	return c.ResponseWriter.Write(b)
}

// decide is the whole policy, and every clause in it is a way to break a
// response rather than a preference.
func (c *compressingWriter) decide(status int) {
	if c.decided {
		return
	}
	c.decided = true

	header := c.Header()
	switch {
	// A 206 is a slice of a representation and its Content-Range counts bytes
	// of the original, so compressing one describes the answer wrongly. 204
	// and 304 have no body to compress at all.
	case status == http.StatusPartialContent || status == http.StatusNoContent ||
		status == http.StatusNotModified || header.Get("Content-Range") != "":
		return
	// Somebody already encoded this. There is exactly one Content-Encoding and
	// it is not ours to take.
	case header.Get("Content-Encoding") != "":
		return
	case !compressible(header.Get("Content-Type")):
		return
	case tooSmall(header.Get("Content-Length")):
		return
	}

	header.Set("Content-Encoding", "gzip")
	// The length of what the handler was about to write, which is not the
	// length of what goes out. Removing it is what makes the response chunked.
	header.Del("Content-Length")
	// The ETag names a representation and this is a different one. Weakened
	// rather than removed, so a conditional request still works and simply
	// compares the way a weak validator does -- the same thing Apache and
	// nginx do, and it matters here because a downloaded file carries a strong
	// ETag and may well be text.
	if etag := header.Get("ETag"); etag != "" && !strings.HasPrefix(etag, "W/") {
		header.Set("ETag", "W/"+etag)
	}

	c.gz = writers.Get().(*gzip.Writer)
	c.gz.Reset(c.ResponseWriter)
}

// close finishes the gzip stream. Nothing useful can be done about a failure:
// the status and most of the body have already gone out.
func (c *compressingWriter) close() {
	if c.gz == nil {
		return
	}
	_ = c.gz.Close()
	writers.Put(c.gz)
	c.gz = nil
}

// FlushError puts what gzip is holding into the response before flushing it.
//
// Without this, http.ResponseController would find Unwrap below, flush the
// writer underneath and leave the compressed bytes sitting in a buffer -- a
// streaming response that never arrives. ResponseController looks for this
// method on the outermost writer first, which is why it is the one that has
// to exist.
func (c *compressingWriter) FlushError() error {
	if c.gz != nil {
		if err := c.gz.Flush(); err != nil {
			return err
		}
	}
	return http.NewResponseController(c.ResponseWriter).Flush()
}

// Unwrap keeps everything else a ResponseController can do working through
// this wrapper, the same way the log's recorder does.
func (c *compressingWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

// writers are pooled because a gzip.Writer is a quarter of a megabyte of
// window and hash tables, and this is on the path of every page.
var writers = sync.Pool{
	New: func() any { return gzip.NewWriter(nil) },
}

// compressible reports whether a content type is worth the CPU.
//
// Text of any kind is, and so are the three application types this server
// answers with. Everything else is assumed to be a blob: what the file
// surfaces hand over is a photograph, a video or a track, all of them already
// compressed and all of them large enough that trying would be the most
// expensive thing the request does.
func compressible(contentType string) bool {
	media, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	if strings.HasPrefix(media, "text/") {
		return true
	}
	switch media {
	case "application/xml", "application/json", "application/javascript":
		return true
	}
	return false
}

func tooSmall(contentLength string) bool {
	if contentLength == "" {
		return false
	}
	length, err := strconv.Atoi(contentLength)
	return err == nil && length < minimumWorth
}
