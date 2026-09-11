package web_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/C0piIot/stratus-backend/internal/auth"
	"github.com/C0piIot/stratus-backend/internal/web"
)

const (
	username = "edu"
	// "example" in the name and the value, so a secret scanner can tell a
	// fixture from a leaked credential.
	examplePassword = "example correct horse battery staple"
	version         = "1.2.3-test"
)

func credentials() auth.Credentials {
	return auth.Credentials{Username: username, Password: examplePassword}
}

// newHandler builds the UI over the real credentials. A nil verifier means
// those same credentials, which is what the server does.
func newHandler(t *testing.T, v auth.Verifier) http.Handler {
	t.Helper()
	creds := credentials()
	if v == nil {
		v = creds
	}
	return web.Handler(version, v, auth.NewSessions(creds, auth.DefaultSessionTTL))
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

func TestHomeNeedsASession(t *testing.T) {
	t.Parallel()
	h := newHandler(t, nil)

	rec := get(t, h, "/")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("GET / without a session = %d, want 303", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/login?next=%2F" {
		t.Errorf("Location = %q, want the login form with where we were going", got)
	}
	if body := rec.Body.String(); strings.Contains(body, "Signed in") {
		t.Error("the home page was rendered to somebody with no session")
	}
}

func TestHomeWithASession(t *testing.T) {
	t.Parallel()
	h := newHandler(t, nil)

	rec := get(t, h, "/", signIn(t, h))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / with a session = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Signed in as "+username) {
		t.Error("the page does not say who is signed in")
	}
	// The sign-out form is the only way out, so its absence is a bug rather
	// than a cosmetic miss.
	if !strings.Contains(body, `action="/logout"`) {
		t.Error("the page has no sign-out form")
	}
	if !strings.Contains(body, version) {
		t.Error("the page does not carry the version")
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

	after := web.Handler(version, credentials(),
		auth.NewSessions(auth.Credentials{Username: username, Password: "example a different one"}, auth.DefaultSessionTTL))

	rec := get(t, after, "/", before)
	if rec.Code != http.StatusSeeOther {
		t.Errorf("GET / with a stale cookie = %d, want 303 to the login form", rec.Code)
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
