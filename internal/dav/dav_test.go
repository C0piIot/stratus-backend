package dav_test

import (
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/C0piIot/stratus-backend/internal/auth"
	"github.com/C0piIot/stratus-backend/internal/dav"
	"github.com/C0piIot/stratus-backend/internal/db/sqlite"
	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/storage/disk"
)

const prefix = "/dav/"

// server drives the real handler over the real backends. A WebDAV adapter that
// is only tested against fakes tests the fakes.
func server(t *testing.T) http.Handler {
	t.Helper()
	// The handler takes the owner from the request, so the tests put one there
	// the way auth.Basic does.
	return withUser(dav.Handler(prefix, service(t)), "edu")
}

// service is the real file layer over real backends in a temporary directory:
// an adapter tested only against fakes tests the fakes.
func service(t *testing.T) *files.Service {
	t.Helper()
	dir := t.TempDir()

	blobs, err := disk.New(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blobs.Close() })

	meta, err := sqlite.New(t.Context(), filepath.Join(dir, "stratus.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = meta.Close() })
	if err := meta.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return files.New(blobs, meta)
}

func withUser(h http.Handler, username string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(auth.WithUser(r.Context(), username)))
	})
}

func do(t *testing.T, h http.Handler, method, target, body string, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequestWithContext(t.Context(), method, target, reader)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestPutGetDelete(t *testing.T) {
	t.Parallel()
	h := server(t)

	if got := do(t, h, http.MethodPut, "/dav/notes.txt", "hello").Code; got != http.StatusCreated {
		t.Errorf("PUT = %d, want 201", got)
	}
	// A second PUT replaces rather than creates.
	if got := do(t, h, http.MethodPut, "/dav/notes.txt", "hello again").Code; got != http.StatusNoContent {
		t.Errorf("PUT over an existing file = %d, want 204", got)
	}

	rec := do(t, h, http.MethodGet, "/dav/notes.txt", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "hello again" {
		t.Errorf("body = %q", rec.Body.String())
	}
	if rec.Header().Get("ETag") == "" {
		t.Error("no ETag, so a client cannot tell whether it changed")
	}

	if got := do(t, h, http.MethodDelete, "/dav/notes.txt", "").Code; got != http.StatusNoContent {
		t.Errorf("DELETE = %d, want 204", got)
	}
	if got := do(t, h, http.MethodGet, "/dav/notes.txt", "").Code; got != http.StatusNotFound {
		t.Errorf("GET after DELETE = %d, want 404", got)
	}
}

// TestRangeRequest is the assertion that the reader really seeks: without it
// go-webdav copies the whole body and video seeking does not work.
func TestRangeRequest(t *testing.T) {
	t.Parallel()
	h := server(t)
	do(t, h, http.MethodPut, "/dav/alphabet", "abcdefghijklmnopqrstuvwxyz")

	rec := do(t, h, http.MethodGet, "/dav/alphabet", "", "Range", "bytes=2-4")
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("ranged GET = %d, want 206", rec.Code)
	}
	if rec.Body.String() != "cde" {
		t.Errorf("body = %q, want cde", rec.Body.String())
	}
	if got := rec.Header().Get("Content-Range"); got != "bytes 2-4/26" {
		t.Errorf("Content-Range = %q", got)
	}
}

// hrefs is every <D:response> href in a multistatus, in the order the server
// sent them.
//
// Parsed rather than grepped because strings.Contains cannot answer the
// question this file asks: "/dav/album" is a substring of "/dav/album/one.txt",
// so a listing that omits the collection looks exactly like one that includes
// it -- and that difference is the whole of #126. It cannot see a duplicate
// either.
func hrefs(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("PROPFIND = %d, want 207:\n%s", rec.Code, rec.Body.String())
	}

	var ms struct {
		XMLName   xml.Name `xml:"DAV: multistatus"`
		Responses []struct {
			Href string `xml:"DAV: href"`
		} `xml:"DAV: response"`
	}
	// Namespaced rather than by local name: it costs nothing here and turns
	// "some XML came back" into "a DAV multistatus came back".
	if err := xml.Unmarshal(rec.Body.Bytes(), &ms); err != nil {
		t.Fatalf("the multistatus does not parse: %v\n%s", err, rec.Body.String())
	}

	out := make([]string, 0, len(ms.Responses))
	for _, r := range ms.Responses {
		href, err := url.PathUnescape(strings.TrimSpace(r.Href))
		if err != nil {
			t.Fatalf("href %q does not decode: %v", r.Href, err)
		}
		out = append(out, href)
	}
	return out
}

