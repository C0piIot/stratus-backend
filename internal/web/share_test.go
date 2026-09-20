package web_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// getRange is get with a Range header, which no other page in this package
// needs and every receiver of a shared film sends.
func getRange(t *testing.T, h http.Handler, target, spec string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	req.Header.Set("Range", spec)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// A link somebody without an account can open (#169). Every request in this
// file is made with no cookie at all, which is the point.

// linkTo makes a share through the UI, the way a person does, and returns the
// URL it handed back.
func linkTo(t *testing.T, h http.Handler, target, life string) string {
	t.Helper()
	rec := post(t, h, "/share/"+target, url.Values{"life": {life}}, signIn(t, h))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /share/%s = %d: %s", target, rec.Code, rec.Body)
	}
	found := regexp.MustCompile(`value="(https?://[^"]+)"`).FindStringSubmatch(rec.Body.String())
	if found == nil {
		t.Fatalf("the share page handed back no link: %s", rec.Body)
	}
	return html(found[1])
}

// html undoes the escaping a template applies to an attribute, since what the
// test wants is the URL a browser would follow.
func html(s string) string {
	return strings.NewReplacer("&amp;", "&", "&#43;", "+", "&#61;", "=").Replace(s)
}

// TestAShareLinkIsAWholeAddress is the bug this page shipped with: it handed
// back a path, and a path is not something you can send anybody. The host comes
// from the request, the way every self-hosted server builds one.
func TestAShareLinkIsAWholeAddress(t *testing.T) {
	t.Parallel()
	h, s, _ := browserOver(t)
	write(t, s, "holiday.txt", "the whole film")

	link := linkTo(t, h, "holiday.txt", "7d")
	if !strings.HasPrefix(link, "http://example.com/files/holiday.txt?") {
		t.Errorf("the link is %q, want the address this request arrived at", link)
	}

	// And behind something that terminates TLS, which is how most of these are
	// actually reached.
	rec := postProxied(t, h, "/share/holiday.txt", url.Values{"life": {"7d"}}, signIn(t, h))
	if !strings.Contains(rec.Body.String(), "https://stratus.example/files/holiday.txt?") {
		t.Errorf("behind a proxy the link is not https at the proxy's name: %s", rec.Body)
	}
}

