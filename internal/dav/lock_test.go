package dav_test

import (
	"encoding/xml"
	"net/http"
	"strings"
	"testing"

	"github.com/C0piIot/stratus-backend/internal/dav"
)

// The body Finder sends. Anything here that comes back changed is a client that
// will not mount.
const lockRequest = `<?xml version="1.0" encoding="utf-8"?>
<D:lockinfo xmlns:D="DAV:">
  <D:lockscope><D:exclusive/></D:lockscope>
  <D:locktype><D:write/></D:locktype>
  <D:owner><D:href>mailto:edu@example.com</D:href></D:owner>
</D:lockinfo>`

func TestOptionsAdvertisesClassTwo(t *testing.T) {
	t.Parallel()
	h := server(t)
	do(t, h, http.MethodPut, "/dav/notes.txt", "hello")

	rec := do(t, h, http.MethodOptions, "/dav/notes.txt", "")
	dav := rec.Header().Get("DAV")

	// Finder looks for a 2 and mounts read-only without it. The 1 and the 3
	// come from the library and must survive.
	for _, class := range []string{"1", "2", "3"} {
		if !strings.Contains(dav, class) {
			t.Errorf("DAV = %q, want it to include class %s", dav, class)
		}
	}
	if allow := rec.Header().Get("Allow"); !strings.Contains(allow, "LOCK") || !strings.Contains(allow, "UNLOCK") {
		t.Errorf("Allow = %q, want LOCK and UNLOCK", allow)
	}
}

func TestLockExistingFile(t *testing.T) {
	t.Parallel()
	h := server(t)
	do(t, h, http.MethodPut, "/dav/notes.txt", "hello")

	rec := do(t, h, "LOCK", "/dav/notes.txt", lockRequest)
	if rec.Code != http.StatusOK {
		t.Fatalf("LOCK on an existing file = %d, want 200", rec.Code)
	}

	token := rec.Header().Get("Lock-Token")
	if !strings.HasPrefix(token, "<opaquelocktoken:") || !strings.HasSuffix(token, ">") {
		t.Errorf("Lock-Token = %q, want an angle-bracketed opaque token", token)
	}

	body := rec.Body.String()
	if !xmlIsWellFormed(t, body) {
		t.Fatalf("the response is not well-formed XML:\n%s", body)
	}
	for _, want := range []string{"lockdiscovery", "activelock", "locktoken", "<D:write/>", "<D:exclusive/>"} {
		if !strings.Contains(body, want) {
			t.Errorf("the lockdiscovery is missing %s:\n%s", want, body)
		}
	}
	// The owner is echoed untouched: clients display it and some compare it.
	if !strings.Contains(body, "mailto:edu@example.com") {
		t.Errorf("the owner was not echoed back:\n%s", body)
	}
	// The lockroot has to name the resource as the client addressed it, prefix
	// and all, or it points at something the client cannot reach.
	if !strings.Contains(body, "<D:href>/dav/notes.txt</D:href>") {
		t.Errorf("the lockroot is not the URL the client used:\n%s", body)
	}
	// The token in the header and the one in the body have to be the same one.
	inner := strings.TrimSuffix(strings.TrimPrefix(token, "<"), ">")
	if !strings.Contains(body, inner) {
		t.Errorf("the body carries a different token than the header:\n%s", body)
	}
}

// TestLockOnNothing: RFC 4918 7.3 lets a LOCK create an empty resource to hang
// itself on, and this refuses. It would mean writing through a filesystem
// built to refuse writes, and no client here needs it -- Finder locks the file
// it is about to replace, which exists.
func TestLockOnNothing(t *testing.T) {
	t.Parallel()
	h := server(t)

	if got := do(t, h, "LOCK", "/dav/new.txt", lockRequest).Code; got != http.StatusNotFound {
		t.Errorf("LOCK on a path with nothing at it = %d, want 404", got)
	}
	if got := do(t, h, http.MethodGet, "/dav/new.txt", "").Code; got != http.StatusNotFound {
		t.Errorf("GET after the refused LOCK = %d, want 404", got)
	}
}

