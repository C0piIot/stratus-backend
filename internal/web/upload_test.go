package web_test

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/storage/storagetest"
)

// form builds a multipart body the way a browser does: one part per chosen
// file, each with its name and type.
func form(t *testing.T, parts ...string) (body io.Reader, contentType string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for i := 0; i+1 < len(parts); i += 2 {
		part, err := w.CreateFormFile("file", parts[i])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(part, parts[i+1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, w.FormDataContentType()
}

func upload(t *testing.T, h http.Handler, target string, cookie *http.Cookie, parts ...string) *httptest.ResponseRecorder {
	t.Helper()
	body, contentType := form(t, parts...)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, target, body)
	req.Header.Set("Content-Type", contentType)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// stored is what the tree holds at path, read back through the same service the
// upload wrote through.
func stored(t *testing.T, s *files.Service, path string) string {
	t.Helper()
	body, _, err := s.Open(t.Context(), username, path)
	if err != nil {
		t.Fatalf("reading %q back: %v", path, err)
	}
	defer func() { _ = body.Close() }()
	out, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestUpload(t *testing.T) {
	t.Parallel()
	h, s := browser(t)
	cookie := signIn(t, h)

	rec := upload(t, h, "/files/", cookie, "notes.txt", "hello")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST an upload = %d, want 303: a reload must not send it twice", rec.Code)
	}
	// Back to the listing it came from, and with something to say about it.
	if got := rec.Header().Get("Location"); got != "/files/?added=1" {
		t.Errorf("Location = %q", got)
	}
	if got := stored(t, s, "notes.txt"); got != "hello" {
		t.Errorf("the file holds %q", got)
	}

	body := get(t, h, "/files/?added=1", cookie).Body.String()
	if !strings.Contains(body, "1 file uploaded") {
		t.Error("the listing says nothing about the upload that just happened")
	}
	if !strings.Contains(body, ">notes.txt<") {
		t.Error("the file is not in the listing")
	}
}

func TestUploadSeveralAtOnce(t *testing.T) {
	t.Parallel()
	h, s := browser(t)
	cookie := signIn(t, h)

	rec := upload(t, h, "/files/", cookie, "one.txt", "1", "two.txt", "2", "three.txt", "3")
	if got := rec.Header().Get("Location"); got != "/files/?added=3" {
		t.Fatalf("Location = %q, want all three counted", got)
	}
	for name, want := range map[string]string{"one.txt": "1", "two.txt": "2", "three.txt": "3"} {
		if got := stored(t, s, name); got != want {
			t.Errorf("%s holds %q, want %q", name, got, want)
		}
	}
}

func TestUploadIntoADirectory(t *testing.T) {
	t.Parallel()
	h, s := browser(t)
	mkdir(t, s, "photos")
	cookie := signIn(t, h)

	rec := upload(t, h, "/files/photos", cookie, "img.jpg", "not really a jpeg")
	if got := rec.Header().Get("Location"); got != "/files/photos?added=1" {
		t.Fatalf("Location = %q, want back to the directory it was sent to", got)
	}
	if got := stored(t, s, "photos/img.jpg"); got != "not really a jpeg" {
		t.Errorf("photos/img.jpg holds %q", got)
	}
}

// TestUploadReplaces: the same tree behaves the same way through both doors,
// and a PUT over WebDAV replaces what was there.
func TestUploadReplaces(t *testing.T) {
	t.Parallel()
	h, s := browser(t)
	write(t, s, "notes.txt", "the old one")
	cookie := signIn(t, h)

	if rec := upload(t, h, "/files/", cookie, "notes.txt", "the new one"); rec.Code != http.StatusSeeOther {
		t.Fatalf("uploading over a file = %d", rec.Code)
	}
	if got := stored(t, s, "notes.txt"); got != "the new one" {
		t.Errorf("notes.txt holds %q, want the upload", got)
	}
}

// TestUploadKeepsTheType, because what a file is served back as is decided
// here: the browser is the only party that saw it.
func TestUploadKeepsTheType(t *testing.T) {
	t.Parallel()
	h, _ := browser(t)
	cookie := signIn(t, h)

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	head := make(map[string][]string)
	head["Content-Disposition"] = []string{`form-data; name="file"; filename="chart.svg"`}
	head["Content-Type"] = []string{"image/svg+xml"}
	part, err := w.CreatePart(head)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(part, "<svg/>"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/files/", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("uploading = %d", rec.Code)
	}

	if got := get(t, h, "/files/chart.svg", cookie).Header().Get("Content-Type"); got != "image/svg+xml" {
		t.Errorf("served back as %q, want what was declared", got)
	}
}

// TestUploadNamesThatAreNotNames: the filename comes from the client, so it is
// a name at most -- never a path, and never a way out of the directory the form
// was shown in.
func TestUploadNamesThatAreNotNames(t *testing.T) {
	t.Parallel()
	h, s := browser(t)
	mkdir(t, s, "photos")
	cookie := signIn(t, h)

	tests := []struct {
		name string
		sent string
		want string // where it must land, or "" when it must be refused
		code int
		// why, for the two that are refused: the same status comes from two
		// different guards, and which one fired is the point.
		says string
	}{
		{name: "a plain name", sent: "img.jpg", want: "photos/img.jpg", code: http.StatusSeeOther},
		{
			name: "a relative path, as a directory upload sends",
			sent: "holiday/img.jpg", want: "photos/img.jpg", code: http.StatusSeeOther,
		},
		{
			name: "a windows path", sent: `C:\Users\edu\img.jpg`,
			want: "photos/img.jpg", code: http.StatusSeeOther,
		},
		{name: "climbing out", sent: "../../img.jpg", want: "photos/img.jpg", code: http.StatusSeeOther},
		{
			name: "nothing but dots", sent: "..",
			code: http.StatusBadRequest, says: "not a path this server can answer",
		},
		{
			name: "not valid UTF-8", sent: "img\xff.jpg",
			code: http.StatusBadRequest, says: "not a path this server can answer",
		},
		{
			// A MIME header cannot carry one, so this never reaches the tree:
			// the body itself stops being readable.
			name: "a control character", sent: "img\x01.jpg",
			code: http.StatusBadRequest, says: "did not arrive whole",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := upload(t, h, "/files/photos", cookie, tt.sent, "pixels")
			if rec.Code != tt.code {
				t.Fatalf("uploading %q = %d, want %d", tt.sent, rec.Code, tt.code)
			}
			if tt.want == "" {
				if !strings.Contains(rec.Body.String(), tt.says) {
					t.Errorf("refused %q, but not for the reason expected: want %q", tt.sent, tt.says)
				}
				return
			}
			if _, err := s.Stat(t.Context(), username, tt.want); err != nil {
				t.Errorf("%q did not land at %q: %v", tt.sent, tt.want, err)
			}
		})
	}
}

func TestUploadRefuses(t *testing.T) {
	t.Parallel()
	h, s := browser(t)
	write(t, s, "notes.txt", "hello")
	cookie := signIn(t, h)

	t.Run("into a file", func(t *testing.T) {
		t.Parallel()
		rec := upload(t, h, "/files/notes.txt", cookie, "img.jpg", "pixels")
		if rec.Code != http.StatusConflict {
			t.Errorf("uploading into a file = %d, want 409", rec.Code)
		}
	})

	t.Run("into nothing", func(t *testing.T) {
		t.Parallel()
		rec := upload(t, h, "/files/no/such/place", cookie, "img.jpg", "pixels")
		if rec.Code != http.StatusNotFound {
			t.Errorf("uploading into a directory that is not there = %d, want 404", rec.Code)
		}
	})

	t.Run("something that is not an upload", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/files/",
			strings.NewReader("username=edu"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("posting a form that is not multipart = %d, want 400", rec.Code)
		}
	})

	t.Run("with no session", func(t *testing.T) {
		t.Parallel()
		body, contentType := form(t, "img.jpg", "pixels")
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/files/", body)
		req.Header.Set("Content-Type", contentType)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/login") {
			t.Errorf("uploading with no session = %d to %q, want the login form",
				rec.Code, rec.Header().Get("Location"))
		}
	})

	t.Run("from somebody else's page", func(t *testing.T) {
		t.Parallel()
		body, contentType := form(t, "img.jpg", "pixels")
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/files/", body)
		req.Header.Set("Content-Type", contentType)
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("a cross-site upload = %d, want 403", rec.Code)
		}
	})
}