// postProxied is post with the headers a reverse proxy adds.
func postProxied(t *testing.T, h http.Handler, target string, form url.Values, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, target, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Host = "stratus.example"
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestTheShareePageOffersToCopy is the button, and the half that matters is
// that it is not there until something can make it work: a browser with no
// script gets the field alone rather than a button that does nothing.
func TestTheSharePageOffersToCopy(t *testing.T) {
	t.Parallel()
	h, s, _ := browserOver(t)
	write(t, s, "holiday.txt", "the whole film")

	body := post(t, h, "/share/holiday.txt", url.Values{"life": {"7d"}}, signIn(t, h)).Body.String()
	if !strings.Contains(body, `data-copies="link"`) {
		t.Errorf("no copy button: %s", body)
	}
	if !strings.Contains(body, `class="btn btn-outline-secondary d-none"`) {
		t.Error("the button is not hidden, so a browser with no script shows a dead one")
	}
	if !strings.Contains(body, "/static/stratus/copy.js?v=") {
		t.Error("the page does not load the script that would show it")
	}

	// And the script is actually served, with the build on the URL so a new
	// one is a new address.
	rec := get(t, h, "/static/stratus/copy.js?v="+version, signIn(t, h))
	if rec.Code != http.StatusOK {
		t.Fatalf("the script = %d", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
		t.Errorf("Cache-Control = %q", rec.Header().Get("Cache-Control"))
	}
}

// TestSharedFileOpensWithoutAnAccount is the requirement, including the range:
// a receiver seeks, and a film that cannot seek is not a film.
func TestSharedFileOpensWithoutAnAccount(t *testing.T) {
	t.Parallel()
	h, s, _ := browserOver(t)
	write(t, s, "holiday.txt", "the whole film")

	link := linkTo(t, h, "holiday.txt", "7d")

	rec := get(t, h, link)
	if rec.Code != http.StatusOK {
		t.Fatalf("a shared file with no cookie = %d", rec.Code)
	}
	if rec.Body.String() != "the whole film" {
		t.Errorf("the link served %q", rec.Body)
	}

	// The range, which is what casting turns on.
	ranged := getRange(t, h, link, "bytes=4-8")
	if ranged.Code != http.StatusPartialContent {
		t.Fatalf("a range over a shared file = %d, want 206", ranged.Code)
	}
	if ranged.Body.String() != "whole" {
		t.Errorf("the range served %q", ranged.Body)
	}
}

// TestSharedFolderReachesUnderItAndNoFurther is the rule that decides whether
// one shared folder is one folder or the whole library.
func TestSharedFolderReachesUnderItAndNoFurther(t *testing.T) {
	t.Parallel()
	h, s, _ := browserOver(t)
	mkdir(t, s, "album")
	mkdir(t, s, "album/raw")
	write(t, s, "album/one.txt", "one")
	write(t, s, "album/raw/two.txt", "two")
	write(t, s, "secret.txt", "not shared")
	mkdir(t, s, "album2")
	write(t, s, "album2/other.txt", "also not shared")

	link := linkTo(t, h, "album", "never")
	token := url.Values{}
	if u, err := url.Parse(link); err == nil {
		token.Set("k", u.Query().Get("k"))
	}

	if rec := get(t, h, link); rec.Code != http.StatusOK {
		t.Fatalf("the shared folder = %d", rec.Code)
	}
	for _, reach := range []string{"album/one.txt", "album/raw", "album/raw/two.txt"} {
		if rec := get(t, h, "/files/"+reach+"?"+token.Encode()); rec.Code != http.StatusOK {
			t.Errorf("the link does not reach %s: %d", reach, rec.Code)
		}
	}
	// Above it, beside it, and the one a prefix test gets wrong.
	for _, refused := range []string{"", "secret.txt", "album2", "album2/other.txt"} {
		if rec := get(t, h, "/files/"+refused+"?"+token.Encode()); rec.Code != http.StatusForbidden {
			t.Errorf("the link reached %q: %d", refused, rec.Code)
		}
	}
}

// TestSharedPageOffersNoWayToWrite: the gate is what makes a link read-only,
// and the page has to agree with it -- an upload form that 302s to a login is
// worse than no form.
func TestSharedPageOffersNoWayToWrite(t *testing.T) {
	t.Parallel()
	h, s, _ := browserOver(t)
	mkdir(t, s, "album")
	write(t, s, "album/one.txt", "one")

	body := get(t, h, linkTo(t, h, "album", "never")).Body.String()

	for what, marker := range map[string]string{
		"an upload form":     `enctype="multipart/form-data"`,
		"a new folder form":  "/folders/",
		"a rename link":      "/rename/",
		"a delete link":      "/delete/",
		"a share link":       "/share/",
		"a way to sign out":  "/logout",
		"the status page":    "/status",
		"a path to the root": `href="/files/"`,
	} {
		if strings.Contains(body, marker) {
			t.Errorf("a shared listing offers %s", what)
		}
	}
	// Nor the owner's name, which is not a visitor's business.
	if strings.Contains(body, username) {
		t.Errorf("a shared listing names the account it belongs to")
	}
}

// TestSharedPageCarriesItsOwnToken is the half that fails quietly: a listing
// whose links drop the signature works for one click.
func TestSharedPageCarriesItsOwnToken(t *testing.T) {
	t.Parallel()
	h, s, _ := browserOver(t)
	mkdir(t, s, "album")
	write(t, s, "album/one.jpg", "not really a photograph")

	link := linkTo(t, h, "album", "never")
	body := get(t, h, link).Body.String()

	// The row's own link, which is how somebody gets to the file at all.
	if !strings.Contains(body, "k=") {
		t.Fatalf("no link on the page carries the signature: %s", body)
	}
	found := regexp.MustCompile(`href="(/files/album/[^"]+)"`).FindStringSubmatch(body)
	if found == nil {
		t.Fatal("the listing has no link to what is in it")
	}
	if rec := get(t, h, html(found[1])); rec.Code != http.StatusOK {
		t.Errorf("following the listing's own link = %d", rec.Code)
	}
}

// TestSharedCrumbsWalkBackToTheShareAndNoFurther: a visitor two folders deep
// needs a way back to what they were sent, and no way past it.
func TestSharedCrumbsWalkBackToTheShareAndNoFurther(t *testing.T) {
	t.Parallel()
	h, s, _ := browserOver(t)
	mkdir(t, s, "album")
	mkdir(t, s, "album/raw")
	write(t, s, "album/raw/two.txt", "two")

	link := linkTo(t, h, "album", "never")
	token := link[strings.Index(link, "?"):]

	body := get(t, h, "/files/album/raw"+token).Body.String()
	trail := regexp.MustCompile(`(?s)<ol class="breadcrumb">.*?</ol>`).FindString(body)
	if trail == "" {
		t.Fatal("the page has no breadcrumbs at all")
	}
	if !strings.Contains(trail, "album") {
		t.Errorf("two folders into a share there is no way back to it: %s", trail)
	}
	if strings.Contains(trail, `href="/files/"`) {
		t.Errorf("the trail climbs above the share: %s", trail)
	}

	// And the way back works, signature and all.
	back := regexp.MustCompile(`href="(/files/album\?[^"]*)"`).FindStringSubmatch(trail)
	if back == nil {
		t.Fatalf("the share is in the trail but not as a link: %s", trail)
	}
	if rec := get(t, h, html(back[1])); rec.Code != http.StatusOK {
		t.Errorf("following the trail back to the share = %d", rec.Code)
	}
}

// TestADeadLinkIsRefusedRatherThanRedirected: somebody sent a link that no
// longer works needs to be told, and a receiver needs a status code rather than
// the HTML of a login form.
func TestADeadLinkIsRefusedRatherThanRedirected(t *testing.T) {
	t.Parallel()
	h, s, _ := browserOver(t)
	write(t, s, "holiday.txt", "the whole film")
	link := linkTo(t, h, "holiday.txt", "7d")

	for name, target := range map[string]string{
		"an edited token": link[:len(link)-4] + "aaaa",
		"no token at all": "/files/holiday.txt?k=",
		"nonsense":        "/files/holiday.txt?k=hello",
	} {
		t.Run(name, func(t *testing.T) {
			rec := get(t, h, target)
			switch {
			// An empty parameter is not an offered token: that request has no
			// credentials at all and belongs at the login form.
			case target == "/files/holiday.txt?k=":
				if rec.Code != http.StatusSeeOther {
					t.Errorf("= %d, want the login form", rec.Code)
				}
			case rec.Code != http.StatusForbidden:
				t.Errorf("= %d, want 403", rec.Code)
			}
		})
	}
}

// TestShareFormAsksFirst is the shape rename and delete already have: a GET
// that asks and a POST that does. What it asks is the only control a stateless
// link has.
func TestShareFormAsksFirst(t *testing.T) {
	t.Parallel()
	h, s, _ := browserOver(t)
	mkdir(t, s, "album")
	write(t, s, "holiday.txt", "the whole film")

	body := get(t, h, "/share/holiday.txt", signIn(t, h)).Body.String()
	if !strings.Contains(body, "Share file") {
		t.Errorf("the form does not say what it is sharing: %s", body)
	}
	if !strings.Contains(body, "until the password changes") {
		t.Error("the form does not offer a link with no expiry")
	}

	folder := get(t, h, "/share/album", signIn(t, h)).Body.String()
	if !strings.Contains(folder, "Share folder") {
		t.Errorf("a folder is offered as a file: %s", folder)
	}
	// And it says what a folder link reaches, because that is the part somebody
	// would otherwise get wrong.
	if !strings.Contains(folder, "nothing above it") {
		t.Error("the form does not say how far a folder link reaches")
	}
}

// TestAShareOfNothing: a token offered at a path this server will not even
// accept is refused where it is offered, rather than carried further in.
func TestAShareOfNothing(t *testing.T) {
	t.Parallel()
	h, s, _ := browserOver(t)
	write(t, s, "holiday.txt", "the whole film")
	link := linkTo(t, h, "holiday.txt", "7d")
	token := link[strings.Index(link, "?"):]

	tooLong := strings.Repeat("a", db.MaxPathLen+1)
	if rec := get(t, h, "/files/"+tooLong+token); rec.Code != http.StatusForbidden {
		t.Errorf("a link at a path this server refuses = %d, want 403", rec.Code)
	}
}

// TestSharingNeedsASession: making a link is the owner's, even though opening
// one is not.
func TestSharingNeedsASession(t *testing.T) {
	t.Parallel()
	h, s, _ := browserOver(t)
	write(t, s, "holiday.txt", "the whole film")

	if rec := get(t, h, "/share/holiday.txt"); rec.Code != http.StatusSeeOther {
		t.Errorf("GET /share with no session = %d, want the login form", rec.Code)
	}
	if rec := post(t, h, "/share/holiday.txt", url.Values{"life": {"7d"}}); rec.Code != http.StatusSeeOther {
		t.Errorf("POST /share with no session = %d, want the login form", rec.Code)
	}
	// And a lifetime nobody offered is refused rather than invented.
	rec := post(t, h, "/share/holiday.txt", url.Values{"life": {"forever and ever"}}, signIn(t, h))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("an unoffered lifetime = %d, want 400", rec.Code)
	}
}
