package dav_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

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
	return dav.Handler(prefix, files.New(blobs, meta), "edu")
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

func TestPropfind(t *testing.T) {
	t.Parallel()
	h := server(t)
	do(t, h, "MKCOL", "/dav/album", "")
	do(t, h, http.MethodPut, "/dav/album/one.txt", "one")
	do(t, h, http.MethodPut, "/dav/album/two.txt", "two")

	rec := do(t, h, "PROPFIND", "/dav/album", "", "Depth", "1")
	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("PROPFIND = %d, want 207", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"/dav/album/one.txt", "/dav/album/two.txt"} {
		if !strings.Contains(body, want) {
			t.Errorf("the listing does not mention %s:\n%s", want, body)
		}
	}
}

func TestPropfindInfiniteDepth(t *testing.T) {
	t.Parallel()
	h := server(t)
	do(t, h, "MKCOL", "/dav/album", "")
	do(t, h, "MKCOL", "/dav/album/raw", "")
	do(t, h, http.MethodPut, "/dav/album/raw/deep.txt", "deep")

	rec := do(t, h, "PROPFIND", "/dav/", "", "Depth", "infinity")
	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("PROPFIND = %d, want 207", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "/dav/album/raw/deep.txt") {
		t.Errorf("an infinite-depth listing missed the deepest file:\n%s", rec.Body.String())
	}
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

	do(t, h, "MKCOL", "/dav/album", "")
	if got := do(t, h, "COPY", "/dav/album", "", "Destination", "/dav/copy").Code; got != http.StatusNotImplemented {
		t.Errorf("COPY of a collection = %d, want 501 until it is implemented", got)
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
