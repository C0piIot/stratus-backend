package web_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/C0piIot/stratus-backend/internal/db/dbtest"
	"github.com/C0piIot/stratus-backend/internal/files"
)

func mkdir(t *testing.T, s *files.Service, path string) {
	t.Helper()
	if _, err := s.Mkdir(t.Context(), username, path); err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, s *files.Service, path, body string) {
	t.Helper()
	_, err := s.Write(t.Context(), username, path, strings.NewReader(body), int64(len(body)), "text/plain")
	if err != nil {
		t.Fatal(err)
	}
}

// TestListing is the page itself: what is in the directory, directories first,
// each linking to its own URL.
func TestListing(t *testing.T) {
	t.Parallel()
	h, s := browser(t)
	// Deliberately in an order that is neither the answer nor the reverse of
	// it: the listing is sorted, not echoed.
	write(t, s, "notes.txt", "hello")
	mkdir(t, s, "photos")
	write(t, s, "archive.bin", strings.Repeat("x", 2048))
	mkdir(t, s, "music")

	body := get(t, h, "/files/", signIn(t, h)).Body.String()

	var order []string
	for _, name := range []string{"music", "photos", "archive.bin", "notes.txt"} {
		i := strings.Index(body, ">"+name)
		if i < 0 {
			t.Fatalf("%q is not in the listing", name)
		}
		order = append(order, name)
		if !strings.Contains(body, `href="/files/`+name+`"`) {
			t.Errorf("%q does not link to its own URL", name)
		}
	}
	if got := indexes(body, order); !sorted(got) {
		t.Errorf("the listing is in the wrong order: directories come first, then names (%v)", order)
	}

	// A directory is marked as one, and a file carries what a person wants to
	// know about it.
	if !strings.Contains(body, ">photos/<") {
		t.Error("a directory is not shown as one")
	}
	if !strings.Contains(body, "2.0 KB") {
		t.Error("the size is not there, or not in bytes a person reads")
	}
}

func indexes(body string, names []string) []int {
	out := make([]int, 0, len(names))
	for _, n := range names {
		out = append(out, strings.Index(body, ">"+n))
	}
	return out
}

func sorted(xs []int) bool {
	for i := 1; i < len(xs); i++ {
		if xs[i] < xs[i-1] {
			return false
		}
	}
	return true
}

// TestListingDeeper: the trail back is the only navigation there is, so it is
// the part of the page worth asserting.
func TestListingDeeper(t *testing.T) {
	t.Parallel()
	h, s := browser(t)
	mkdir(t, s, "photos")
	mkdir(t, s, "photos/2026")
	write(t, s, "photos/2026/img.jpg", "not really a jpeg")

	body := get(t, h, "/files/photos/2026", signIn(t, h)).Body.String()

	for _, want := range []string{`href="/files/"`, `href="/files/photos"`, ">2026<", ">img.jpg<"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page is missing %s", want)
		}
	}
	// The directory being listed is where you are, not somewhere to go.
	if strings.Contains(body, `href="/files/photos/2026"`) {
		t.Error("the current directory links to itself")
	}
}

func TestListingWhenThereIsNothing(t *testing.T) {
	t.Parallel()
	h, _ := browser(t)

	rec := get(t, h, "/files/", signIn(t, h))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /files/ on an empty server = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "empty") {
		t.Error("an empty directory says nothing about being empty")
	}
}

// TestDownload: opening a file hands over the bytes, as an attachment rather
// than as something this origin renders.
func TestDownload(t *testing.T) {
	t.Parallel()
	h, s := browser(t)
	const body = "the bytes that were stored"
	write(t, s, "notes.txt", body)

	rec := get(t, h, "/files/notes.txt", signIn(t, h))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET a file = %d, want 200", rec.Code)
	}
	if rec.Body.String() != body {
		t.Errorf("body = %q, want %q", rec.Body.String(), body)
	}

	head := rec.Header()
	if got := head.Get("Content-Disposition"); got != `attachment; filename=notes.txt` {
		t.Errorf("Content-Disposition = %q", got)
	}
	if got := head.Get("Content-Type"); got != "text/plain" {
		t.Errorf("Content-Type = %q, want what was stored with it", got)
	}
	if got := head.Get("ETag"); got != `"`+strings.Trim(head.Get("ETag"), `"`)+`"` || got == `""` {
		t.Errorf("ETag = %q, want the stored validator in quotes", got)
	}
	if got := head.Get("Cache-Control"); !strings.Contains(got, "private") {
		t.Errorf("Cache-Control = %q, want it private to this user", got)
	}
}

// TestDownloadIsConditionalAndRangeable: none of this is written here -- it is
// http.ServeContent over the seeker internal/files returns -- which is exactly
// why it is worth one test that it is actually reached.
func TestDownloadIsConditionalAndRangeable(t *testing.T) {
	t.Parallel()
	h, s := browser(t)
	write(t, s, "notes.txt", "0123456789")
	cookie := signIn(t, h)

	etag := get(t, h, "/files/notes.txt", cookie).Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag to be conditional about")
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/files/notes.txt", nil)
	req.AddCookie(cookie)
	req.Header.Set("If-None-Match", etag)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotModified {
		t.Errorf("a conditional request = %d, want 304", rec.Code)
	}

	req = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/files/notes.txt", nil)
	req.AddCookie(cookie)
	req.Header.Set("Range", "bytes=2-5")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("a range request = %d, want 206", rec.Code)
	}
	if got := rec.Body.String(); got != "2345" {
		t.Errorf("the range = %q, want 2345", got)
	}
}

