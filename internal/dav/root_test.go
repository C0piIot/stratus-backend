package dav_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/C0piIot/stratus-backend/internal/dav"
)

// root is the collection the whole server is, with the children it is given.
func root(t *testing.T) http.Handler {
	t.Helper()
	return dav.Root("/", "files", "photos", "playlists")
}

// TestTheRootAdvertisesWebDAV is the answer the Windows redirector probes for
// before it will mount anything at all (#281): it asks the origin, not the
// path it was given, and refuses a server whose answer says nothing about DAV.
func TestTheRootAdvertisesWebDAV(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	root(t).ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodOptions, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("OPTIONS / = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("DAV"); got != "1, 2" {
		t.Errorf("DAV = %q, want class 2: the tree underneath locks", got)
	}
	if got := rec.Header().Get("MS-Author-Via"); got != "DAV" {
		t.Errorf("MS-Author-Via = %q", got)
	}
	// Honest about itself: nothing is written here.
	if got := rec.Header().Get("Allow"); got != "OPTIONS, PROPFIND" {
		t.Errorf("Allow = %q, want the two it answers", got)
	}
}

// TestTheRootListsTheMounts is what the composition buys: a client that mounts
// the origin walks into the tree and into the generated collections beside it,
// instead of having to be told three URLs (#279).
func TestTheRootListsTheMounts(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequestWithContext(t.Context(), "PROPFIND", "/", nil)
	req.Header.Set("Depth", "1")
	rec := httptest.NewRecorder()
	root(t).ServeHTTP(rec, req)

	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("PROPFIND / = %d, want 207", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"<D:href>/</D:href>",
		"<D:href>/files/</D:href>",
		"<D:href>/photos/</D:href>",
		"<D:href>/playlists/</D:href>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the listing does not hold %s:\n%s", want, body)
		}
	}
	// Collections, all of them, or a client offers to download the tree.
	if got := strings.Count(body, "<D:collection>"); got != 4 {
		t.Errorf("%d of the four are collections:\n%s", got, body)
	}
}

// TestTheRootAtDepthZeroIsItself.
func TestTheRootAtDepthZeroIsItself(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequestWithContext(t.Context(), "PROPFIND", "/", nil)
	req.Header.Set("Depth", "0")
	rec := httptest.NewRecorder()
	root(t).ServeHTTP(rec, req)

	if body := rec.Body.String(); strings.Contains(body, "/files/") {
		t.Errorf("a depth of zero listed a child:\n%s", body)
	}
}

// TestTheRootRefusesEverythingElse: it is a listing and nothing else, and a
// client that tries to write at this level is told so rather than finding out
// from a database error.
func TestTheRootRefusesEverythingElse(t *testing.T) {
	t.Parallel()
	for _, method := range []string{http.MethodPut, http.MethodDelete, "MKCOL", "MOVE", "COPY", "LOCK", "PROPPATCH"} {
		rec := httptest.NewRecorder()
		root(t).ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), method, "/", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s / = %d, want 405", method, rec.Code)
		}
		if rec.Header().Get("Allow") == "" {
			t.Errorf("%s / refused without saying what it does answer", method)
		}
	}
}

// TestTheRootRefusesTheWholeTree: the same rule as every other mount here, and
// for the same reason -- a multistatus of the library is built whole in memory
// before any of it goes out.
func TestTheRootRefusesTheWholeTree(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequestWithContext(t.Context(), "PROPFIND", "/", nil)
	req.Header.Set("Depth", "infinity")
	rec := httptest.NewRecorder()
	root(t).ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("PROPFIND / with Depth: infinity = %d, want 403", rec.Code)
	}
}
