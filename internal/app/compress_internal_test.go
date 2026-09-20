package app

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// xml is a document big enough to be worth compressing and repetitive enough
// to show what the real thing does: a multistatus is the same forty tags over
// and over.
func xmlBody(entries int) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><D:multistatus xmlns:D="DAV:">`)
	for i := range entries {
		b.WriteString(`<D:response><D:href>/dav/photo-` + strconv.Itoa(i) +
			`.jpg</D:href><D:propstat><D:prop><D:getcontentlength>8</D:getcontentlength>` +
			`</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`)
	}
	b.WriteString(`</D:multistatus>`)
	return b.String()
}

// answering serves one fixed response through the middleware and hands back
// what a client would receive.
func answering(t *testing.T, accept string, header map[string]string, status int, body string) *http.Response {
	t.Helper()
	h := compress(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for name, value := range header {
			w.Header().Set(name, value)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/dav/album/", nil)
	if accept != "" {
		req.Header.Set("Accept-Encoding", accept)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Result()
}

func TestAListingIsCompressed(t *testing.T) {
	t.Parallel()
	body := xmlBody(200)
	res := answering(t, "gzip", map[string]string{"Content-Type": "text/xml; charset=utf-8"},
		http.StatusMultiStatus, body)
	defer func() { _ = res.Body.Close() }()

	if got := res.Header.Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if got := res.Header.Get("Vary"); !strings.Contains(got, "Accept-Encoding") {
		t.Errorf("Vary = %q, want Accept-Encoding: a cache would serve this to somebody who did not ask", got)
	}
	// The length of what the handler wrote is not the length of what goes out.
	if got := res.Header.Get("Content-Length"); got != "" {
		t.Errorf("Content-Length = %q, want it gone", got)
	}

	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	unzipped, err := gzip.NewReader(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("what came back is not gzip: %v", err)
	}
	same, err := io.ReadAll(unzipped)
	if err != nil {
		t.Fatal(err)
	}
	if string(same) != body {
		t.Error("the document did not survive the round trip")
	}
	// The point of the whole file. A multistatus is the most compressible
	// thing this server produces and the number is not marginal.
	if len(raw)*10 > len(body) {
		t.Errorf("%d bytes from %d, which is less than ten times", len(raw), len(body))
	}
}

// TestABlobIsNeverCompressed: a photograph is compressed already, and gzip
// would spend the CPU to make it slightly larger. The gate is the content type
// rather than the route, so a surface added later is covered without anybody
// remembering to.
func TestABlobIsNeverCompressed(t *testing.T) {
	t.Parallel()
	for _, contentType := range []string{"image/jpeg", "video/mp4", "audio/flac", "application/octet-stream"} {
		res := answering(t, "gzip", map[string]string{"Content-Type": contentType},
			http.StatusOK, strings.Repeat("x", 4096))
		_ = res.Body.Close()
		if got := res.Header.Get("Content-Encoding"); got != "" {
			t.Errorf("%s was answered with Content-Encoding %q", contentType, got)
		}
	}
}

// TestARangeIsNeverCompressed is the one that would corrupt a download rather
// than merely waste time: Content-Range counts bytes of the original
// representation, so a compressed 206 describes itself wrongly. Video seeking
// over /dav/ and /files/ is made of these.
func TestARangeIsNeverCompressed(t *testing.T) {
	t.Parallel()
	res := answering(t, "gzip", map[string]string{
		"Content-Type":  "text/plain; charset=utf-8",
		"Content-Range": "bytes 0-1023/1000000",
	}, http.StatusPartialContent, strings.Repeat("a", 1024))
	defer func() { _ = res.Body.Close() }()

	if got := res.Header.Get("Content-Encoding"); got != "" {
		t.Errorf("a 206 was compressed: Content-Encoding = %q", got)
	}
}

// TestWhatIsAlreadyEncodedIsLeftAlone: there is one Content-Encoding and it is
// not ours to take.
func TestWhatIsAlreadyEncodedIsLeftAlone(t *testing.T) {
	t.Parallel()
	res := answering(t, "gzip", map[string]string{
		"Content-Type":     "application/json",
		"Content-Encoding": "br",
	}, http.StatusOK, xmlBody(50))
	defer func() { _ = res.Body.Close() }()

	if got := res.Header.Get("Content-Encoding"); got != "br" {
		t.Errorf("Content-Encoding = %q, want the one that was already there", got)
	}
}

// TestASmallAnswerIsNotWorthIt: a gzip stream carries about twenty bytes of
// framing, so a short document comes out longer than it went in.
func TestASmallAnswerIsNotWorthIt(t *testing.T) {
	t.Parallel()
	const short = "ok\n"
	res := answering(t, "gzip", map[string]string{
		"Content-Type":   "text/plain; charset=utf-8",
		"Content-Length": strconv.Itoa(len(short)),
	}, http.StatusOK, short)
	defer func() { _ = res.Body.Close() }()

	if got := res.Header.Get("Content-Encoding"); got != "" {
		t.Errorf("a three-byte answer was compressed")
	}
	// And a streamed one, which declares no length, still is: the large ones
	// are exactly the ones that cannot say how long they are.
	streamed := answering(t, "gzip", map[string]string{"Content-Type": "text/xml"},
		http.StatusMultiStatus, xmlBody(200))
	defer func() { _ = streamed.Body.Close() }()
	if got := streamed.Header.Get("Content-Encoding"); got != "gzip" {
		t.Errorf("a response with no declared length = %q, want gzip", got)
	}
}

// TestNobodyGetsWhatTheyDidNotAskFor, which is the whole contract of content
// negotiation -- a client that cannot decode gzip includes no such header.
func TestNobodyGetsWhatTheyDidNotAskFor(t *testing.T) {
	t.Parallel()
	for _, accept := range []string{"", "identity", "br", "gzip;q=0", "gzip; q=0.0"} {
		res := answering(t, accept, map[string]string{"Content-Type": "text/xml"},
			http.StatusOK, xmlBody(100))
		_ = res.Body.Close()
		if got := res.Header.Get("Content-Encoding"); got != "" {
			t.Errorf("Accept-Encoding %q was answered with %q", accept, got)
		}
	}
	// The last is RFC 9110 7.6.1: a content coding is case-insensitive, and a
	// client that shouts is still a client that can decode.
	for _, accept := range []string{"gzip", "gzip, deflate", "deflate, gzip;q=1.0, *;q=0.5", "GZIP"} {
		res := answering(t, accept, map[string]string{"Content-Type": "text/xml"},
			http.StatusOK, xmlBody(100))
		_ = res.Body.Close()
		if got := res.Header.Get("Content-Encoding"); got != "gzip" {
			t.Errorf("Accept-Encoding %q was answered with %q, want gzip", accept, got)
		}
	}
}

// TestAHeadIsLeftAlone: there is no body to save, and its Content-Length is
// the one thing a HEAD carries.
func TestAHeadIsLeftAlone(t *testing.T) {
	t.Parallel()
	h := compress(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/xml")
		w.Header().Set("Content-Length", "138822")
	}))
	req := httptest.NewRequestWithContext(t.Context(), http.MethodHead, "/dav/album/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("a HEAD was answered with Content-Encoding %q", got)
	}
	if got := rec.Header().Get("Content-Length"); got != "138822" {
		t.Errorf("Content-Length = %q, want the one the handler set", got)
	}
}

// TestACompressedAnswerHasAWeakETag. The ETag names a representation and this
// is a different one; weakened rather than dropped, so a conditional request
// still works. It matters because a downloaded file carries a strong ETag and
// may well be text.
func TestACompressedAnswerHasAWeakETag(t *testing.T) {
	t.Parallel()
	res := answering(t, "gzip", map[string]string{
		"Content-Type": "text/plain; charset=utf-8",
		"ETag":         `"abc123"`,
	}, http.StatusOK, xmlBody(100))
	defer func() { _ = res.Body.Close() }()

	if got := res.Header.Get("ETag"); got != `W/"abc123"` {
		t.Errorf("ETag = %q, want it weakened", got)
	}
	// And an uncompressed answer keeps the strong one, which is what every
	// other surface compares against.
	plain := answering(t, "identity", map[string]string{
		"Content-Type": "text/plain; charset=utf-8",
		"ETag":         `"abc123"`,
	}, http.StatusOK, xmlBody(100))
	defer func() { _ = plain.Body.Close() }()
	if got := plain.Header.Get("ETag"); got != `"abc123"` {
		t.Errorf("ETag = %q, want it untouched", got)
	}
}

// TestAFlushReachesTheClient: the listing streams, and a ResponseController
// that flushed the writer underneath would leave the compressed bytes in a
// buffer -- a response that never arrives, with nothing to see on the server.
func TestAFlushReachesTheClient(t *testing.T) {
	t.Parallel()
	body := xmlBody(100)
	h := compress(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/xml")
		_, _ = io.WriteString(w, body)
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("flush: %v", err)
		}
	}))
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/dav/album/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !rec.Flushed {
		t.Error("the response was never flushed")
	}
	if rec.Body.Len() == 0 {
		t.Fatal("nothing was written")
	}
	unzipped, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("what was flushed is not gzip: %v", err)
	}
	if got, _ := io.ReadAll(unzipped); string(got) != body {
		t.Error("the flushed document is not the one that was written")
	}
}