// lockFile takes a real lock and hands back the token, angle brackets and all,
// which is the form both Lock-Token and an If header want it in.
func lockFile(t *testing.T, h http.Handler, target string) string {
	t.Helper()
	rec := do(t, h, "LOCK", target, lockRequest)
	if rec.Code != http.StatusOK {
		t.Fatalf("LOCK %s = %d: %s", target, rec.Code, rec.Body)
	}
	token := rec.Header().Get("Lock-Token")
	if token == "" {
		t.Fatalf("LOCK %s returned no token", target)
	}
	return token
}

// ifHeader is one lock token offered as the condition it is.
func ifHeader(token string) string { return "(" + token + ")" }

// TestARefreshKeepsTheSameLock: a LOCK with no body asks for more time on the
// lock named in the If header. Answering with a fresh token, which is what
// this did while nothing was recorded, tells a client about a lock it does not
// hold and loses it the one it does.
func TestARefreshKeepsTheSameLock(t *testing.T) {
	t.Parallel()
	h := server(t)
	do(t, h, http.MethodPut, "/dav/notes.txt", "hello")
	token := lockFile(t, h, "/dav/notes.txt")

	rec := do(t, h, "LOCK", "/dav/notes.txt", "", "If", ifHeader(token))
	if rec.Code != http.StatusOK {
		t.Fatalf("a refresh = %d, want 200: %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Lock-Token"); got != token {
		t.Errorf("a refresh answered %q, want the same lock %q", got, token)
	}
	// And the lock it refreshed is still the one that opens a write.
	if got := do(t, h, http.MethodPut, "/dav/notes.txt", "again", "If", ifHeader(token)).Code; got != http.StatusNoContent {
		t.Errorf("a PUT with the refreshed token = %d", got)
	}
}

// TestARefreshOfNothing: a token nobody holds is 412, not a new lock.
func TestARefreshOfNothing(t *testing.T) {
	t.Parallel()
	h := server(t)
	do(t, h, http.MethodPut, "/dav/notes.txt", "hello")

	if got := do(t, h, "LOCK", "/dav/notes.txt", "", "If", "(<opaquelocktoken:invented>)").Code; got != http.StatusPreconditionFailed {
		t.Errorf("a refresh of a lock nobody holds = %d, want 412", got)
	}
	if got := do(t, h, "LOCK", "/dav/notes.txt", "").Code; got != http.StatusBadRequest {
		t.Errorf("a refresh naming no lock at all = %d, want 400", got)
	}
}

func TestLockRejectsNonsense(t *testing.T) {
	t.Parallel()
	h := server(t)

	if got := do(t, h, "LOCK", "/dav/notes.txt", "this is not xml").Code; got != http.StatusBadRequest {
		t.Errorf("LOCK with a malformed body = %d, want 400", got)
	}
}

func TestUnlock(t *testing.T) {
	t.Parallel()
	h := server(t)
	do(t, h, http.MethodPut, "/dav/notes.txt", "hello")
	token := lockFile(t, h, "/dav/notes.txt")

	if got := do(t, h, "UNLOCK", "/dav/notes.txt", "", "Lock-Token", token).Code; got != http.StatusNoContent {
		t.Errorf("UNLOCK = %d, want 204", got)
	}
	// And the file is writable again with nothing submitted.
	if got := do(t, h, http.MethodPut, "/dav/notes.txt", "after").Code; got != http.StatusNoContent {
		t.Errorf("a PUT after the UNLOCK = %d", got)
	}

	// Releasing something nobody holds is a conflict with the state the client
	// believes in (RFC 4918 9.11.1), and it can be answered now: while nothing
	// was recorded, a 204 was the only honest thing to say.
	if got := do(t, h, "UNLOCK", "/dav/notes.txt", "", "Lock-Token", token).Code; got != http.StatusConflict {
		t.Errorf("UNLOCK of a lock already released = %d, want 409", got)
	}
	if got := do(t, h, "UNLOCK", "/dav/notes.txt", "", "Lock-Token", "<opaquelocktoken:invented>").Code; got != http.StatusConflict {
		t.Errorf("UNLOCK of a token this server never minted = %d, want 409", got)
	}
	if got := do(t, h, "UNLOCK", "/dav/notes.txt", "").Code; got != http.StatusBadRequest {
		t.Errorf("UNLOCK with no token = %d, want 400", got)
	}
}

// TestAWriteNeedsTheToken is the feature: a second client cannot write what a
// first has locked, and the first still can. The second half is the one that
// matters -- a lock that stops its own owner is worse than no lock.
func TestAWriteNeedsTheToken(t *testing.T) {
	t.Parallel()
	h := server(t)
	do(t, h, http.MethodPut, "/dav/notes.txt", "hello")
	token := lockFile(t, h, "/dav/notes.txt")

	if got := do(t, h, http.MethodPut, "/dav/notes.txt", "from somebody else").Code; got != dav.StatusLocked {
		t.Errorf("a PUT over a lock = %d, want 423", got)
	}
	if got := do(t, h, http.MethodPut, "/dav/notes.txt", "from the owner", "If", ifHeader(token)).Code; got != http.StatusNoContent {
		t.Errorf("a PUT with the token = %d, want it to succeed", got)
	}
	if got := do(t, h, http.MethodGet, "/dav/notes.txt", "").Body.String(); got != "from the owner" {
		t.Errorf("the file reads %q", got)
	}
	// A token that is not the one held is 412: the client made a claim about
	// the state of the resource and the claim was false.
	if got := do(t, h, http.MethodPut, "/dav/notes.txt", "nope", "If", "(<opaquelocktoken:invented>)").Code; got != http.StatusPreconditionFailed {
		t.Errorf("a PUT with somebody else's token = %d, want 412", got)
	}
}

// TestEveryWriteIsGated: a gate over one method and not another is how this
// kind of thing rots, so each of them is asked.
func TestEveryWriteIsGated(t *testing.T) {
	t.Parallel()
	h := server(t)
	do(t, h, http.MethodPut, "/dav/notes.txt", "hello")
	token := lockFile(t, h, "/dav/notes.txt")

	if got := do(t, h, http.MethodDelete, "/dav/notes.txt", "").Code; got != dav.StatusLocked {
		t.Errorf("DELETE over a lock = %d, want 423", got)
	}
	if got := do(t, h, "MOVE", "/dav/notes.txt", "", "Destination", "/dav/moved.txt").Code; got != dav.StatusLocked {
		t.Errorf("MOVE over a lock = %d, want 423", got)
	}
	if got := do(t, h, "PROPPATCH", "/dav/notes.txt", `<?xml version="1.0"?><D:propertyupdate xmlns:D="DAV:"/>`).Code; got != dav.StatusLocked {
		t.Errorf("PROPPATCH over a lock = %d, want 423", got)
	}

	// And a MOVE with the token works, onto a destination nobody has locked --
	// which the lock system on its own would refuse, since it asks for every
	// named resource to be covered by a claimed lock.
	if got := do(t, h, "MOVE", "/dav/notes.txt", "", "Destination", "/dav/moved.txt", "If", ifHeader(token)).Code; got != http.StatusCreated {
		t.Errorf("MOVE with the token = %d, want 201", got)
	}
}

// TestACopyIsAskedAboutItsSource: an untagged list is about the Request-URI
// (RFC 4918 10.4.2), which for a COPY is the source -- the one resource the
// request does not write to. A client that holds a lock on the file it is
// copying submits that token because neon and every other library attach it to
// the request URI, so reading the list as being about the destination would
// refuse a copy for a lock nobody broke.
func TestACopyIsAskedAboutItsSource(t *testing.T) {
	t.Parallel()
	h := server(t)
	do(t, h, http.MethodPut, "/dav/one.txt", "one")
	token := lockFile(t, h, "/dav/one.txt")

	if got := do(t, h, "COPY", "/dav/one.txt", "", "Destination", "/dav/copy.txt", "If", ifHeader(token)).Code; got != http.StatusCreated {
		t.Errorf("COPY carrying the source's token = %d, want 201", got)
	}
	// And a claim about the source that is not true is still 412, even though
	// the source is not what the copy writes to.
	if got := do(t, h, "COPY", "/dav/one.txt", "", "Destination", "/dav/other.txt", "If", "(<opaquelocktoken:invented>)").Code; got != http.StatusPreconditionFailed {
		t.Errorf("COPY with a false claim about the source = %d, want 412", got)
	}
}

// TestALockedDestinationRefusesAWrite is the other end of the same rule: it is
// not only the resource being written that a lock protects.
func TestALockedDestinationRefusesAWrite(t *testing.T) {
	t.Parallel()
	h := server(t)
	do(t, h, http.MethodPut, "/dav/one.txt", "one")
	do(t, h, http.MethodPut, "/dav/two.txt", "two")
	lockFile(t, h, "/dav/two.txt")

	if got := do(t, h, "MOVE", "/dav/one.txt", "", "Destination", "/dav/two.txt").Code; got != dav.StatusLocked {
		t.Errorf("MOVE onto a locked destination = %d, want 423", got)
	}
	if got := do(t, h, "COPY", "/dav/one.txt", "", "Destination", "/dav/two.txt").Code; got != dav.StatusLocked {
		t.Errorf("COPY onto a locked destination = %d, want 423", got)
	}
}

// TestCopyLeavesItsSourceAlone: RFC 4918 7.5.1 says a COPY locks the
// destination and not the source, because it does not modify the source.
// litmus checks it explicitly.
func TestCopyLeavesItsSourceAlone(t *testing.T) {
	t.Parallel()
	h := server(t)
	do(t, h, http.MethodPut, "/dav/one.txt", "one")
	lockFile(t, h, "/dav/one.txt")

	if got := do(t, h, "COPY", "/dav/one.txt", "", "Destination", "/dav/copy.txt").Code; got != http.StatusCreated {
		t.Errorf("COPY out of a locked file = %d, want 201", got)
	}
}

// TestALockCoversWhatIsUnderIt: a lock on a collection is a lock on its
// members, which is the whole reason a client locks a folder.
func TestALockCoversWhatIsUnderIt(t *testing.T) {
	t.Parallel()
	h := server(t)
	do(t, h, "MKCOL", "/dav/album", "")
	do(t, h, http.MethodPut, "/dav/album/one.txt", "one")
	token := lockFile(t, h, "/dav/album")

	if got := do(t, h, http.MethodPut, "/dav/album/two.txt", "two").Code; got != dav.StatusLocked {
		t.Errorf("a PUT inside a locked collection = %d, want 423", got)
	}
	if got := do(t, h, http.MethodPut, "/dav/album/two.txt", "two", "If", ifHeader(token)).Code; got != http.StatusCreated {
		t.Errorf("a PUT inside a locked collection with the token = %d", got)
	}
	// Both ends of a move inside it are the same lock, and claiming it twice
	// must not be what refuses the request.
	if got := do(t, h, "MOVE", "/dav/album/one.txt", "", "Destination", "/dav/album/three.txt", "If", ifHeader(token)).Code; got != http.StatusCreated {
		t.Errorf("a MOVE inside a locked collection with the token = %d", got)
	}
}

// TestReadsAreNeverBlocked: a write lock has never stopped a read in this
// protocol, and a client that locks a folder still has to be able to list it.
func TestReadsAreNeverBlocked(t *testing.T) {
	t.Parallel()
	h := server(t)
	do(t, h, http.MethodPut, "/dav/notes.txt", "hello")
	lockFile(t, h, "/dav/notes.txt")

	if got := do(t, h, http.MethodGet, "/dav/notes.txt", "").Code; got != http.StatusOK {
		t.Errorf("GET of a locked file = %d", got)
	}
	if got := do(t, h, "PROPFIND", "/dav/notes.txt", "", "Depth", "0").Code; got != http.StatusMultiStatus {
		t.Errorf("PROPFIND of a locked file = %d", got)
	}
}

// TestATaggedListHasToNameThisRequest closes a hole x/net has: it confirms the
// conditions against the resource the list is tagged with rather than against
// the one being written, so a client holding a lock of its own anywhere can
// name it and walk past somebody else's.
func TestATaggedListHasToNameThisRequest(t *testing.T) {
	t.Parallel()
	h := server(t)
	do(t, h, http.MethodPut, "/dav/mine.txt", "mine")
	do(t, h, http.MethodPut, "/dav/theirs.txt", "theirs")
	mine := lockFile(t, h, "/dav/mine.txt")
	lockFile(t, h, "/dav/theirs.txt")

	tagged := "<http://example.com/dav/mine.txt> " + ifHeader(mine)
	if got := do(t, h, http.MethodPut, "/dav/theirs.txt", "not mine to write", "If", tagged).Code; got == http.StatusNoContent {
		t.Error("a lock on one file opened a write to another")
	}
	// The same list, tagged with the resource it is actually about, works --
	// including as a relative reference, which the grammar allows.
	relative := "</dav/mine.txt> " + ifHeader(mine)
	if got := do(t, h, http.MethodPut, "/dav/mine.txt", "mine to write", "If", relative).Code; got != http.StatusNoContent {
		t.Errorf("a tagged list naming its own resource = %d", got)
	}
}

// TestAnETagConditionIsChecked: `If: (<token> ["etag"])` is a client saying
// "only if the bytes are still these", and the lock system ignores everything
// in a condition that is not a token -- so without locks.go doing it, half of
// what the client asked would be quietly dropped. litmus is what found it.
func TestAnETagConditionIsChecked(t *testing.T) {
	t.Parallel()
	h := server(t)
	do(t, h, http.MethodPut, "/dav/notes.txt", "hello")
	etag := do(t, h, http.MethodGet, "/dav/notes.txt", "").Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag to condition on")
	}
	token := lockFile(t, h, "/dav/notes.txt")

	both := "(" + token + " [" + etag + "])"
	if got := do(t, h, http.MethodPut, "/dav/notes.txt", "still mine", "If", both).Code; got != http.StatusNoContent {
		t.Errorf("a PUT with the token and the right ETag = %d", got)
	}
	// The write above changed the bytes, so the same condition is now false.
	if got := do(t, h, http.MethodPut, "/dav/notes.txt", "stale", "If", both).Code; got != http.StatusPreconditionFailed {
		t.Errorf("a PUT with a stale ETag = %d, want 412", got)
	}
}

