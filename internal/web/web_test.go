package web_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/auth"
	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/db/sqlite"
	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/media"
	"github.com/C0piIot/stratus-backend/internal/storage"
	"github.com/C0piIot/stratus-backend/internal/storage/disk"
	"github.com/C0piIot/stratus-backend/internal/web"
)

const (
	username = "edu"
	// "example" in the name and the value, so a secret scanner can tell a
	// fixture from a leaked credential.
	examplePassword = "example correct horse battery staple"
	version         = "1.2.3-test"
	buildDate       = "2026-01-01T09:30:00Z"
)

func credentials() auth.Credentials {
	return auth.Credentials{Username: username, Password: examplePassword}
}

// newHandler builds the UI over the real credentials and an empty tree. A nil
// verifier means those same credentials, which is what the server does.
func newHandler(t *testing.T, v auth.Verifier) http.Handler {
	t.Helper()
	creds := credentials()
	if v == nil {
		v = creds
	}
	s, thumbs, meta := pieces(t)
	return web.Handler(version, buildDate, v, auth.NewSessions(creds, auth.DefaultSessionTTL),
		s, thumbs, indexing(meta))
}

// browser is newHandler and the service behind it, for the tests that have to
// put something in the tree before they can browse it.
func browser(t *testing.T) (http.Handler, *files.Service) {
	t.Helper()
	s, thumbs, meta := pieces(t)
	creds := credentials()
	return web.Handler(version, buildDate, creds, auth.NewSessions(creds, auth.DefaultSessionTTL),
		s, thumbs, indexing(meta)), s
}

// browserOver is browser plus the store behind it, for the tests that have to
// write a media row or count one.
func browserOver(t *testing.T) (http.Handler, *files.Service, db.Store) {
	t.Helper()
	s, thumbs, meta := pieces(t)
	creds := credentials()
	return web.Handler(version, buildDate, creds, auth.NewSessions(creds, auth.DefaultSessionTTL),
		s, thumbs, indexing(meta)), s, meta
}

// handlerIndexing is handlerOver for the tests that care about what the status
// page was told rather than about what is in the tree.
func handlerIndexing(t *testing.T, s *files.Service, blobs storage.Storage, ix web.Indexing) http.Handler {
	t.Helper()
	creds := credentials()
	return web.Handler(version, buildDate, creds, auth.NewSessions(creds, auth.DefaultSessionTTL),
		s, media.NewThumbs(blobs, s), ix)
}

// service is the real file layer over real backends in a temporary directory,
// for the reason internal/dav's tests give: an adapter tested only against
// fakes tests the fakes.
// pieces is a service and the thumbnail generator over the same blob store,
// which is the part that matters: a generator built over a second store would
// never find the file it was asked to make a picture of.
func pieces(t *testing.T) (*files.Service, *media.Thumbs, db.Store) {
	t.Helper()
	blobs, meta := backends(t)
	s := files.New(blobs, meta)
	return s, media.NewThumbs(blobs, s), meta
}

// indexing is what the status page and the listing's marks read, with the
// interval the server defaults to so that a test sees the page an ordinary
// install shows.
func indexing(index db.MediaIndex) web.Indexing {
	return web.Indexing{Index: index, Interval: time.Minute}
}

// backends are those two seams on their own, for the tests that put a fault
// injector in front of one of them.
func backends(t *testing.T) (storage.Storage, db.Store) {
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
	return blobs, meta
}

// handlerOver is web.Handler over a service somebody else assembled, which is
// how a test gets a broken backend behind the pages.
func handlerOver(t *testing.T, s *files.Service, blobs storage.Storage, index db.MediaIndex) http.Handler {
	t.Helper()
	creds := credentials()
	return web.Handler(version, buildDate, creds, auth.NewSessions(creds, auth.DefaultSessionTTL),
		s, media.NewThumbs(blobs, s), indexing(index))
}

