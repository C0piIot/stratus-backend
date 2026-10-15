package dav

import (
	"context"
	"net/http"
)

// Carrying a PUT's declared length past the library.
//
// go-webdav hands the backend a body and a pair of conditions, and nothing
// else about the request: the Content-Length a client sent is read by the
// standard library and then dropped. The backend was passing -1 for every
// PUT because of it, which the storage port takes for "unknown" -- and on S3
// an unknown length is an upload the client has to buffer a part of, where a
// known one under a part is a single request with no buffer at all (#348).
//
// So the length is read here, before the library sees the request, the way
// depth.go reads the Depth header and locks.go the If header, and put where
// Create can reach it: the context is the one thing the library passes
// through untouched.

type contentLengthKey struct{}

// withContentLength puts a PUT's declared length on the request's context.
// The standard library reports -1 for a chunked body, which is exactly what
// the port means by it, and 0 for a body that is declared empty.
func withContentLength(r *http.Request) *http.Request {
	if r.Method != http.MethodPut {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), contentLengthKey{}, r.ContentLength))
}

// contentLength is the declared length of the body Create was handed, or -1
// when the request did not say.
func contentLength(ctx context.Context) int64 {
	if n, ok := ctx.Value(contentLengthKey{}).(int64); ok {
		return n
	}
	return -1
}