// TestASharedLockIsRefused: the lock system holds one token per resource, so a
// shared lock cannot be granted -- and answering with an exclusive one, which
// is what happens if the body is not read, tells a client it shares something
// it does not. PROPFIND advertises exclusive alone, so this is the same answer
// twice rather than a surprise.
func TestASharedLockIsRefused(t *testing.T) {
	t.Parallel()
	h := server(t)
	do(t, h, http.MethodPut, "/dav/notes.txt", "hello")

	const shared = `<?xml version="1.0" encoding="utf-8"?>
<D:lockinfo xmlns:D="DAV:">
  <D:lockscope><D:shared/></D:lockscope>
  <D:locktype><D:write/></D:locktype>
</D:lockinfo>`
	if got := do(t, h, "LOCK", "/dav/notes.txt", shared).Code; got != http.StatusNotImplemented {
		t.Errorf("a shared LOCK = %d, want 501", got)
	}
	// And nothing was taken: an exclusive one still works.
	if got := do(t, h, "LOCK", "/dav/notes.txt", lockRequest).Code; got != http.StatusOK {
		t.Errorf("an exclusive LOCK after the refusal = %d", got)
	}
}

// TestATaggedCollectionCoversAFileInIt is how a client submits the token of a
// folder it locked while writing a file inside: the list is tagged with the
// collection, which is neither the request URI nor a resource the request
// names. litmus does exactly this and it is the shape a first cut refuses.
func TestATaggedCollectionCoversAFileInIt(t *testing.T) {
	t.Parallel()
	h := server(t)
	do(t, h, "MKCOL", "/dav/album", "")
	token := lockFile(t, h, "/dav/album")

	tagged := "<http://example.com/dav/album/> " + ifHeader(token)
	if got := do(t, h, http.MethodPut, "/dav/album/one.txt", "one", "If", tagged).Code; got != http.StatusCreated {
		t.Errorf("a PUT under a collection whose token was submitted for it = %d", got)
	}
}

// TestARestartForgets is the cost of keeping locks in memory, asserted rather
// than left implied: the README says it and this is what makes that true.
func TestARestartForgets(t *testing.T) {
	t.Parallel()
	svc := service(t)
	before := withUser(dav.Handler(prefix, svc), "edu")
	do(t, before, http.MethodPut, "/dav/notes.txt", "hello")
	lockFile(t, before, "/dav/notes.txt")

	after := withUser(dav.Handler(prefix, svc), "edu")
	if got := do(t, after, http.MethodPut, "/dav/notes.txt", "after the restart").Code; got != http.StatusNoContent {
		t.Errorf("a PUT after a restart = %d, want the lock to have been forgotten", got)
	}
}

func xmlIsWellFormed(t *testing.T, s string) bool {
	t.Helper()
	decoder := xml.NewDecoder(strings.NewReader(s))
	for {
		_, err := decoder.Token()
		if err != nil {
			return err.Error() == "EOF"
		}
	}
}
