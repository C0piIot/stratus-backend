package dav

import (
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	xnet "golang.org/x/net/webdav"
)

// Locking, and it is real now (#174).
//
// macOS Finder refuses to mount a WebDAV share read-write unless the server
// says it is class 2, which means LOCK and UNLOCK. The library this package was
// built on is class 1 and answers 405 to both, so Finder mounts read-only or
// not at all (#3) -- and for a long time this file answered LOCK with a
// well-formed token that nothing ever checked. That was a lie, documented as
// one, on the argument that the real protection against a lost update is the
// strong ETag and If-Match.
//
// What this file predicted is exactly what it cost to stop lying: "the state is
// a table and the hard part is the If: header grammar in RFC 4918 section 10.4,
// not this file." Half right. The state is not a table -- x/net/webdav arrived
// for PROPFIND (#136) and brought a LockSystem, held for the process in
// dav.go. The If: header was the hard part, and it is vendored in
// ifheader.go with the enforcement in locks.go.
//
// What stays true: a lock lives in memory, so a restart drops every one of
// them. That is the same trade the signed session makes, and the ETag is still
// the defence that survives a restart.
const (
	// lockTimeout is how long a lock lasts without being refreshed. An hour is
	// long enough that a client editing a file does not lose it, and short
	// enough that a client which died holding one does not block the path for
	// a day.
	lockTimeout = time.Hour

	// maxLockBody bounds the request body. The owner element is arbitrary XML
	// echoed straight back, so it is the one place a client could hand us
	// something unbounded.
	maxLockBody = 64 << 10
)

// lockInfo is the request body of a LOCK.
type lockInfo struct {
	XMLName xml.Name `xml:"DAV: lockinfo"`
	// Shared is the other lock scope, which this server does not have: the
	// lock system holds one token per resource. It is read so that asking for
	// one can be refused rather than answered with an exclusive lock and a
	// response saying so -- which is a client being told it shares something
	// it does not.
	Shared *struct{} `xml:"lockscope>shared"`
	// Owner is arbitrary XML that the client expects to see again untouched, so
	// it travels as raw bytes rather than being interpreted.
	Owner ownerXML `xml:"owner"`
}

type ownerXML struct {
	Inner string `xml:",innerxml"`
}

// handleLock takes a lock, or refreshes one.
func (f *fileSystem) handleLock(w http.ResponseWriter, r *http.Request) {
	path, err := toPath(r.URL.Path)
	if err != nil {
		http.Error(w, "that is not a path here", http.StatusBadRequest)
		return
	}

	var info lockInfo
	body, err := io.ReadAll(io.LimitReader(r.Body, maxLockBody))
	if err != nil {
		http.Error(w, "cannot read the lock request", http.StatusBadRequest)
		return
	}
	// An empty body is a refresh of a lock the client already holds, named by
	// the If header rather than by the body.
	if len(body) == 0 {
		f.refreshLock(w, r)
		return
	}
	if uerr := xml.Unmarshal(body, &info); uerr != nil {
		http.Error(w, "malformed lock request", http.StatusBadRequest)
		return
	}

	// A shared lock is refused rather than granted as an exclusive one. What
	// PROPFIND advertises in supportedlock is exclusive alone, so this is the
	// same answer twice rather than a surprise.
	if info.Shared != nil {
		http.Error(w, "this server takes exclusive write locks only", http.StatusNotImplemented)
		return
	}

	// A lock on a path that is not there would have to create an empty
	// resource to hold it (RFC 4918 7.3). Refused: it means writing through a
	// filesystem built to refuse writes, and no client this server is for
	// needs it. Finder locks a file it is about to replace, which exists.
	owner, err := f.owner(r.Context())
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if _, serr := f.files.Stat(r.Context(), owner, path); serr != nil {
		http.Error(w, "there is nothing there to lock", http.StatusNotFound)
		return
	}

	token, err := f.locks.Create(f.now(), xnet.LockDetails{
		Root:      lockName(path),
		Duration:  lockTimeout,
		OwnerXML:  info.Owner.Inner,
		ZeroDepth: depthOf(r) == "0",
	})
	if err != nil {
		http.Error(w, "locked", lockStatus(err))
		return
	}

	w.Header().Set("Lock-Token", "<"+token+">")
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusOK)

	// The prefix is back on: the request path arrives stripped, and a lockroot
	// pointing at /notes.txt instead of /dav/notes.txt names a resource the
	// client cannot address. Same trap as the hrefs in a multistatus.
	_, _ = io.WriteString(w, lockDiscovery(token, f.prefix+r.URL.Path, depthOf(r), info.Owner.Inner))
}

