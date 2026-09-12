package web_test

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/auth"
)

func TestLoginForm(t *testing.T) {
	t.Parallel()
	h := newHandler(t, nil)

	rec := get(t, h, "/login")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /login = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{`action="/login"`, `name="username"`, `name="password"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the form has no %s", want)
		}
	}
	if sessionCookie(rec) != nil {
		t.Error("asking for the form set a session")
	}
}

// TestLoginFormWithASession: a signed-in browser that lands on the form is sent
// on rather than shown a second login.
func TestLoginFormWithASession(t *testing.T) {
	t.Parallel()
	h := newHandler(t, nil)

	rec := get(t, h, "/login", signIn(t, h))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("GET /login with a session = %d, want 303", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/" {
		t.Errorf("Location = %q, want /", got)
	}
}

func TestLoginSucceeds(t *testing.T) {
	t.Parallel()
	h := newHandler(t, nil)

	rec := post(t, h, "/login", url.Values{
		"username": {username},
		"password": {examplePassword},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /login = %d, want 303", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/" {
		t.Errorf("Location = %q, want /", got)
	}

	c := sessionCookie(rec)
	if c == nil {
		t.Fatal("no session cookie")
	}
	switch {
	case !c.HttpOnly:
		t.Error("the cookie is readable from JavaScript")
	case c.SameSite != http.SameSiteLaxMode:
		t.Errorf("SameSite = %v, want Lax", c.SameSite)
	case c.Path != "/":
		t.Errorf("Path = %q, want /", c.Path)
	case c.Secure:
		t.Error("Secure over plain HTTP: the browser would never send it back")
	}
	// Within a minute of the ceiling auth.Sessions signs into the value. A
	// cookie that outlived the signature would look like a session and behave
	// like a redirect loop.
	if want := int(auth.DefaultSessionTTL.Seconds()); c.MaxAge > want || c.MaxAge < want-60 {
		t.Errorf("Max-Age = %d, want about %d", c.MaxAge, want)
	}
}

// TestCookieIsSecureOverTLS: the attribute cannot be unconditional -- a
// self-hoster on plain HTTP would get a cookie the browser stores and never
// sends -- so it follows how the request actually arrived.
func TestCookieIsSecureOverTLS(t *testing.T) {
	t.Parallel()
	h := newHandler(t, nil)
	form := url.Values{"username": {username}, "password": {examplePassword}}

	tests := []struct {
		name  string
		setup func(*http.Request)
	}{
		{name: "TLS on the connection", setup: func(r *http.Request) { r.TLS = &tls.ConnectionState{} }},
		{
			name:  "a proxy that terminated it",
			setup: func(r *http.Request) { r.Header.Set("X-Forwarded-Proto", "https") },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/login",
				strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			tt.setup(req)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if c := sessionCookie(rec); c == nil || !c.Secure {
				t.Errorf("cookie = %v, want one marked Secure", c)
			}
		})
	}
}

func TestLoginRefuses(t *testing.T) {
	t.Parallel()
	h := newHandler(t, nil)

	rec := post(t, h, "/login", url.Values{
		"username": {username},
		"password": {"not it"},
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("POST /login with the wrong password = %d, want 401", rec.Code)
	}
	if sessionCookie(rec) != nil {
		t.Fatal("a failed login set a session")
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Wrong username or password") {
		t.Error("the form does not say what went wrong")
	}
	// The name is typed back in, the password is not.
	if !strings.Contains(body, `value="`+username+`"`) {
		t.Error("the form lost the username")
	}
	if strings.Contains(body, "not it") {
		t.Error("the form echoed the password back")
	}
	// A 401 with this header would raise the browser's own credential dialog
	// on top of the page.
	if got := rec.Header().Get("WWW-Authenticate"); got != "" {
		t.Errorf("WWW-Authenticate = %q, want none on an HTML login", got)
	}
}

// TestLoginIsThrottled: the form guesses against the same budget WebDAV and
// OpenSubsonic do, so the answer for a flood has to survive to the page.
func TestLoginIsThrottled(t *testing.T) {
	t.Parallel()
	h := newHandler(t, refusing{err: auth.ErrTooManyAttempts})

	rec := post(t, h, "/login", url.Values{"username": {username}, "password": {"guess"}})
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("POST /login while throttled = %d, want 429", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got == "" {
		t.Error("no Retry-After, so a client cannot know when to come back")
	}
	if sessionCookie(rec) != nil {
		t.Error("a throttled login set a session")
	}
	if !strings.Contains(rec.Body.String(), "Too many attempts") {
		t.Error("the form does not say why it was refused")
	}
}

func TestLogout(t *testing.T) {
	t.Parallel()
	h := newHandler(t, nil)
	cookie := signIn(t, h)

	rec := post(t, h, "/logout", nil, cookie)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /logout = %d, want 303", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/login" {
		t.Errorf("Location = %q, want /login", got)
	}
	c := sessionCookie(rec)
	if c == nil {
		t.Fatal("logging out did not touch the cookie")
	}
	if c.Value != "" || c.MaxAge >= 0 {
		t.Errorf("cookie = %q with Max-Age %d, want it deleted", c.Value, c.MaxAge)
	}
}

// TestWhereItSendsYouAfterSigningIn covers the redirect the login form carries,
// which is an open redirect the day nobody checks it.
func TestWhereItSendsYouAfterSigningIn(t *testing.T) {
	t.Parallel()
	h := newHandler(t, nil)

	tests := []struct {
		name string
		next string
		want string
	}{
		{name: "nothing asked for", want: "/"},
		{name: "a page in this server", next: "/files/photos", want: "/files/photos"},
		{name: "a query survives", next: "/files?sort=name", want: "/files?sort=name"},
		{name: "another host", next: "https://evil.example/", want: "/"},
		{name: "scheme-relative", next: "//evil.example/", want: "/"},
		{name: "scheme-relative with a backslash", next: `/\evil.example/`, want: "/"},
		{name: "not a path at all", next: "evil.example", want: "/"},
		{name: "a path that is not a URL", next: "/%zz", want: "/"},
		{name: "the root itself", next: "/", want: "/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := post(t, h, "/login", url.Values{
				"username": {username},
				"password": {examplePassword},
				"next":     {tt.next},
			})
			if got := rec.Header().Get("Location"); got != tt.want {
				t.Errorf("Location = %q, want %q", got, tt.want)
			}
		})
	}

	// And the form passes it along, so the round trip works: a browser sent to
	// the login form from a page comes back to that page.
	rec := get(t, h, "/login?next=%2Ffiles%2Fphotos")
	if !strings.Contains(rec.Body.String(), `value="/files/photos"`) {
		t.Error("the form did not carry the page we were going to")
	}
	rec = get(t, h, "/login?next=https%3A%2F%2Fevil.example%2F")
	if strings.Contains(rec.Body.String(), "evil.example") {
		t.Error("the form carried somebody else's host into the redirect")
	}
}

// TestAnExpiredSessionIsNoSession: the value carries its own expiry, so a
// browser that kept the cookie past it is sent back to the form. The cookie's
// own Max-Age is not the guard -- it is a request away from being edited.
func TestAnExpiredSessionIsNoSession(t *testing.T) {
	t.Parallel()
	sessions := auth.NewSessions(credentials(), auth.DefaultSessionTTL)
	value, _ := sessions.Issue(username, time.Now().Add(-2*auth.DefaultSessionTTL))

	h := newHandler(t, nil)
	rec := get(t, h, "/files/", &http.Cookie{Name: "stratus_session", Value: value})
	if rec.Code != http.StatusSeeOther {
		t.Errorf("GET /files/ with an expired session = %d, want 303 to the login form", rec.Code)
	}
}
