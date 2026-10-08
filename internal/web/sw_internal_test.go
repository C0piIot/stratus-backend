package web

import (
	"strings"
	"testing"
)

// TestTheWorkerCachesOnlyWhileItInstalls is the rule sw.go is written around,
// kept where the file is rather than where the behaviour would be: a worker
// that wrote to the cache while it served would leave a listing behind for
// whoever opens the browser next, and nothing on the server would ever see it.
//
// A grep, like TestNoTemplateWritesAnInlineStyle, and for the same reason:
// what makes it true is a property of this file, not of a handler.
func TestTheWorkerCachesOnlyWhileItInstalls(t *testing.T) {
	t.Parallel()

	body, err := staticFS.ReadFile(workerSource)
	if err != nil {
		t.Fatal(err)
	}
	source := string(body)

	// cache.put and cache.add are the two ways to write to one. addAll is the
	// install path and is allowed; anything else is not here at all.
	for _, forbidden := range []string{".put(", ".add("} {
		if strings.Contains(source, forbidden) {
			t.Errorf("the worker writes to the cache with %s, which it may only do while it installs:\n%s",
				forbidden, source)
		}
	}
	if strings.Count(source, "addAll") != 1 {
		t.Errorf("the worker fills its cache somewhere other than install:\n%s", source)
	}
}

// TestTheWorkerIsAllowedByThePolicy: worker-src is the third directive that
// exists for one feature and fails in a browser alone, after connect-src and
// manifest-src. Without it the registration is refused and the page looks
// exactly like a page that never asked for a worker.
func TestTheWorkerIsAllowedByThePolicy(t *testing.T) {
	t.Parallel()

	if !strings.Contains(contentSecurityPolicy, "worker-src 'self'") {
		t.Errorf("the policy refuses the worker: %s", contentSecurityPolicy)
	}
}

// TestTheWorkerDoesNotReissueNavigations is the other grep over this file, and
// it exists because the failure it guards against is invisible from here: a
// navigation re-issued with fetch(request) loses Sec-Fetch-Mode in Firefox,
// and the server reads that header to tell a browser from a WebDAV client at
// the same URL -- so a signed-out navigation through the worker was answered
// with the Basic challenge instead of the login page. The browser's own
// preloaded response carries what only the browser can set.
func TestTheWorkerDoesNotReissueNavigations(t *testing.T) {
	t.Parallel()

	body, err := staticFS.ReadFile(workerSource)
	if err != nil {
		t.Fatal(err)
	}
	source := string(body)

	for _, want := range []string{"navigationPreload.enable()", "event.preloadResponse"} {
		if !strings.Contains(source, want) {
			t.Errorf("the worker re-issues navigations instead of waiting for %s:\n%s", want, source)
		}
	}
}
