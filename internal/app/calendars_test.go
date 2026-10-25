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

	// One address, two protocols (#111): a browser asking for a calendar gets
	// the month, and a client asking for an object by GET still gets the .ics.
	ics := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\nBEGIN:VEVENT\r\nUID:a\r\n" +
		"DTSTAMP:20240101T000000Z\r\nDTSTART:20240305T090000Z\r\nSUMMARY:Standup\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	put := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/calendars/edu/default/a.ics", strings.NewReader(ics))
	put.Header.Set("Content-Type", "text/calendar")
	put.SetBasicAuth("edu", password)
	created := httptest.NewRecorder()
	h.ServeHTTP(created, put)
	if created.Code != http.StatusCreated {
		t.Fatalf("PUT = %d", created.Code)
	}
	for target, want := range map[string]string{
		"/calendars/edu/default/?month=2024-03": "text/html",
		"/calendars/edu/default/a.ics":          "text/calendar",
	} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
		req.SetBasicAuth("edu", password)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), want) {
			t.Errorf("GET %s = %d %q, want 200 %s", target, rec.Code, rec.Header().Get("Content-Type"), want)
		}
	}
}

// TestContactsAreWiredFromTheOrigin is the same for address books (#423).
func TestContactsAreWiredFromTheOrigin(t *testing.T) {
	t.Parallel()
	blobs, meta := filestest.Backends(t)
	const password = "an example password"
	cfg := runConfig(t, map[string]string{"STRATUS_USERNAME": "edu", "STRATUS_PASSWORD": password})
	h := app.New(cfg, "test", "2026-01-01T09:30:00Z").Handler(app.Deps{
		Storage: blobs, Database: meta, Files: files.New(blobs, meta),
	})
	propfind := func(target string, depth string, basic bool) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequestWithContext(t.Context(), "PROPFIND", target, nil)
		req.Header.Set("Depth", depth)
		if basic {
			req.SetBasicAuth("edu", password)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	if rec := propfind("/.well-known/carddav", "0", false); rec.Code != http.StatusPermanentRedirect ||
		rec.Header().Get("Location") != "/contacts/" {
		t.Fatalf("well-known = %d to %q, want 308 to /contacts/", rec.Code, rec.Header().Get("Location"))
	}
	if rec := propfind("/contacts/", "0", false); rec.Code != http.StatusUnauthorized {
		t.Errorf("the principal without credentials = %d, want 401", rec.Code)
	}
	if rec := propfind("/contacts", "0", true); rec.Code != http.StatusMultiStatus || !strings.Contains(rec.Body.String(), "/contacts/edu/") {
		t.Errorf("the principal without its slash = %d, want 207 naming the home set:\n%s", rec.Code, rec.Body.String())
	}
	if rec := propfind("/", "1", true); !strings.Contains(rec.Body.String(), "/contacts/") {
		t.Errorf("the origin does not list the contacts:\n%s", rec.Body.String())
	}
}
