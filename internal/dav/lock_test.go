package dav_test

import (
	"encoding/xml"
	"net/http"
	"strings"
	"testing"
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

// TestLockMissingFile is the case Finder actually performs: it locks a path
// before the first PUT, and a 200 there tells it the file already exists.
func TestLockMissingFile(t *testing.T) {
	t.Parallel()
	h := server(t)

	rec := do(t, h, "LOCK", "/dav/new.txt", lockRequest)
	if rec.Code != http.StatusCreated {
		t.Errorf("LOCK on a path with nothing at it = %d, want 201", rec.Code)
	}
	// And it created nothing: the lock is a promise, not a file.
	if got := do(t, h, http.MethodGet, "/dav/new.txt", "").Code; got != http.StatusNotFound {
		t.Errorf("GET after LOCK = %d, want 404", got)
	}
}

func TestLockRefreshHasNoBody(t *testing.T) {
	t.Parallel()
	h := server(t)
	do(t, h, http.MethodPut, "/dav/notes.txt", "hello")

	// A refresh carries no body and the token in an If: header. Nothing is
	// stored, so it gets a fresh answer rather than an error.
	rec := do(t, h, "LOCK", "/dav/notes.txt", "", "If", "(<opaquelocktoken:whatever>)")
	if rec.Code != http.StatusOK {
		t.Errorf("a refresh = %d, want 200", rec.Code)
	}
	if rec.Header().Get("Lock-Token") == "" {
		t.Error("a refresh returned no token")
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

	rec := do(t, h, "UNLOCK", "/dav/notes.txt", "", "Lock-Token", "<opaquelocktoken:whatever>")
	if rec.Code != http.StatusNoContent {
		t.Errorf("UNLOCK = %d, want 204", rec.Code)
	}
	// Even one nobody ever took: telling a client its unlock failed would
	// strand it holding something that never existed.
	if got := do(t, h, "UNLOCK", "/dav/never-locked.txt", "").Code; got != http.StatusNoContent {
		t.Errorf("UNLOCK of a lock that was never taken = %d, want 204", got)
	}
}

// TestWritesIgnoreTheLock is the honest half of the feature: the token is not
// checked, so a write carrying somebody else's token still succeeds. It is
// asserted rather than left implicit, because that is what "advertised and not
// enforced" means in practice.
func TestWritesIgnoreTheLock(t *testing.T) {
	t.Parallel()
	h := server(t)
	do(t, h, "LOCK", "/dav/notes.txt", lockRequest)

	rec := do(t, h, http.MethodPut, "/dav/notes.txt", "written anyway", "If", "(<opaquelocktoken:somebody-elses>)")
	if rec.Code != http.StatusCreated {
		t.Errorf("PUT with a foreign lock token = %d, want it to succeed", rec.Code)
	}
	if got := do(t, h, http.MethodGet, "/dav/notes.txt", "").Body.String(); got != "written anyway" {
		t.Errorf("the file reads %q", got)
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