// wantHrefs compares a listing as a set, since the order of the members is the
// server's business -- except for the first entry, which the callers that care
// about check themselves.
// wantHrefs compares a listing's hrefs, where a collection's ends in a slash:
// that is what RFC 4918's own examples do and what the library answering
// PROPFIND emits (see propfind.go).
func wantHrefs(t *testing.T, got []string, want ...string) {
	t.Helper()
	sorted := slices.Clone(got)
	slices.Sort(sorted)
	slices.Sort(want)
	if !slices.Equal(sorted, want) {
		t.Errorf("hrefs = %v, want %v", got, want)
	}
}

// TestPropfind is the regression test for #126: a Depth 1 listing is the
// collection and its members, not its members alone. A client reads the
// collection's own properties out of the entry whose href is the one it asked
// for, and without it has to spend a second request to learn them.
func TestPropfind(t *testing.T) {
	t.Parallel()
	h := server(t)
	do(t, h, "MKCOL", "/dav/album", "")
	do(t, h, http.MethodPut, "/dav/album/one.txt", "one")
	do(t, h, http.MethodPut, "/dav/album/two.txt", "two")

	got := hrefs(t, do(t, h, "PROPFIND", "/dav/album", "", "Depth", "1"))
	wantHrefs(t, got, "/dav/album/", "/dav/album/one.txt", "/dav/album/two.txt")

	// First, which is where mod_dav, sabre/dav and go-webdav's own local
	// backend put it, and what a client that takes response[0] expects.
	if len(got) > 0 && got[0] != "/dav/album/" {
		t.Errorf("hrefs[0] = %q, want the collection itself", got[0])
	}
}

// TestPropfindDepthZero pins the shape the self entry above is copied from: one
// response, and the same href a Depth 1 listing now carries for it.
func TestPropfindDepthZero(t *testing.T) {
	t.Parallel()
	h := server(t)
	do(t, h, "MKCOL", "/dav/album", "")
	do(t, h, http.MethodPut, "/dav/album/one.txt", "one")

	wantHrefs(t, hrefs(t, do(t, h, "PROPFIND", "/dav/album", "", "Depth", "0")), "/dav/album/")
}

// TestPropfindOfAnEmptyCollection is the other half of #126, and the pair is the
// point: before the self entry, an empty collection and one that does not exist
// were both zero members, and no client could tell them apart without asking a
// second time.
func TestPropfindOfAnEmptyCollection(t *testing.T) {
	t.Parallel()
	h := server(t)
	do(t, h, "MKCOL", "/dav/empty", "")

	wantHrefs(t, hrefs(t, do(t, h, "PROPFIND", "/dav/empty", "", "Depth", "1")), "/dav/empty/")

	if code := do(t, h, "PROPFIND", "/dav/missing", "", "Depth", "1").Code; code != http.StatusNotFound {
		t.Errorf("PROPFIND of a missing collection = %d, want 404", code)
	}
}

// TestPropfindOfTheRoot covers the collection that has no database row, and
// pins the one asymmetry in the hrefs: the root carries a trailing slash and
// every other collection does not. That is what Depth 0 has always answered,
// and this change only makes it visible inside a listing -- a client comparing
// hrefs has to normalise before it compares.
func TestPropfindOfTheRoot(t *testing.T) {
	t.Parallel()
	h := server(t)
	do(t, h, "MKCOL", "/dav/album", "")
	do(t, h, http.MethodPut, "/dav/notes.txt", "notes")

	wantHrefs(t, hrefs(t, do(t, h, "PROPFIND", "/dav/", "", "Depth", "1")),
		"/dav/", "/dav/album/", "/dav/notes.txt")
}

