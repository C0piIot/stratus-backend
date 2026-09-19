package dav

import (
	"net/http"
)

// Refusing a PROPFIND that asks for the whole tree at once.
//
// RFC 4918 9.1 says a server MAY reject Depth: infinity "due to the performance
// and security concerns", and that when it does it MUST answer 403 with the
// DAV:propfind-finite-depth precondition -- which is a thing a client can act
// on, unlike a timeout.
//
// This one does, and it is measured: a hundred thousand files in a thousand
// folders answered in 44 MB of XML built inside 300 MB of heap, none of which
// could go out before all of it existed, because the multistatus is a value the
// library marshals in one go. It is linear, so a million files is three
// gigabytes on a server that is meant to fit on a phone (#160).
//
// The client's way through is Depth: 1 per collection, which is what almost all
// of them do already, and which now costs an indexed read rather than a scan.
// Streaming the multistatus is the answer that would let us say yes, and it is
// not ours to write: go-webdav marshals the whole value and its FileSystem
// hands back a slice. The day a real client needs infinity is the day to take
// that up with the library.
//
// A header this package reads for itself, because the library does not pass it
// on: the backend is given a bool and by then the answer is already being
// built.

// finiteDepth is the body of that refusal, spelled the way RFC 4918 14.5 does.
const finiteDepth = xmlHeader + `<D:error xmlns:D="DAV:"><D:propfind-finite-depth/></D:error>` + "\n"

const xmlHeader = `<?xml version="1.0" encoding="utf-8"?>` + "\n"

// refuseInfiniteDepth answers a PROPFIND that would walk the whole tree, and
// reports whether it did.
//
// An absent Depth header is infinity: RFC 4918 9.1 says so, and go-webdav
// defaults to it, so a request that leaves it out is refused as loudly as one
// that spells it. That is the point of answering with a precondition rather
// than with less than was asked for -- a client that meant the whole tree finds
// out, instead of quietly receiving one level and believing it.
func refuseInfiniteDepth(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != "PROPFIND" {
		return false
	}
	switch r.Header.Get("Depth") {
	case "0", "1":
		return false
	}

	w.Header().Set("Content-Type", `application/xml; charset="utf-8"`)
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(finiteDepth))
	return true
}