// TestNamesThatNeedEscaping goes there and back: the listing has to produce a
// URL a browser can follow, for a name that is not a tidy one.
func TestNamesThatNeedEscaping(t *testing.T) {
	t.Parallel()
	h, s := browser(t)
	const name = "a file & a <name>.txt"
	mkdir(t, s, "odd names")
	write(t, s, "odd names/"+name, "hello")
	cookie := signIn(t, h)

	body := get(t, h, "/files/odd%20names", cookie).Body.String()
	if strings.Contains(body, "<name>") {
		t.Error("the name went into the page unescaped")
	}
	const want = `href="/files/odd%20names/a%20file%20&amp;%20a%20%3Cname%3E.txt"`
	if !strings.Contains(body, want) {
		t.Fatalf("the link is not what a browser can follow:\n%s", excerpt(body, "href=\"/files/odd"))
	}

	rec := get(t, h, "/files/odd%20names/a%20file%20&%20a%20%3cname%3e.txt", cookie)
	if rec.Code != http.StatusOK || rec.Body.String() != "hello" {
		t.Errorf("following that link = %d %q", rec.Code, rec.Body)
	}
	// The filename travels in a header, where quoting is the protocol's problem
	// and not the browser's.
	if got := rec.Header().Get("Content-Disposition"); !strings.Contains(got, `"a file & a <name>.txt"`) {
		t.Errorf("Content-Disposition = %q", got)
	}
}

func excerpt(body, around string) string {
	i := strings.Index(body, around)
	if i < 0 {
		return "(not found)"
	}
	return body[i:min(i+120, len(body))]
}

// TestPathsThatGoNowhere: the URL is a path in somebody's tree, and the only
// implementation of what that may be is db.ValidatePath -- the same one WebDAV
// goes through.
func TestPathsThatGoNowhere(t *testing.T) {
	t.Parallel()
	h, s := browser(t)
	write(t, s, "notes.txt", "hello")
	cookie := signIn(t, h)

	tests := []struct {
		name   string
		target string
		want   int
	}{
		{name: "nothing there", target: "/files/nope", want: http.StatusNotFound},
		{name: "nothing there, deeper", target: "/files/no/such/place", want: http.StatusNotFound},
		{name: "a control character", target: "/files/a%01b", want: http.StatusBadRequest},
		{name: "a file used as a directory", target: "/files/notes.txt/deeper", want: http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := get(t, h, tt.target, cookie)
			if rec.Code != tt.want {
				t.Errorf("GET %s = %d, want %d", tt.target, rec.Code, tt.want)
			}
			if strings.Contains(rec.Body.String(), "root:") {
				t.Error("something outside the tree came back")
			}
		})
	}
}

// TestClimbingOutOfTheTree: the dots never reach a lookup. net/http cleans a
// plain ".." itself and answers a redirect to the tidied path, and toPath
// cleans an encoded one before internal/files sees it -- so both end up asking
// for something that is simply not there, rather than for somebody's /etc.
func TestClimbingOutOfTheTree(t *testing.T) {
	t.Parallel()
	h, s := browser(t)
	write(t, s, "notes.txt", "hello")
	cookie := signIn(t, h)

	t.Run("plain dots, tidied by the server", func(t *testing.T) {
		t.Parallel()
		rec := get(t, h, "/files/../../etc/passwd", cookie)
		if rec.Code != http.StatusTemporaryRedirect {
			t.Fatalf("GET with dots = %d, want the redirect net/http answers", rec.Code)
		}
		// Somewhere on this server, which is then a 404 like any other.
		if got := rec.Header().Get("Location"); got != "/etc/passwd" {
			t.Errorf("Location = %q", got)
		}
		if code := get(t, h, "/etc/passwd", cookie).Code; code != http.StatusNotFound {
			t.Errorf("following it = %d, want 404", code)
		}
	})

	t.Run("encoded dots, tidied by us", func(t *testing.T) {
		t.Parallel()
		rec := get(t, h, "/files/%2e%2e/%2e%2e/etc/passwd", cookie)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET with encoded dots = %d, want 404", rec.Code)
		}
		if strings.Contains(rec.Body.String(), "root:") {
			t.Error("something outside the tree came back")
		}
	})
}

// TestABackendThatWillNotAnswer is the arm of the error page nothing else
// reaches: not a missing file and not a bad path, but the database refusing.
// It has to be a page rather than a blank 500, and it must not put the reason
// on it -- that is what the log is for.
func TestABackendThatWillNotAnswer(t *testing.T) {
	t.Parallel()
	blobs, meta := backends(t)
	h := handlerOver(t, files.New(blobs, dbtest.FailOn(t, meta, "ListFilesPage")), blobs)

	rec := get(t, h, "/files/", signIn(t, h))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("GET /files/ over a database that refuses = %d, want 500", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Something went wrong") {
		t.Error("the failure is not a page")
	}
	if strings.Contains(body, dbtest.ErrInjected.Error()) {
		t.Error("the page carries the error, which belongs in the log")
	}
}
