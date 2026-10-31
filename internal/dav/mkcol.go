package dav

import "net/http"

// A MKCOL with a type and no body.
//
// go-webdav refuses any MKCOL that carries a Content-Type with 415, reading
// the header as if it were the body. RFC 4918 9.3 has the 415 for a body the
// server does not understand, and an empty one is not a body of any type.
// KDE's KIO sends `Content-Type: text/xml` on every WebDAV request, so every
// folder Dolphin made over a mount came back as "The server does not support
// the request type of the body" -- and so would any client that writes the
// header out of habit.
//
// So the header comes off before the library sees a MKCOL that declares no
// body. One that declares a body keeps it, and the library's refusal with
// it: an extended MKCOL is RFC 5689, which this collection does not speak.

// withoutEmptyBodyType is r without its Content-Type when it is a MKCOL whose
// body is declared empty. A chunked body is -1 and keeps its header, since
// whether it is empty is only known by reading it.
func withoutEmptyBodyType(r *http.Request) *http.Request {
	if r.Method != "MKCOL" || r.ContentLength != 0 || r.Header.Get("Content-Type") == "" {
		return r
	}
	r = r.Clone(r.Context())
	r.Header.Del("Content-Type")
	return r
}
