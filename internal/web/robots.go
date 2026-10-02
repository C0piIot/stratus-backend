package web

import (
	"io"
	"net/http"
)

// robotsTxt asks every crawler to stay out of all of it.
//
// This is one person's files, and the one surface that would otherwise be worth
// indexing is the one that must not be: a share link's only protection is that
// nobody else has the URL, and an indexer that found one would publish it. The
// login page is nothing to find either.
//
// It is a request and not a control -- a crawler that ignores it is not stopped
// by anything here -- which is why the pages carry the same thing as a meta
// element: this file says do not crawl, the element says do not index what you
// crawled anyway.
const robotsTxt = "User-agent: *\nDisallow: /\n"

// robots answers it, in front of the session rather than behind it: a crawler
// has no credentials, and a robots.txt that asked for a password would be one
// nothing ever reads.
func robots(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	// A day. It is the same four words on every request and a crawler asks for
	// it before every visit.
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = io.WriteString(w, robotsTxt)
}