// refusing answers every login with one error, for the arms a correct password
// does not reach.
type refusing struct{ err error }

func (r refusing) Verify(context.Context, string, string) error { return r.err }

func get(t *testing.T, h http.Handler, target string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func post(t *testing.T, h http.Handler, target string, form url.Values, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, target, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// sessionCookie returns the cookie a response set, or nil.
func sessionCookie(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range (&http.Response{Header: rec.Header()}).Cookies() {
		if c.Name == "stratus_session" {
			return c
		}
	}
	return nil
}

// signIn logs in and hands back the cookie, which is what the tests of every
// page behind the login need.
func signIn(t *testing.T, h http.Handler) *http.Cookie {
	t.Helper()
	rec := post(t, h, "/login", url.Values{
		"username": {username},
		"password": {examplePassword},
	})
	c := sessionCookie(rec)
	if c == nil {
		t.Fatalf("signing in set no cookie: %d %s", rec.Code, rec.Body)
	}
	return c
}

func TestBrowsingNeedsASession(t *testing.T) {
	t.Parallel()
	h := newHandler(t, nil)

	rec := get(t, h, "/files/")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("GET /files/ without a session = %d, want 303", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/login?next=%2Ffiles%2F" {
		t.Errorf("Location = %q, want the login form with where we were going", got)
	}
	if body := rec.Body.String(); strings.Contains(body, "breadcrumb") {
		t.Error("the listing was rendered to somebody with no session")
	}
}

// TestTheRootIsTheTree: one canonical URL per directory, so "/" is a signpost
// rather than a second page listing the same thing.
func TestTheRootIsTheTree(t *testing.T) {
	t.Parallel()
	h := newHandler(t, nil)

	rec := get(t, h, "/", signIn(t, h))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/files/" {
		t.Errorf("GET / = %d to %q, want 303 to /files/", rec.Code, rec.Header().Get("Location"))
	}
}

func TestThePageAroundTheListing(t *testing.T) {
	t.Parallel()
	h := newHandler(t, nil)

	rec := get(t, h, "/files/", signIn(t, h))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /files/ with a session = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, ">"+username+"<") {
		t.Error("the page does not say who is signed in")
	}
	// The sign-out form is the only way out, so its absence is a bug rather
	// than a cosmetic miss.
	if !strings.Contains(body, `action="/logout"`) {
		t.Error("the page has no sign-out form")
	}
	// The version is what the demo instance uses as its password, so it has to
	// stay readable as its own token beside the date rather than run into it.
	if !strings.Contains(body, version) {
		t.Error("the page does not carry the version")
	}
	if !strings.Contains(body, "built "+buildDate) {
		t.Errorf("the page does not carry the build date:\n%s", body)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
}

// TestASessionFromAnotherPasswordIsNotOne ties the two halves together: the key
// is derived from the credentials, so a cookie issued before a password change
// is refused by the page rather than merely by auth.Sessions.
func TestASessionFromAnotherPasswordIsNotOne(t *testing.T) {
	t.Parallel()
	before := signIn(t, newHandler(t, nil))

	service, thumbs, meta := pieces(t)
	after := web.Handler(version, buildDate, credentials(),
		auth.NewSessions(auth.Credentials{Username: username, Password: "example a different one"}, auth.DefaultSessionTTL),
		service, thumbs, indexing(meta))

	rec := get(t, after, "/files/", before)
	if rec.Code != http.StatusSeeOther {
		t.Errorf("GET /files/ with a stale cookie = %d, want 303 to the login form", rec.Code)
	}
}

func TestUnknownPathIsAPage(t *testing.T) {
	t.Parallel()
	h := newHandler(t, nil)

	rec := get(t, h, "/nothing/here")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /nothing/here = %d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "/nothing/here") {
		t.Error("the 404 page does not say what was not found")
	}
	// Escaped rather than reflected: the path is whatever the client sent.
	rec = get(t, h, "/%3Cscript%3Ealert(1)%3C/script%3E")
	if strings.Contains(rec.Body.String(), "<script>") {
		t.Error("the 404 page reflected a path unescaped")
	}
}

