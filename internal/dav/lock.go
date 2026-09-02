package dav

import (
	"crypto/rand"
	"encoding/xml"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Locking here is advertised and not enforced, on purpose.
//
// macOS Finder refuses to mount a WebDAV share read-write unless the server
// says it is class 2, which means LOCK and UNLOCK. The library this package is
// built on is class 1 and answers 405 to both, so Finder mounts read-only or
// not at all (#3).
//
// What is below answers LOCK with a well-formed token that nothing ever checks.
// That is a lie to the client, and it is worth being exact about what it costs:
// two clients writing the same file at the same time are not protected -- and
// they were not protected before either, because there was no locking at all.
// It removes no guarantee; it declines to add one, in exchange for a client
// that works. The real protection against a lost update is the strong ETag and
// If-Match, which this server has done since the WebDAV surface landed.
//
// If it ever needs to become real, the state is a table and the hard part is
// the If: header grammar in RFC 4918 section 10.4, not this file.
const (
	// lockTimeout is what a client is told its lock lasts. Nothing expires
	// because nothing is stored, so this is only the number Finder shows.
	lockTimeout = time.Hour

	// maxLockBody bounds the request body. The owner element is arbitrary XML
	// echoed straight back, so it is the one place a client could hand us
	// something unbounded.
	maxLockBody = 64 << 10
)

// lockInfo is the request body of a LOCK.
type lockInfo struct {
	XMLName xml.Name `xml:"DAV: lockinfo"`
	// Owner is arbitrary XML that the client expects to see again untouched, so
	// it travels as raw bytes rather than being interpreted.
	Owner ownerXML `xml:"owner"`
}

type ownerXML struct {
	Inner string `xml:",innerxml"`
}

// handleLock answers a LOCK with a token nothing records.
func (f *fileSystem) handleLock(w http.ResponseWriter, r *http.Request) {
	var info lockInfo
	body, err := io.ReadAll(io.LimitReader(r.Body, maxLockBody))
	if err != nil {
		http.Error(w, "cannot read the lock request", http.StatusBadRequest)
		return
	}
	// An empty body is a refresh of an existing lock, which for a server that
	// stores none is the same answer as a new one.
	if len(body) > 0 {
		if err := xml.Unmarshal(body, &info); err != nil {
			http.Error(w, "malformed lock request", http.StatusBadRequest)
			return
		}
	}

	// 201 when the lock creates a lock-null resource, 200 when the resource is
	// already there. Finder locks a path before its first PUT, so this is the
	// ordinary case rather than an edge one.
	status := http.StatusOK
	if p, perr := toPath(strings.TrimPrefix(r.URL.Path, f.prefix)); perr == nil {
		if owner, oerr := f.owner(r.Context()); oerr == nil {
			if _, serr := f.files.Stat(r.Context(), owner, p); serr != nil {
				status = http.StatusCreated
			}
		}
	}

	token := "opaquelocktoken:" + strings.ToLower(rand.Text())
	w.Header().Set("Lock-Token", "<"+token+">")
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(status)

	_, _ = io.WriteString(w, lockDiscovery(token, r.URL.Path, depthOf(r), info.Owner.Inner))
}

// handleUnlock always succeeds. There is nothing to release, and telling a
// client its unlock failed would strand it holding a lock that never existed.
func handleUnlock(w http.ResponseWriter, _ *http.Request) {
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