// refreshLock is a LOCK with no body: the client is asking for more time on a
// lock it names in its If header.
//
// Answering it with a fresh token, which is what this did when nothing was
// stored, is what litmus calls out -- a client that asked to keep its lock is
// told about a different one and holds neither.
func (f *fileSystem) refreshLock(w http.ResponseWriter, r *http.Request) {
	parsed, ok := parseIfHeader(r.Header.Get("If"))
	if !ok || len(parsed.lists) == 0 || len(parsed.lists[0].conditions) == 0 {
		http.Error(w, "a refresh has to name its lock", http.StatusBadRequest)
		return
	}

	token := parsed.lists[0].conditions[0].Token
	details, err := f.locks.Refresh(f.now(), token, lockTimeout)
	if err != nil {
		http.Error(w, "no such lock", lockStatus(err))
		return
	}

	w.Header().Set("Lock-Token", "<"+token+">")
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	depth := "infinity"
	if details.ZeroDepth {
		depth = "0"
	}
	_, _ = io.WriteString(w, lockDiscovery(token, f.prefix+r.URL.Path, depth, details.OwnerXML))
}

// handleUnlock releases a lock, and can fail now: a token nobody holds is a
// client that believes something untrue, and saying so is more use than a 204
// it cannot learn from.
func (f *fileSystem) handleUnlock(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimSuffix(strings.TrimPrefix(r.Header.Get("Lock-Token"), "<"), ">")
	if token == "" {
		http.Error(w, "no lock token", http.StatusBadRequest)
		return
	}
	switch err := f.locks.Unlock(f.now(), token); {
	case err == nil:
	case errors.Is(err, xnet.ErrNoSuchLock):
		// RFC 4918 9.11.1: 409, and not the 412 the same error means when a
		// write was claiming to hold something. Here the request is the unlock
		// itself, and what it names is not there.
		http.Error(w, "no such lock", http.StatusConflict)
		return
	default:
		http.Error(w, "cannot unlock that", lockStatus(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// lockDiscovery is the body RFC 4918 section 9.10.1 asks for.
func lockDiscovery(token, href, depth, owner string) string {
	var b strings.Builder
	b.WriteString(xml.Header)
	b.WriteString(`<D:prop xmlns:D="DAV:"><D:lockdiscovery><D:activelock>`)
	b.WriteString(`<D:locktype><D:write/></D:locktype>`)
	b.WriteString(`<D:lockscope><D:exclusive/></D:lockscope>`)
	b.WriteString(`<D:depth>` + depth + `</D:depth>`)
	if owner != "" {
		b.WriteString(`<D:owner>` + owner + `</D:owner>`)
	}
	b.WriteString(`<D:timeout>Second-` + strconv.Itoa(int(lockTimeout.Seconds())) + `</D:timeout>`)
	b.WriteString(`<D:locktoken><D:href>` + token + `</D:href></D:locktoken>`)
	b.WriteString(`<D:lockroot><D:href>` + xmlEscape(href) + `</D:href></D:lockroot>`)
	b.WriteString(`</D:activelock></D:lockdiscovery></D:prop>`)
	return b.String()
}

func depthOf(r *http.Request) string {
	if d := r.Header.Get("Depth"); d == "0" {
		return "0"
	}
	return "infinity"
}

func xmlEscape(s string) string {
	var b strings.Builder
	if err := xml.EscapeText(&b, []byte(s)); err != nil {
		return ""
	}
	return b.String()
}

// advertiseLocking rewrites what the library says about itself on OPTIONS.
//
// The capability list is built inside the library from its own backend, which
// this package does not implement, so the header is corrected on the way out
// rather than by forking anything.
type advertiseLocking struct {
	http.ResponseWriter
	written bool
}

func (a *advertiseLocking) WriteHeader(status int) {
	if !a.written {
		a.written = true
		if dav := a.Header().Get("DAV"); dav != "" && !strings.Contains(dav, "2") {
			a.Header().Set("DAV", dav+", 2")
		}
		if allow := a.Header().Get("Allow"); allow != "" {
			a.Header().Set("Allow", allow+", LOCK, UNLOCK")
		}
	}
	a.ResponseWriter.WriteHeader(status)
}

func (a *advertiseLocking) Unwrap() http.ResponseWriter { return a.ResponseWriter }
