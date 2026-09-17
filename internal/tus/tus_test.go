package tus_test

import (
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/C0piIot/stratus-backend/internal/auth"
	"github.com/C0piIot/stratus-backend/internal/db/sqlite"
	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/storage/disk"
	"github.com/C0piIot/stratus-backend/internal/tus"
)

const (
	prefix = "/tus/"
	owner  = "edu"
)

// server drives the real handler over the real backends, for the same reason
// internal/dav does: an adapter tested against fakes tests the fakes.
func server(t *testing.T) (http.Handler, *files.Service) {
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

	service := files.New(blobs, meta)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tus.Handler(prefix, service).ServeHTTP(w, r.WithContext(auth.WithUser(r.Context(), owner)))
	})
	return h, service
}

// do sends a request with the protocol header already on it, which every
// request but OPTIONS needs.
func do(t *testing.T, h http.Handler, method, target, body string, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequestWithContext(t.Context(), method, target, reader)
	req.Header.Set("Tus-Resumable", tus.Version)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func metadata(path, kind string) string {
	enc := base64.StdEncoding
	return "filename " + enc.EncodeToString([]byte(path)) + ",filetype " + enc.EncodeToString([]byte(kind))
}

// TestUploadInChunks is the protocol doing the thing it exists for: a file that
// arrives in pieces, with the server able to say where it got to in between.
func TestUploadInChunks(t *testing.T) {
	t.Parallel()
	h, service := server(t)
	if _, err := service.Mkdir(t.Context(), owner, "holiday"); err != nil {
		t.Fatal(err)
	}
	const body = "a video, allegedly, in two halves"
	half := len(body) / 2

	rec := do(t, h, http.MethodPost, prefix, "",
		"Upload-Length", strconv.Itoa(len(body)), "Upload-Metadata", metadata("holiday/clip.mp4", "video/mp4"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST = %d, want 201: %s", rec.Code, rec.Body)
	}
	location := rec.Header().Get("Location")
	if !strings.HasPrefix(location, prefix) {
		t.Fatalf("Location = %q, want an upload under %q", location, prefix)
	}
	if rec.Header().Get("Upload-Expires") == "" {
		t.Error("a created upload does not say when it expires")
	}

	// Where to start, which is the question the whole protocol is built around.
	rec = do(t, h, http.MethodHead, location, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Upload-Offset"); got != "0" {
		t.Errorf("Upload-Offset = %q, want 0", got)
	}
	if got := rec.Header().Get("Upload-Length"); got != strconv.Itoa(len(body)) {
		t.Errorf("Upload-Length = %q, want %d", got, len(body))
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q: a cached offset is a client writing over itself", got)
	}

	rec = do(t, h, http.MethodPatch, location, body[:half],
		"Content-Type", "application/offset+octet-stream", "Upload-Offset", "0")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("PATCH = %d, want 204: %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Upload-Offset"); got != strconv.Itoa(half) {
		t.Fatalf("Upload-Offset after the first chunk = %q, want %d", got, half)
	}

	rec = do(t, h, http.MethodPatch, location, body[half:],
		"Content-Type", "application/offset+octet-stream", "Upload-Offset", strconv.Itoa(half))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("PATCH = %d, want 204: %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Upload-Offset"); got != strconv.Itoa(len(body)) {
		t.Errorf("Upload-Offset after the last chunk = %q, want %d", got, len(body))
	}

	// The protocol has no "finish": the file appears when the upload is as long
	// as it said it would be.
	f, err := service.Stat(t.Context(), owner, "holiday/clip.mp4")
	if err != nil {
		t.Fatalf("the file is not there after the last chunk: %v", err)
	}
	if f.Size != int64(len(body)) {
		t.Errorf("Size = %d, want %d", f.Size, len(body))
	}
	if f.MIMEType != "video/mp4" {
		t.Errorf("MIMEType = %q, want the one the metadata carried", f.MIMEType)
	}
	// And the upload is over, so its URL is not there any more.
	if code := do(t, h, http.MethodHead, location, "").Code; code != http.StatusNotFound {
		t.Errorf("HEAD after completion = %d, want 404", code)
	}
}

// TestOptionsAdvertises is how a client finds out what it may send, and the one
// request that needs no version header.
func TestOptionsAdvertises(t *testing.T) {
	t.Parallel()
	h, _ := server(t)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodOptions, prefix, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("OPTIONS = %d, want 204", rec.Code)
	}
	for header, want := range map[string]string{
		"Tus-Resumable": tus.Version,
		"Tus-Version":   tus.Version,
		"Tus-Extension": "creation,expiration,termination",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
}

// TestVersionIsChecked: a client speaking something else has to be told, or it
// will misread every answer it gets.
func TestVersionIsChecked(t *testing.T) {
	t.Parallel()
	h, _ := server(t)

	for _, version := range []string{"", "0.2.2"} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, prefix, nil)
		if version != "" {
			req.Header.Set("Tus-Resumable", version)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusPreconditionFailed {
			t.Errorf("POST with Tus-Resumable %q = %d, want 412", version, rec.Code)
		}
		if got := rec.Header().Get("Tus-Version"); got != tus.Version {
			t.Errorf("Tus-Version = %q, want %q", got, tus.Version)
		}
	}
}

// TestPatchRefusesTheWrongOffset is the case a retry produces, and answering it
// with anything but a 409 would write the same bytes twice.
func TestPatchRefusesTheWrongOffset(t *testing.T) {
	t.Parallel()
	h, _ := server(t)

	location := create(t, h, "notes.txt", 10)
	if code := do(t, h, http.MethodPatch, location, "12345",
		"Content-Type", "application/offset+octet-stream", "Upload-Offset", "0").Code; code != http.StatusNoContent {
		t.Fatalf("the first chunk = %d, want 204", code)
	}

	rec := do(t, h, http.MethodPatch, location, "12345",
		"Content-Type", "application/offset+octet-stream", "Upload-Offset", "0")
	if rec.Code != http.StatusConflict {
		t.Errorf("a repeated chunk = %d, want 409", rec.Code)
	}
}

// TestPatchNeedsItsMediaType, which is what keeps an accidental form post from
// being mistaken for a chunk.
func TestPatchNeedsItsMediaType(t *testing.T) {
	t.Parallel()
	h, _ := server(t)
	location := create(t, h, "notes.txt", 10)

	rec := do(t, h, http.MethodPatch, location, "12345",
		"Content-Type", "text/plain", "Upload-Offset", "0")
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("PATCH with the wrong media type = %d, want 415", rec.Code)
	}

	rec = do(t, h, http.MethodPatch, location, "12345",
		"Content-Type", "application/offset+octet-stream")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("PATCH with no Upload-Offset = %d, want 400", rec.Code)
	}
}

