package auth_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/auth"
)

// whoever serves the name auth.Session put on the request, or nothing.
func whoever(served *string) http.Handler {
	return http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		*served, _ = auth.User(r.Context())
	})
}

func withCookie(t *testing.T, s *auth.Sessions, site string) *http.Request {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/rest/deletePlaylist?id=3", nil)
	value, _ := s.Issue(username, time.Now())
	req.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: value})
	if site != "" {
		req.Header.Set("Sec-Fetch-Site", site)
	}
	return req
}

func TestSessionOpensAProtocolToItsOwnPages(t *testing.T) {
	t.Parallel()
	s := sessions(t)
	for _, site := range []string{"same-origin", "none"} {
		var served string
		auth.Session(s, whoever(&served)).ServeHTTP(httptest.NewRecorder(), withCookie(t, s, site))
		if served != username {
			t.Errorf("Sec-Fetch-Site: %s served %q, want %q", site, served, username)
		}
	}
}

// TestSessionIgnoresACookieFromElsewhere is the whole reason for the rule:
// OpenSubsonic changes state over GET, and SameSite=Lax sends the cookie on a
// link followed from any page. A browser that does not say is not believed.
func TestSessionIgnoresACookieFromElsewhere(t *testing.T) {
	t.Parallel()
	s := sessions(t)
	for _, site := range []string{"cross-site", "same-site", ""} {
		served := "untouched"
		auth.Session(s, whoever(&served)).ServeHTTP(httptest.NewRecorder(), withCookie(t, s, site))
		if served != "" {
			t.Errorf("Sec-Fetch-Site: %q served %q, want nobody", site, served)
		}
	}
}

func TestSessionIgnoresACookieItDidNotIssue(t *testing.T) {
	t.Parallel()
	other := auth.NewSessions(auth.Credentials{Username: username, Password: "another"}, auth.DefaultSessionTTL)
	var served string
	auth.Session(sessions(t), whoever(&served)).ServeHTTP(httptest.NewRecorder(), withCookie(t, other, "same-origin"))
	if served != "" {
		t.Errorf("a foreign cookie served %q", served)
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	if _, err := sessions(t).FromCookie(req, time.Now()); err == nil {
		t.Error("no cookie at all was accepted")
	}
}

// TestSessionLeavesAnUpstreamUserAlone: a signed link said who this is first.
func TestSessionLeavesAnUpstreamUserAlone(t *testing.T) {
	t.Parallel()
	s := sessions(t)
	req := withCookie(t, s, "same-origin")
	req = req.WithContext(auth.WithUser(req.Context(), "owner of the link"))
	var served string
	auth.Session(s, whoever(&served)).ServeHTTP(httptest.NewRecorder(), req)
	if served != "owner of the link" {
		t.Errorf("served %q", served)
	}
}

// TestBasicRefusesWhatABrowserSendsFromElsewhere: a browser caches Basic and
// attaches it like a cookie. 403 with no challenge, and the throttle is not
// asked -- refusingVerifier would turn that into a 401.
func TestBasicRefusesWhatABrowserSendsFromElsewhere(t *testing.T) {
	t.Parallel()
	var reached bool
	h := auth.Basic("Stratus", refusingVerifier{}, protected(&reached))
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/dav/notes.txt", nil)
	req.SetBasicAuth(username, examplePassword)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden || reached {
		t.Errorf("status = %d, reached = %v; want 403 and no handler", rec.Code, reached)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != "" {
		t.Errorf("WWW-Authenticate = %q, want none", got)
	}
}

func TestBasicUserFromItsOwnPage(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/rest/ping", nil)
	req.SetBasicAuth(username, examplePassword)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	got, sent, err := auth.BasicUser(req, credentials(t))
	if got != username || !sent || err != nil {
		t.Errorf("BasicUser = %q, %v, %v", got, sent, err)
	}
	if _, _, err := auth.BasicUser(withPassword(t, "not it"), credentials(t)); err == nil ||
		!strings.Contains(err.Error(), "unauthorized") {
		t.Errorf("a wrong password = %v", err)
	}
}

func withPassword(t *testing.T, password string) *http.Request {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/rest/ping", nil)
	req.SetBasicAuth(username, password)
	return req
}