// TestPropfindOfAFile pins that none of this reaches a resource that is not a
// collection: the library answers those from Stat alone.
func TestPropfindOfAFile(t *testing.T) {
	t.Parallel()
	h := server(t)
	do(t, h, http.MethodPut, "/dav/notes.txt", "notes")

	wantHrefs(t, hrefs(t, do(t, h, "PROPFIND", "/dav/notes.txt", "", "Depth", "1")), "/dav/notes.txt")
}

// TestPropfindEscapesTheSelfHref: the collection's own href goes through the
// same encoder its members do, which is worth one case because the self entry
// is the one nothing exercised before.
func TestPropfindEscapesTheSelfHref(t *testing.T) {
	t.Parallel()
	h := server(t)
	do(t, h, "MKCOL", "/dav/rock%20&%20roll", "")
	do(t, h, http.MethodPut, "/dav/rock%20&%20roll/song.mp3", "song")

	wantHrefs(t, hrefs(t, do(t, h, "PROPFIND", "/dav/rock%20&%20roll", "", "Depth", "1")),
		"/dav/rock & roll/", "/dav/rock & roll/song.mp3")
}

// TestMoveACollectionWithThingsInIt is #101 over WebDAV: renaming a folder is
// what a Finder drag or an rclone moveto does, and it answered 409 until the
// metadata port could rewrite a subtree.
func TestMoveACollectionWithThingsInIt(t *testing.T) {
	t.Parallel()
	h := server(t)
	do(t, h, "MKCOL", "/dav/album", "")
	do(t, h, "MKCOL", "/dav/album/raw", "")
	do(t, h, http.MethodPut, "/dav/album/one.txt", "one")
	do(t, h, http.MethodPut, "/dav/album/raw/deep.txt", "deep")

	// 201 and not 204: RFC 4918 9.9.4 keeps them apart by whether the
	// destination existed, and nothing was at /dav/archive.
	if got := do(t, h, "MOVE", "/dav/album", "", "Destination", "/dav/archive").Code; got != http.StatusCreated {
		t.Fatalf("MOVE of a collection = %d, want 201", got)
	}

	// Everything came with it, which is what a listing of the new name shows.
	wantHrefs(t, hrefs(t, do(t, h, "PROPFIND", "/dav/archive", "", "Depth", "1")),
		"/dav/archive/", "/dav/archive/one.txt", "/dav/archive/raw/")
	if body := do(t, h, http.MethodGet, "/dav/archive/raw/deep.txt", "").Body.String(); body != "deep" {
		t.Errorf("the deepest file reads %q after the move", body)
	}
	if got := do(t, h, http.MethodGet, "/dav/album/one.txt", "").Code; got != http.StatusNotFound {
		t.Errorf("the old path still answers: %d", got)
	}

	// And the one rename that cannot be done, because the destination is inside
	// what is being moved.
	do(t, h, "MKCOL", "/dav/photos", "")
	if got := do(t, h, "MOVE", "/dav/photos", "", "Destination", "/dav/photos/inner").Code; got != http.StatusConflict {
		t.Errorf("MOVE of a collection into itself = %d, want 409", got)
	}
}

