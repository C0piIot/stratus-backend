package web_test

import (
	"net/http"
	"strings"
	"testing"
)

// TestNothingHereIsForCrawlers: the file is in front of the session, because a
// crawler has no credentials and would otherwise be told to log in before it
// could be told to go away.
func TestNothingHereIsForCrawlers(t *testing.T) {
	t.Parallel()
	h := newHandler(t, nil)

	rec := get(t, h, "/robots.txt")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /robots.txt with no session = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "User-agent: *\nDisallow: /\n" {
		t.Errorf("robots.txt = %q", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "max-age=") {
		t.Errorf("Cache-Control = %q, want it cacheable", got)
	}
}

// TestEveryPageSaysNotToIndexIt is the other half: a crawler that read the file
// and came in anyway, and the share link it must not publish.
func TestEveryPageSaysNotToIndexIt(t *testing.T) {
	t.Parallel()
	h, s := browser(t)
	mkdir(t, s, "holiday")
	cookie := signIn(t, h)

	// The login form is fetched by somebody with no session, which is the only
	// way it renders: a signed-in browser is sent to the tree instead.
	pages := []struct {
		name    string
		target  string
		cookies []*http.Cookie
	}{
		{"the login form", "/login", nil},
		{"a listing", "/files/", []*http.Cookie{cookie}},
		{"a share link", linkTo(t, h, "holiday", "7d"), nil},
	}
	for _, p := range pages {
		body := get(t, h, p.target, p.cookies...).Body.String()
		if !strings.Contains(body, `<meta name="robots" content="noindex, nofollow">`) {
			t.Errorf("%s does not ask to be left out of an index:\n%s", p.name, body)
		}
	}
}