// TestCreateIsChecked covers what a malformed creation looks like, because the
// alternative is an upload that fails an hour later.
func TestCreateIsChecked(t *testing.T) {
	t.Parallel()
	h, _ := server(t)

	tests := []struct {
		name    string
		headers []string
		want    int
	}{
		{"no length", []string{"Upload-Metadata", metadata("notes.txt", "")}, http.StatusBadRequest},
		{"a length that is not a number", []string{"Upload-Length", "soon", "Upload-Metadata", metadata("notes.txt", "")}, http.StatusBadRequest},
		{"a negative length", []string{"Upload-Length", "-1", "Upload-Metadata", metadata("notes.txt", "")}, http.StatusBadRequest},
		{"no metadata at all", []string{"Upload-Length", "10"}, http.StatusBadRequest},
		{"metadata with no filename", []string{"Upload-Length", "10", "Upload-Metadata", "filetype " + base64.StdEncoding.EncodeToString([]byte("text/plain"))}, http.StatusBadRequest},
		{"a filename that is not a path", []string{"Upload-Length", "10", "Upload-Metadata", metadata("../escape", "")}, http.StatusBadRequest},
		{"a directory that is not there", []string{"Upload-Length", "10", "Upload-Metadata", metadata("missing/notes.txt", "")}, http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if code := do(t, h, http.MethodPost, prefix, "", tt.headers...).Code; code != tt.want {
				t.Errorf("POST = %d, want %d", code, tt.want)
			}
		})
	}
}

