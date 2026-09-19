package dav_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/auth"
	"github.com/C0piIot/stratus-backend/internal/dav"
)

// A share link works on this surface too, because the app speaks WebDAV and
// needs a URL a Chromecast can fetch (stratus-app#6).

const sharePassword = "an example password"

// signedServer is the mount as the composition root builds it: a signature, or
// HTTP Basic behind it.
func signedServer(t *testing.T) (http.Handler, *auth.Shares) {
	t.Helper()
	creds := auth.Credentials{Username: "edu", Password: sharePassword}
	shares := auth.NewShares(creds)
	inner := dav.Handler(prefix, service(t))
	return dav.SignedLinks(prefix, shares,
		auth.Basic("stratus", auth.NewThrottle(creds, auth.DefaultThrottle), inner)), shares
}

// signedGet is a request with no credentials at all beyond what is in the URL.
func signedGet(t *testing.T, h http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, target, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestSignedLinkReadsAFile is the point: the URL the app already builds, plus a
// signature, fetched by something that cannot send a header.
func TestSignedLinkReadsAFile(t *testing.T) {
	t.Parallel()
	h, shares := signedServer(t)
	put(t, h, "/dav/holiday.mp4", "the whole film")

	token := shares.Issue("edu", "holiday.mp4", false, time.Now().Add(time.Hour))
	rec := signedGet(t, h, http.MethodGet, "/dav/holiday.mp4?k="+token)
	if rec.Code != http.StatusOK {
		t.Fatalf("a signed read = %d: %s", rec.Code, rec.Body)
	}
	if rec.Body.String() != "the whole film" {
		t.Errorf("it served %q", rec.Body)
	}

	// And it seeks, which is the whole reason a receiver can play it.
	ranged := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/dav/holiday.mp4?k="+token, nil)
	ranged.Header.Set("Range", "bytes=4-8")
	partial := httptest.NewRecorder()
	h.ServeHTTP(partial, ranged)
	if partial.Code != http.StatusPartialContent || partial.Body.String() != "whole" {
		t.Errorf("a range = %d %q", partial.Code, partial.Body)
	}
}

// TestSignedLinkIsReadOnlyHere is what the gate is scoped by: a link opens a
// read and nothing else, wherever it is presented.
func TestSignedLinkIsReadOnlyHere(t *testing.T) {
	t.Parallel()
	h, shares := signedServer(t)
	put(t, h, "/dav/notes.txt", "notes")
	token := shares.Issue("edu", "notes.txt", false, time.Time{})

	// A write with a link on it is not a write: Basic is still behind this and
	// still asks, which is a 401 rather than a 403 because nothing was judged.
	for _, method := range []string{http.MethodPut, http.MethodDelete, "MOVE", "COPY", "PROPPATCH"} {
		req := httptest.NewRequestWithContext(t.Context(), method, "/dav/notes.txt?k="+token,
			strings.NewReader("x"))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s with a link = %d, want 401", method, rec.Code)
		}
	}

	// Nor a listing. A folder link is for a person, and a person opens the
	// browser surface, which renders one.
	folder := shares.Issue("edu", "", true, time.Time{})
	rec := signedGet(t, h, "PROPFIND", "/dav/?k="+folder)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("PROPFIND with a link = %d, want 401", rec.Code)
	}
}

// TestSignedLinkCoversWhatItNames, from the side that matters here: a link to
// one file is not a key to the tree.
func TestSignedLinkCoversWhatItNames(t *testing.T) {
	t.Parallel()
	h, shares := signedServer(t)
	put(t, h, "/dav/holiday.mp4", "the film")
	put(t, h, "/dav/secret.txt", "not shared")
	mkcol(t, h, "/dav/album")
	put(t, h, "/dav/album/inside.txt", "inside")

	file := shares.Issue("edu", "holiday.mp4", false, time.Now().Add(time.Hour))
	if rec := signedGet(t, h, http.MethodGet, "/dav/secret.txt?k="+file); rec.Code != http.StatusForbidden {
		t.Errorf("a link to one file opened another: %d", rec.Code)
	}

	// A folder link does reach a read under it -- the scope is the signature's,
	// not this gate's, and a client fetching one file of a shared album is the
	// case that wants it.
	folder := shares.Issue("edu", "album", true, time.Time{})
	if rec := signedGet(t, h, http.MethodGet, "/dav/album/inside.txt?k="+folder); rec.Code != http.StatusOK {
		t.Errorf("a folder link does not reach inside it: %d", rec.Code)
	}
	if rec := signedGet(t, h, http.MethodGet, "/dav/secret.txt?k="+folder); rec.Code != http.StatusForbidden {
		t.Errorf("a folder link reached outside it: %d", rec.Code)
	}
}

// TestADeadLinkHereIsRefusedAndNotChallenged: a receiver needs a status code it
// can act on, not a password prompt for an account nobody has.
func TestADeadLinkHereIsRefusedAndNotChallenged(t *testing.T) {
	t.Parallel()
	h, shares := signedServer(t)
	put(t, h, "/dav/holiday.mp4", "the film")

	expired := shares.Issue("edu", "holiday.mp4", false, time.Now().Add(-time.Hour))
	for name, token := range map[string]string{"expired": expired, "nonsense": "hello"} {
		rec := signedGet(t, h, http.MethodGet, "/dav/holiday.mp4?k="+token)
		if rec.Code != http.StatusForbidden {
			t.Errorf("a %s link = %d, want 403", name, rec.Code)
		}
		if got := rec.Header().Get("WWW-Authenticate"); got != "" {
			t.Errorf("a %s link was challenged: %q", name, got)
		}
	}
}

// TestBasicStillAsks is the half that must not have moved: nothing about a
// request without a link changed.
func TestBasicStillAsks(t *testing.T) {
	t.Parallel()
	h, _ := signedServer(t)

	rec := signedGet(t, h, http.MethodGet, "/dav/notes.txt")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no credentials = %d, want 401", rec.Code)
	}
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Error("no challenge on a request that has to authenticate")
	}
}

// mkcol makes a folder, with a password for the same reason put uses one.
func mkcol(t *testing.T, h http.Handler, target string) {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), "MKCOL", target, nil)
	req.SetBasicAuth("edu", sharePassword)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("MKCOL %s = %d", target, rec.Code)
	}
}

// put writes a file the way the other tests in this package do, with a
// password, since making the fixture is not what is under test.
func put(t *testing.T, h http.Handler, target, body string) {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, target, strings.NewReader(body))
	req.SetBasicAuth("edu", sharePassword)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT %s = %d", target, rec.Code)
	}
}
