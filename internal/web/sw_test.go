package web_test

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// shellList reads the addresses the handler wrote in front of the worker.
var shellList = regexp.MustCompile(`const SHELL = (\[[^\n]*\]);`)

// TestTheWorkerAnswersWithoutASession, from the root, and names a shell every
// address of which answers the same way.
//
// All three halves are the same failure if they are wrong: a worker behind the
// login does not register, one served from /static/ controls nothing, and one
// precaching an address that 404s fails to install — and none of the three
// says anything the server can see.
func TestTheWorkerAnswersWithoutASession(t *testing.T) {
	t.Parallel()
	h := newHandler(t, nil)

	rec := get(t, h, "/sw.js")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /sw.js with no session = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/javascript") {
		t.Errorf("Content-Type = %q, want a script", got)
	}
	// The browser decides there is a new worker by comparing the bytes, so a
	// cached copy is one that never updates.
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", got)
	}

	body := rec.Body.String()
	if !strings.Contains(body, `const VERSION = "`+version+`"`) {
		t.Errorf("the worker does not name this build, so its cache cannot turn over:\n%s", body)
	}
	if !strings.Contains(body, "addEventListener(\"fetch\"") {
		t.Error("the worker has no fetch handler, which is what a browser asks for before it offers to install")
	}

	found := shellList.FindStringSubmatch(body)
	if found == nil {
		t.Fatalf("the worker was served with no shell to cache:\n%s", body)
	}
	for _, target := range strings.Split(strings.Trim(found[1], "[]"), ",") {
		target = strings.Trim(strings.TrimSpace(target), `"`)
		if rec := get(t, h, target); rec.Code != http.StatusOK {
			t.Errorf("GET %s with no session = %d, and the worker will not install without it", target, rec.Code)
		}
	}
}

// TestTheOfflinePageIsAPage: it is what a navigation gets when the server
// cannot be reached, so it is rendered from the layout like the rest and it
// takes no session — there is no network to check one over.
func TestTheOfflinePageIsAPage(t *testing.T) {
	t.Parallel()
	h := newHandler(t, nil)

	rec := get(t, h, "/offline")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /offline = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "cannot be reached") || !strings.Contains(body, "bootstrap.min.css") {
		t.Errorf("the offline page is not the UI's own:\n%s", body)
	}
}

// TestThePagesRegisterTheWorker: the third script of this project's own, on
// every page, with the build on the end like the other two.
func TestThePagesRegisterTheWorker(t *testing.T) {
	t.Parallel()
	h := newHandler(t, nil)

	body := get(t, h, "/login").Body.String()
	if !strings.Contains(body, `/static/stratus/register.js?v=`) {
		t.Errorf("no page registers the worker:\n%s", body)
	}
	if rec := get(t, h, "/static/stratus/register.js?v="+version); rec.Code != http.StatusOK {
		t.Errorf("GET register.js = %d, want 200", rec.Code)
	}
}