// TestTerminate is the client saying it is not coming back.
func TestTerminate(t *testing.T) {
	t.Parallel()
	h, _ := server(t)
	location := create(t, h, "notes.txt", 10)

	if code := do(t, h, http.MethodDelete, location, "").Code; code != http.StatusNoContent {
		t.Fatalf("DELETE = %d, want 204", code)
	}
	if code := do(t, h, http.MethodHead, location, "").Code; code != http.StatusNotFound {
		t.Errorf("HEAD after DELETE = %d, want 404", code)
	}
	// A second one has nothing to terminate, and says so rather than pretending.
	if code := do(t, h, http.MethodDelete, location, "").Code; code != http.StatusNotFound {
		t.Errorf("a second DELETE = %d, want 404", code)
	}
}

// TestUnknownUpload: every verb on an upload takes its id from a client.
func TestUnknownUpload(t *testing.T) {
	t.Parallel()
	h, _ := server(t)
	gone := prefix + "an-upload-that-never-was"

	for _, method := range []string{http.MethodHead, http.MethodDelete} {
		if code := do(t, h, method, gone, "").Code; code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", method, code)
		}
	}
	if code := do(t, h, http.MethodPatch, gone, "x",
		"Content-Type", "application/offset+octet-stream", "Upload-Offset", "0").Code; code != http.StatusNotFound {
		t.Errorf("PATCH = %d, want 404", code)
	}
}

// TestMethodsAreRefused covers the two endpoints separately, because what is
// allowed on the collection is not what is allowed on an upload.
func TestMethodsAreRefused(t *testing.T) {
	t.Parallel()
	h, _ := server(t)

	rec := do(t, h, http.MethodGet, prefix, "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET on the creation endpoint = %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); !strings.Contains(got, "POST") {
		t.Errorf("Allow = %q, want it to name POST", got)
	}

	rec = do(t, h, http.MethodPost, prefix+"an-id", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST on an upload = %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); !strings.Contains(got, "PATCH") {
		t.Errorf("Allow = %q, want it to name PATCH", got)
	}
}

// TestWithoutAnAuthenticatedUser: the handler is mounted behind Basic auth, and
// this is what it does if that ever stops being true.
func TestWithoutAnAuthenticatedUser(t *testing.T) {
	t.Parallel()
	_, service := server(t)
	bare := tus.Handler(prefix, service)

	for _, method := range []string{http.MethodPost, http.MethodHead, http.MethodPatch, http.MethodDelete} {
		// POST is the creation endpoint and the rest name an upload, so the two
		// go to different URLs or they would be refused as the wrong method
		// before anything asked who was calling.
		target := prefix + "an-id"
		if method == http.MethodPost {
			target = prefix
		}
		req := httptest.NewRequestWithContext(t.Context(), method, target, nil)
		req.Header.Set("Tus-Resumable", tus.Version)
		req.Header.Set("Content-Type", "application/offset+octet-stream")
		req.Header.Set("Upload-Offset", "0")
		rec := httptest.NewRecorder()
		bare.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s with no user = %d, want 401", method, rec.Code)
		}
	}
}

// create starts an upload and returns its URL, for the cases that are about
// what happens next.
func create(t *testing.T, h http.Handler, path string, length int64) string {
	t.Helper()
	rec := do(t, h, http.MethodPost, prefix, "",
		"Upload-Length", strconv.FormatInt(length, 10), "Upload-Metadata", metadata(path, "text/plain"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST = %d, want 201: %s", rec.Code, rec.Body)
	}
	return rec.Header().Get("Location")
}