func TestSecurityHeaders(t *testing.T) {
	t.Parallel()
	h := newHandler(t, nil)

	// On a page and on an asset: a header set only on one of them is a header
	// somebody moved without noticing.
	for _, target := range []string{"/login", "/static/bootstrap-5.3.8/bootstrap.min.css"} {
		rec := get(t, h, target)
		head := rec.Header()
		if got := head.Get("Content-Security-Policy"); !strings.Contains(got, "default-src 'none'") ||
			!strings.Contains(got, "form-action 'self'") {
			t.Errorf("%s: Content-Security-Policy = %q", target, got)
		}
		if got := head.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options = %q", target, got)
		}
		if got := head.Get("Referrer-Policy"); got != "same-origin" {
			t.Errorf("%s: Referrer-Policy = %q", target, got)
		}
	}

	// A page is somebody's session and an asset is nobody's: they cache in
	// opposite directions, which is the one header that must not be shared.
	if got := get(t, h, "/login").Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("a page says Cache-Control: %q, want no-store", got)
	}
	if got := get(t, h, "/static/bootstrap-5.3.8/bootstrap.min.css").Header().Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Errorf("an asset says Cache-Control: %q, want it cached forever", got)
	}
}

// TestAssets: the vendored library is served from the binary, which is what
// makes the policy above possible -- there is no CDN to allow.
func TestAssets(t *testing.T) {
	t.Parallel()
	h := newHandler(t, nil)

	for _, tt := range []struct{ path, contentType string }{
		{"/static/bootstrap-5.3.8/bootstrap.min.css", "text/css"},
		{"/static/bootstrap-5.3.8/bootstrap.bundle.min.js", "text/javascript"},
	} {
		rec := get(t, h, tt.path)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", tt.path, rec.Code)
		}
		if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, tt.contentType) {
			t.Errorf("%s: Content-Type = %q, want %s", tt.path, got, tt.contentType)
		}
		if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "immutable") {
			t.Errorf("%s: Cache-Control = %q, want it cacheable forever", tt.path, got)
		}
		if rec.Body.Len() == 0 {
			t.Errorf("%s: served nothing", tt.path)
		}
	}

	// The version is in the path precisely so the header above can say
	// immutable, and the page has to ask for the path that exists.
	if body := get(t, h, "/login").Body.String(); !strings.Contains(body, "/static/bootstrap-5.3.8/bootstrap.min.css") {
		t.Error("the layout does not link the stylesheet it ships")
	}

	// A directory is not an asset: the file server would answer a listing.
	if rec := get(t, h, "/static/bootstrap-5.3.8/"); rec.Code != http.StatusNotFound {
		t.Errorf("GET a static directory = %d, want 404", rec.Code)
	}
}

// TestCrossSiteFormsAreRefused is the CSRF defence, asserted rather than
// assumed: the cookie is SameSite=Lax, and this is the second half.
func TestCrossSiteFormsAreRefused(t *testing.T) {
	t.Parallel()
	h := newHandler(t, nil)
	cookie := signIn(t, h)

	tests := []struct {
		name   string
		header string
		value  string
	}{
		{name: "a browser saying it is cross-site", header: "Sec-Fetch-Site", value: "cross-site"},
		{name: "an origin that is not this host", header: "Origin", value: "https://evil.example"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/logout", nil)
			req.Header.Set(tt.header, tt.value)
			req.AddCookie(cookie)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Errorf("a cross-site POST = %d, want 403", rec.Code)
			}
		})
	}

	// And the same request from this site is not refused, so the guard is not
	// simply blocking everything.
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/logout", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Errorf("a same-origin POST = %d, want 303", rec.Code)
	}
}