func TestPropfindRefusesAnInfiniteDepth(t *testing.T) {
	t.Parallel()
	h := server(t)
	do(t, h, "MKCOL", "/dav/album", "")
	do(t, h, "MKCOL", "/dav/album/raw", "")
	do(t, h, http.MethodPut, "/dav/album/raw/deep.txt", "deep")

	// RFC 4918 9.1 lets a server refuse the whole tree at once, and 14.5 says
	// what it has to answer so that a client knows to walk it a level at a
	// time instead of retrying the same thing. Measured at 44 MB of XML built
	// inside 300 MB of heap for a hundred thousand files, and linear (#160).
	for _, depth := range []string{"infinity", ""} {
		rec := do(t, h, "PROPFIND", "/dav/", "", "Depth", depth)
		if rec.Code != http.StatusForbidden {
			t.Errorf("PROPFIND with Depth %q = %d, want 403", depth, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "propfind-finite-depth") {
			t.Errorf("PROPFIND with Depth %q answered %q, want the precondition",
				depth, rec.Body.String())
		}
	}

	// An absent header is the same request: RFC 4918 9.1 says a PROPFIND with
	// no Depth means infinity, and answering one level to it would be telling
	// a client the tree is four entries deep when it is not.

	// And a level at a time still works, which is the way through.
	wantHrefs(t, hrefs(t, do(t, h, "PROPFIND", "/dav/", "", "Depth", "1")),
		"/dav/", "/dav/album/")
	wantHrefs(t, hrefs(t, do(t, h, "PROPFIND", "/dav/album/raw", "", "Depth", "1")),
		"/dav/album/raw/", "/dav/album/raw/deep.txt")
}

func TestCollections(t *testing.T) {
	t.Parallel()
	h := server(t)

	if got := do(t, h, "MKCOL", "/dav/album", "").Code; got != http.StatusCreated {
		t.Errorf("MKCOL = %d, want 201", got)
	}
	// RFC 4918 9.3.1.
	if got := do(t, h, "MKCOL", "/dav/album", "").Code; got != http.StatusMethodNotAllowed {
		t.Errorf("MKCOL over an existing collection = %d, want 405", got)
	}
	if got := do(t, h, "MKCOL", "/dav/missing/inner", "").Code; got != http.StatusConflict {
		t.Errorf("MKCOL with no parent = %d, want 409", got)
	}
	// RFC 4918 9.7.1: same rule for PUT.
	if got := do(t, h, http.MethodPut, "/dav/missing/file.txt", "x").Code; got != http.StatusConflict {
		t.Errorf("PUT with no parent = %d, want 409", got)
	}

	do(t, h, http.MethodPut, "/dav/album/one.txt", "one")
	if got := do(t, h, http.MethodDelete, "/dav/album", "").Code; got != http.StatusNoContent {
		t.Errorf("DELETE on a collection = %d, want 204", got)
	}
	if got := do(t, h, http.MethodGet, "/dav/album/one.txt", "").Code; got != http.StatusNotFound {
		t.Error("the collection was deleted but its contents survived")
	}
}

func TestMove(t *testing.T) {
	t.Parallel()
	h := server(t)
	do(t, h, "MKCOL", "/dav/album", "")
	do(t, h, http.MethodPut, "/dav/photo.jpg", "bytes")

	rec := do(t, h, "MOVE", "/dav/photo.jpg", "", "Destination", "/dav/album/photo.jpg")
	if rec.Code != http.StatusCreated {
		t.Fatalf("MOVE = %d, want 201", rec.Code)
	}
	if got := do(t, h, http.MethodGet, "/dav/album/photo.jpg", "").Body.String(); got != "bytes" {
		t.Errorf("the moved file reads %q", got)
	}
	if got := do(t, h, http.MethodGet, "/dav/photo.jpg", "").Code; got != http.StatusNotFound {
		t.Errorf("the old path still resolves: %d", got)
	}
}

func TestCopy(t *testing.T) {
	t.Parallel()
	h := server(t)
	do(t, h, http.MethodPut, "/dav/one.txt", "content")

	if got := do(t, h, "COPY", "/dav/one.txt", "", "Destination", "/dav/two.txt").Code; got != http.StatusCreated {
		t.Errorf("COPY = %d, want 201", got)
	}
	if got := do(t, h, http.MethodGet, "/dav/two.txt", "").Body.String(); got != "content" {
		t.Errorf("the copy reads %q", got)
	}
	// The original is untouched.
	if got := do(t, h, http.MethodGet, "/dav/one.txt", "").Body.String(); got != "content" {
		t.Errorf("the source reads %q", got)
	}

	// And a collection, with everything under it (#43).
	do(t, h, "MKCOL", "/dav/album", "")
	do(t, h, http.MethodPut, "/dav/album/deep.txt", "deep")
	if got := do(t, h, "COPY", "/dav/album", "", "Destination", "/dav/copy").Code; got != http.StatusCreated {
		t.Errorf("COPY of a collection = %d, want 201", got)
	}
	if got := do(t, h, http.MethodGet, "/dav/copy/deep.txt", "").Body.String(); got != "deep" {
		t.Errorf("the copied tree reads %q", got)
	}
}

// TestPathTraversal is the one every file server gets wrong. The dots are
// resolved against the root of the owner's own tree, so the worst a client can
// do is address something it already had permission to address.
func TestPathTraversal(t *testing.T) {
	t.Parallel()
	h := server(t)

	// One level up from the collection root lands back at the collection root.
	if got := do(t, h, http.MethodPut, "/dav/../notes.txt", "in the tree").Code; got != http.StatusCreated {
		t.Fatalf("PUT = %d, want 201", got)
	}
	if got := do(t, h, http.MethodGet, "/dav/notes.txt", "").Body.String(); got != "in the tree" {
		t.Errorf("it did not land at the root of the tree: %q", got)
	}

	// And a deeper escape is not an escape either: it becomes a path inside the
	// tree whose parent does not exist.
	if got := do(t, h, http.MethodPut, "/dav/../../etc/passwd", "pwned").Code; got != http.StatusConflict {
		t.Errorf("PUT = %d, want 409: it should be a path in the tree with no parent", got)
	}
}

func TestConditionalPut(t *testing.T) {
	t.Parallel()
	h := server(t)
	do(t, h, http.MethodPut, "/dav/notes.txt", "first")

	// If-None-Match: * means "only if it does not exist yet".
	if got := do(t, h, http.MethodPut, "/dav/notes.txt", "second", "If-None-Match", "*").Code; got != http.StatusPreconditionFailed {
		t.Errorf("PUT with If-None-Match: * over an existing file = %d, want 412", got)
	}
	if got := do(t, h, http.MethodGet, "/dav/notes.txt", "").Body.String(); got != "first" {
		t.Errorf("the refused PUT wrote anyway: %q", got)
	}

	etag := do(t, h, http.MethodGet, "/dav/notes.txt", "").Header().Get("ETag")
	if got := do(t, h, http.MethodPut, "/dav/notes.txt", "third", "If-Match", etag).Code; got != http.StatusNoContent {
		t.Errorf("PUT with a matching If-Match = %d, want 204", got)
	}
	if got := do(t, h, http.MethodPut, "/dav/notes.txt", "fourth", "If-Match", `"stale"`).Code; got != http.StatusPreconditionFailed {
		t.Errorf("PUT with a stale If-Match = %d, want 412", got)
	}
}

func TestMoveEdges(t *testing.T) {
	t.Parallel()
	h := server(t)
	do(t, h, http.MethodPut, "/dav/one.txt", "one")
	do(t, h, http.MethodPut, "/dav/two.txt", "two")

	// RFC 4918 9.9.4: a destination outside this collection is not ours to
	// write to.
	if got := do(t, h, "MOVE", "/dav/one.txt", "", "Destination", "/elsewhere/one.txt").Code; got != http.StatusBadGateway {
		t.Errorf("MOVE outside the collection = %d, want 502", got)
	}
	// Overwrite: F means do not clobber.
	if got := do(t, h, "MOVE", "/dav/one.txt", "", "Destination", "/dav/two.txt", "Overwrite", "F").Code; got != http.StatusPreconditionFailed {
		t.Errorf("MOVE with Overwrite: F onto an existing file = %d, want 412", got)
	}
	// And with overwrite allowed it replaces, answering 204 rather than 201.
	if got := do(t, h, "MOVE", "/dav/one.txt", "", "Destination", "/dav/two.txt").Code; got != http.StatusNoContent {
		t.Errorf("MOVE over an existing file = %d, want 204", got)
	}
	if got := do(t, h, http.MethodGet, "/dav/two.txt", "").Body.String(); got != "one" {
		t.Errorf("the destination reads %q, want the moved content", got)
	}
}

func TestMissingAndInvalid(t *testing.T) {
	t.Parallel()
	h := server(t)

	if got := do(t, h, "PROPFIND", "/dav/nothing/", "", "Depth", "1").Code; got != http.StatusNotFound {
		t.Errorf("PROPFIND on a missing collection = %d, want 404", got)
	}
	if got := do(t, h, http.MethodDelete, "/dav/nothing.txt", "").Code; got != http.StatusNotFound {
		t.Errorf("DELETE of a missing file = %d, want 404", got)
	}
	// The root is not a row and is not deletable.
	if got := do(t, h, http.MethodDelete, "/dav/", "").Code; got != http.StatusForbidden {
		t.Errorf("DELETE of the collection root = %d, want 403", got)
	}
}

func TestContentTypeComesFromTheExtension(t *testing.T) {
	t.Parallel()
	h := server(t)
	// A distroless image has no /etc/mime.types, so the table this relies on is
	// the one pinned in the package.
	do(t, h, http.MethodPut, "/dav/photo.heic", "not really a heic")

	if got := do(t, h, http.MethodGet, "/dav/photo.heic", "").Header().Get("Content-Type"); got != "image/heic" {
		t.Errorf("Content-Type = %q, want image/heic", got)
	}
}

// TestWithoutAnAuthenticatedUser is the fail-closed case for the change that
// took the owner off the constructor: reaching the backend with no user on the
// request is a routing mistake, and serving somebody's files on a guess is the
// wrong way to find out.
func TestWithoutAnAuthenticatedUser(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	blobs, err := disk.New(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blobs.Close() })
	meta, err := sqlite.New(t.Context(), filepath.Join(dir, "stratus.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = meta.Close() })
	if merr := meta.Migrate(t.Context()); merr != nil {
		t.Fatal(merr)
	}

	// No auth.Basic in front of it, so nothing put a user on the context.
	h := dav.Handler(prefix, files.New(blobs, meta))

	// PROPFIND goes with an empty body: a body that is not XML is rejected
	// before the request ever reaches the backend, which would test the parser
	// rather than the check.
	// Every entry point, not a sample of them: the check sits in each one, so
	// one that forgot it would be the one nobody listed here.
	for _, tt := range []struct {
		method, body string
		headers      []string
	}{
		{method: http.MethodGet},
		{method: http.MethodPut, body: "x"},
		{method: http.MethodDelete},
		{method: "MKCOL"},
		// Depth 1, because a PROPFIND that asks for the whole tree is refused
		// before this package looks at who is asking -- in the server that
		// cannot happen, since auth.Basic is in front of the whole handler.
		{method: "PROPFIND", headers: []string{"Depth", "1"}},
		{method: "MOVE", headers: []string{"Destination", "/dav/moved.txt"}},
		{method: "COPY", headers: []string{"Destination", "/dav/copied.txt"}},
	} {
		rec := do(t, h, tt.method, "/dav/notes.txt", tt.body, tt.headers...)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s with no user = %d, want 401", tt.method, rec.Code)
		}
	}
}

// TestConditionalDelete is the other place the conditional headers are read.
// A client that deletes what it believes it has read is protecting itself from
// removing somebody else's change, and RemoveAll has to honour that the way PUT
// does.
func TestConditionalDelete(t *testing.T) {
	t.Parallel()
	h := server(t)
	do(t, h, http.MethodPut, "/dav/notes.txt", "first")
	etag := do(t, h, http.MethodGet, "/dav/notes.txt", "").Header().Get("ETag")

	if got := do(t, h, http.MethodDelete, "/dav/notes.txt", "", "If-Match", `"stale"`).Code; got != http.StatusPreconditionFailed {
		t.Errorf("DELETE with a stale If-Match = %d, want 412", got)
	}
	if got := do(t, h, http.MethodGet, "/dav/notes.txt", "").Code; got != http.StatusOK {
		t.Error("the refused DELETE removed the file anyway")
	}

	if got := do(t, h, http.MethodDelete, "/dav/notes.txt", "", "If-Match", etag).Code; got != http.StatusNoContent {
		t.Errorf("DELETE with a matching If-Match = %d, want 204", got)
	}
	if got := do(t, h, http.MethodGet, "/dav/notes.txt", "").Code; got != http.StatusNotFound {
		t.Error("the file survived a delete that was allowed")
	}
}

// TestCopyEdges covers what COPY does that MOVE does not: Depth 0 on a
// collection, and 201 or 204 depending on whether the destination was there.
func TestCopyEdges(t *testing.T) {
	t.Parallel()
	h := server(t)
	do(t, h, "MKCOL", "/dav/album", "")
	do(t, h, http.MethodPut, "/dav/album/one.txt", "one")
	do(t, h, http.MethodPut, "/dav/two.txt", "two")

	// Depth: 0 on a collection is the collection and not its members, which is
	// RFC 4918 9.8.3 and the one copy that moves no bytes at all.
	if got := do(t, h, "COPY", "/dav/album", "", "Destination", "/dav/shallow", "Depth", "0").Code; got != http.StatusCreated {
		t.Errorf("COPY of a collection at Depth 0 = %d, want 201", got)
	}
	if got := do(t, h, http.MethodGet, "/dav/shallow/one.txt", "").Code; got != http.StatusNotFound {
		t.Errorf("Depth 0 copied a member: /dav/shallow/one.txt = %d, want 404", got)
	}
	// A destination that does not exist yet is created, which is 201.
	if got := do(t, h, "COPY", "/dav/two.txt", "", "Destination", "/dav/three.txt").Code; got != http.StatusCreated {
		t.Errorf("COPY to a new path = %d, want 201", got)
	}
	// Over something that does exist it is 204, and the bytes are the source's.
	if got := do(t, h, "COPY", "/dav/album/one.txt", "", "Destination", "/dav/three.txt").Code; got != http.StatusNoContent {
		t.Errorf("COPY over an existing file = %d, want 204", got)
	}
	if got := do(t, h, http.MethodGet, "/dav/three.txt", "").Body.String(); got != "one" {
		t.Errorf("the copy reads %q, want the source", got)
	}
	// And Overwrite: F refuses rather than replacing.
	if got := do(t, h, "COPY", "/dav/two.txt", "", "Destination", "/dav/three.txt", "Overwrite", "F").Code; got != http.StatusPreconditionFailed {
		t.Errorf("COPY with Overwrite: F onto an existing file = %d, want 412", got)
	}
	if got := do(t, h, http.MethodGet, "/dav/three.txt", "").Body.String(); got != "one" {
		t.Errorf("the refused COPY wrote anyway: %q", got)
	}
	// The source is still there, which is the whole difference from MOVE.
	if got := do(t, h, http.MethodGet, "/dav/album/one.txt", "").Code; got != http.StatusOK {
		t.Error("COPY removed the source")
	}
}

// TestMoveOntoACollection is a destructive operation the specification
// requires: RFC 4918 9.9.3 says a MOVE with overwrite allowed performs a DELETE
// with infinite depth on the destination first. So the collection goes, and
// what this pins is that it goes *whole* -- a row left under a path that is now
// a file would be a subtree nothing can reach and nothing can delete.
func TestMoveOntoACollection(t *testing.T) {
	t.Parallel()
	h := server(t)
	do(t, h, "MKCOL", "/dav/album", "")
	do(t, h, http.MethodPut, "/dav/album/one.txt", "one")
	do(t, h, http.MethodPut, "/dav/two.txt", "two")

	// Overwrite: F is how a client says it did not mean that.
	if got := do(t, h, "MOVE", "/dav/two.txt", "", "Destination", "/dav/album", "Overwrite", "F").Code; got != http.StatusPreconditionFailed {
		t.Errorf("MOVE with Overwrite: F onto a collection = %d, want 412", got)
	}
	if got := do(t, h, http.MethodGet, "/dav/album/one.txt", "").Code; got != http.StatusOK {
		t.Fatal("the refused MOVE deleted the collection anyway")
	}

	if got := do(t, h, "MOVE", "/dav/two.txt", "", "Destination", "/dav/album").Code; got != http.StatusNoContent {
		t.Fatalf("MOVE onto a collection = %d, want 204", got)
	}
	if got := do(t, h, http.MethodGet, "/dav/album", "").Body.String(); got != "two" {
		t.Errorf("the destination reads %q, want the moved file", got)
	}
	// And nothing is left underneath a path that is now a file.
	if got := do(t, h, http.MethodGet, "/dav/album/one.txt", "").Code; got != http.StatusNotFound {
		t.Errorf("a row survived under the replaced collection: GET = %d", got)
	}
}

// TestCopyOutsideTheCollection is destPath's other half, asserted for COPY
// because MOVE already has it: a destination on somebody else's server is 502
// rather than a path we quietly rewrite.
func TestCopyOutsideTheCollection(t *testing.T) {
	t.Parallel()
	h := server(t)
	do(t, h, http.MethodPut, "/dav/one.txt", "one")

	if got := do(t, h, "COPY", "/dav/one.txt", "", "Destination", "/elsewhere/one.txt").Code; got != http.StatusBadGateway {
		t.Errorf("COPY outside the collection = %d, want 502", got)
	}
}

// TestLockDepth is what a client reads back to know what it holds: LOCK defaults
// to infinite depth and only "0" means this resource alone. Answering the wrong
// one is not a lie about a lock we keep -- we keep none -- but it is a lie about
// the answer, and a client that parses it deserves the truth.
func TestLockDepth(t *testing.T) {
	t.Parallel()
	h := server(t)
	do(t, h, http.MethodPut, "/dav/notes.txt", "one")

	const body = `<?xml version="1.0"?><D:lockinfo xmlns:D="DAV:">` +
		`<D:lockscope><D:exclusive/></D:lockscope><D:locktype><D:write/></D:locktype></D:lockinfo>`

	deep := do(t, h, "LOCK", "/dav/notes.txt", body, "Content-Type", "application/xml")
	if !strings.Contains(deep.Body.String(), "<D:depth>infinity</D:depth>") {
		t.Errorf("LOCK with no Depth header answered %s", deep.Body.String())
	}
	shallow := do(t, h, "LOCK", "/dav/notes.txt", body, "Content-Type", "application/xml", "Depth", "0")
	if !strings.Contains(shallow.Body.String(), "<D:depth>0</D:depth>") {
		t.Errorf("LOCK with Depth: 0 answered %s", shallow.Body.String())
	}
}

// TestLockOfANameThatNeedsEscaping: the lock root is a URL written into XML by
// hand, so a name with an ampersand in it is where that goes wrong -- and a
// malformed document is worse than no lock at all, because a client cannot
// parse its way out of it.
func TestLockOfANameThatNeedsEscaping(t *testing.T) {
	t.Parallel()
	h := server(t)
	// Percent-encoded because a raw space is not a request target; the
	// ampersand is legal in a path and is the character under test.
	const target = "/dav/rock%20&%20roll.txt"
	do(t, h, http.MethodPut, target, "one")

	const body = `<?xml version="1.0"?><D:lockinfo xmlns:D="DAV:">` +
		`<D:lockscope><D:exclusive/></D:lockscope><D:locktype><D:write/></D:locktype></D:lockinfo>`
	rec := do(t, h, "LOCK", target, body, "Content-Type", "application/xml")

	if rec.Code != http.StatusOK {
		t.Fatalf("LOCK = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "roll & rock") || strings.Contains(rec.Body.String(), "& roll") {
		t.Errorf("the ampersand reached the document unescaped: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "&amp;") {
		t.Errorf("the lock root is not escaped: %s", rec.Body.String())
	}
}

// TestContentTypeIgnoresTheCaseOfTheExtension, because a camera writes IMG.JPG
// and a distroless image has no table to fall back on.
func TestContentTypeIgnoresTheCaseOfTheExtension(t *testing.T) {
	t.Parallel()
	h := server(t)
	do(t, h, http.MethodPut, "/dav/IMG_0001.HEIC", "not really a heic")

	if got := do(t, h, http.MethodGet, "/dav/IMG_0001.HEIC", "").Header().Get("Content-Type"); got != "image/heic" {
		t.Errorf("Content-Type = %q, want image/heic", got)
	}
}
