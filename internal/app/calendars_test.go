package app_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/C0piIot/stratus-backend/internal/app"
	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/files/filestest"
)

// TestCalendarsAreWiredFromTheOrigin is the half of #75 a handler cannot test
// by itself: a client given only the server's address is pointed at the
// calendars without being asked who it is, then asked, then shown where its
// calendars are -- and the origin lists them beside the other collections.
func TestCalendarsAreWiredFromTheOrigin(t *testing.T) {
	t.Parallel()
	blobs, meta := filestest.Backends(t)
	const password = "an example password"
	cfg := runConfig(t, map[string]string{"STRATUS_USERNAME": "edu", "STRATUS_PASSWORD": password})
	h := app.New(cfg, "test", "2026-01-01T09:30:00Z").Handler(app.Deps{
		Storage: blobs, Database: meta, Files: files.New(blobs, meta),
	})

	propfind := func(target string, basic bool) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequestWithContext(t.Context(), "PROPFIND", target, nil)
		req.Header.Set("Depth", "0")
		if basic {
			req.SetBasicAuth("edu", password)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// RFC 6764's address keeps the method, which is a PROPFIND while a client
	// is discovering, and says nothing that needs a password.
	if rec := propfind("/.well-known/caldav", false); rec.Code != http.StatusPermanentRedirect ||
		rec.Header().Get("Location") != "/calendars/" {
		t.Fatalf("well-known = %d to %q, want 308 to /calendars/", rec.Code, rec.Header().Get("Location"))
	}
	if rec := propfind("/calendars/", false); rec.Code != http.StatusUnauthorized {
		t.Errorf("the principal without credentials = %d, want 401", rec.Code)
	}
	if rec := propfind("/calendars", true); rec.Code != http.StatusMultiStatus || !strings.Contains(rec.Body.String(), "/calendars/edu/") {
		t.Errorf("the principal without its slash = %d, want 207 naming the home set", rec.Code)
	}
	rec := propfind("/calendars/", true)
	body, _ := io.ReadAll(rec.Body)
	if rec.Code != http.StatusMultiStatus || !strings.Contains(string(body), "/calendars/edu/") {
		t.Errorf("the principal = %d, want 207 naming the home set:\n%s", rec.Code, body)
	}

	req := httptest.NewRequestWithContext(t.Context(), "PROPFIND", "/", nil)
	req.Header.Set("Depth", "1")
	req.SetBasicAuth("edu", password)
	listing := httptest.NewRecorder()
	h.ServeHTTP(listing, req)
	if !strings.Contains(listing.Body.String(), "/calendars/") {
		t.Errorf("the origin does not list the calendars:\n%s", listing.Body.String())
	}
}