// TestUploadWhenTheStoreRefuses: the blob is written before the row, so a store
// that says no has to stop the upload rather than leave a name in the tree with
// nothing behind it. The page says so, and says nothing about why.
func TestUploadWhenTheStoreRefuses(t *testing.T) {
	t.Parallel()
	blobs, meta := backends(t)
	broken := files.New(storagetest.FailOn(t, blobs, "Put"), meta)
	h := handlerOver(t, broken, blobs)
	cookie := signIn(t, h)

	rec := upload(t, h, "/files/", cookie, "notes.txt", "hello")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("uploading into a store that refuses = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), storagetest.ErrInjected.Error()) {
		t.Error("the page carries the error, which belongs in the log")
	}
	// Nothing half-written: no row, so nothing above this package can see it.
	working := files.New(blobs, meta)
	if _, err := working.Stat(t.Context(), username, "notes.txt"); err == nil {
		t.Error("a file exists after an upload that never landed")
	}
}

// TestUploadIntoAPathThatIsNotOne: the directory in the URL is validated the
// same way it is for a listing -- the form posts to wherever it was shown.
func TestUploadIntoAPathThatIsNotOne(t *testing.T) {
	t.Parallel()
	h, _ := browser(t)

	rec := upload(t, h, "/files/a%01b", signIn(t, h), "img.jpg", "pixels")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("uploading into a path that is not one = %d, want 400", rec.Code)
	}
}

// TestUploadIgnoresWhatIsNotAFile: a form can carry more than files, and a
// submit button with no file chosen must not become an empty one in the tree.
func TestUploadIgnoresWhatIsNotAFile(t *testing.T) {
	t.Parallel()
	h, s := browser(t)
	cookie := signIn(t, h)

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.WriteField("go", "Upload"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/files/", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Location"); got != "/files/?added=0" {
		t.Errorf("Location = %q, want nothing counted", got)
	}
	children, err := s.List(t.Context(), username, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 0 {
		t.Errorf("the tree holds %d things after an upload with no files", len(children))
	}
	// And the listing says nothing, rather than claiming zero files arrived.
	if strings.Contains(get(t, h, "/files/?added=0", cookie).Body.String(), "uploaded") {
		t.Error("the listing announced an upload that did not happen")
	}
}
