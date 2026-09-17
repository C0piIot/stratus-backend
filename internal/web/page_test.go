package web_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// pageSize is what internal/web renders one page of. Duplicated here rather
// than exported: a test that read the constant would still pass if the
// constant became the whole directory.
const pageSize = 100

// nextLink is where the sentinel row points, which is both the link somebody
// with no JavaScript clicks and the URL htmx fetches.
var nextLink = regexp.MustCompile(`href="([^"]*\?after=[^"]*)"`)

// getHTMX is get with the header htmx puts on every request it makes.
func getHTMX(t *testing.T, h http.Handler, target string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	req.AddCookie(cookie)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestAFolderBiggerThanAPage walks a folder page by page and reassembles it:
// every file once, in order, with nothing repeated across the boundary. That is
// the property a cursor buys over an offset, and the reason the listing is no
// longer one document however big the folder is.
func TestAFolderBiggerThanAPage(t *testing.T) {
	t.Parallel()
	h, s := browser(t)
	const total = pageSize + 5
	mkdir(t, s, "many")
	for i := range total {
		write(t, s, fmt.Sprintf("many/f%03d.txt", i), "x")
	}
	cookie := signIn(t, h)

	first := get(t, h, "/files/many", cookie)
	if first.Code != http.StatusOK {
		t.Fatalf("GET /files/many = %d, want 200", first.Code)
	}
	body := first.Body.String()
	if got := strings.Count(body, ".txt<"); got != pageSize {
		t.Errorf("the first page holds %d files, want %d", got, pageSize)
	}
	if strings.Contains(body, "f100.txt") {
		t.Error("the whole folder came back: the page is not bounded")
	}

	match := nextLink.FindStringSubmatch(body)
	if match == nil {
		t.Fatal("no way to the rest of the folder")
	}
	second := get(t, h, strings.ReplaceAll(match[1], "&amp;", "&"), cookie)
	if second.Code != http.StatusOK {
		t.Fatalf("the next page = %d, want 200", second.Code)
	}
	rest := second.Body.String()
	for i := pageSize; i < total; i++ {
		if !strings.Contains(rest, fmt.Sprintf("f%03d.txt", i)) {
			t.Errorf("f%03d.txt is in neither page", i)
		}
	}
	// The cursor is exclusive: the row it names was on the page before.
	if strings.Contains(rest, "f099.txt") {
		t.Error("the last row of the first page came back again")
	}
	if nextLink.MatchString(rest) {
		t.Error("the last page still offers another one")
	}
}

// TestHtmxGetsTheRowsAndNothingElse: the same URL answers with the page or with
// the piece of it htmx swaps in, and the difference is one request header. No
// second endpoint, and nothing in JSON.
func TestHtmxGetsTheRowsAndNothingElse(t *testing.T) {
	t.Parallel()
	h, s := browser(t)
	mkdir(t, s, "many")
	for i := range pageSize + 1 {
		write(t, s, fmt.Sprintf("many/f%03d.txt", i), "x")
	}
	cookie := signIn(t, h)

	whole := get(t, h, "/files/many", cookie).Body.String()
	match := nextLink.FindStringSubmatch(whole)
	if match == nil {
		t.Fatal("no way to the rest of the folder")
	}

	rec := getHTMX(t, h, strings.ReplaceAll(match[1], "&amp;", "&"), cookie)

	fragment := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("the fragment = %d, want 200", rec.Code)
	}
	if !strings.Contains(fragment, "f100.txt") {
		t.Error("the fragment does not hold the rows it was asked for")
	}
	if strings.Contains(fragment, "<html") || strings.Contains(fragment, "navbar") {
		t.Error("htmx was sent the whole page to swap into the listing")
	}
	if strings.Contains(rec.Header().Get("Content-Type"), "json") {
		t.Error("the fragment is not HTML")
	}
}

// TestACursorThatIsNotOne: the cursor is in the URL, so somebody will type one.
// A refusal, rather than quietly starting from the top, which would look like
// the listing forgetting where it was.
func TestACursorThatIsNotOne(t *testing.T) {
	t.Parallel()
	h, s := browser(t)
	mkdir(t, s, "many")
	write(t, s, "many/one.txt", "x")
	cookie := signIn(t, h)

	for _, cursor := range []string{"f/../etc/passwd", "x/many/one.txt", "nokind"} {
		rec := get(t, h, "/files/many?after="+cursor, cookie)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("?after=%q = %d, want 400", cursor, rec.Code)
		}
	}
}
