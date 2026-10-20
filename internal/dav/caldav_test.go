package dav_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-ical"
	"github.com/emersion/go-webdav/caldav"

	"github.com/C0piIot/stratus-backend/internal/calendar"
	"github.com/C0piIot/stratus-backend/internal/dav"
	"github.com/C0piIot/stratus-backend/internal/files/filestest"
)

func icsEvent(t *testing.T, uid, start, extra string) *ical.Calendar {
	t.Helper()
	text := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\nBEGIN:VEVENT\r\nUID:" + uid +
		"\r\nDTSTAMP:20240101T080000Z\r\nDTSTART:" + start + "\r\nDURATION:PT30M\r\n" + extra +
		"SUMMARY:" + uid + "\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	cal, err := ical.NewDecoder(strings.NewReader(text)).Decode()
	if err != nil {
		t.Fatal(err)
	}
	return cal
}

// TestACalendarClientFindsItsCalendars is #75 driven by the library's own
// client: it finds the calendars from the principal, writes to one, and finds a
// recurring event in a month it only recurs into -- which is the one thing a
// query has to get right and a date column alone gets wrong.
func TestACalendarClientFindsItsCalendars(t *testing.T) {
	t.Parallel()
	_, meta := filestest.Backends(t)
	srv := httptest.NewServer(withUser(dav.CalDAV("/calendars/", calendar.New(meta)), "edu"))
	t.Cleanup(srv.Close)
	hc := srv.Client()
	ctx := t.Context()

	discovering, err := caldav.NewClient(hc, srv.URL+"/calendars/")
	if err != nil {
		t.Fatal(err)
	}
	principal, err := discovering.FindCurrentUserPrincipal(ctx)
	if err != nil {
		t.Fatalf("FindCurrentUserPrincipal: %v", err)
	}
	client, err := caldav.NewClient(hc, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	home, err := client.FindCalendarHomeSet(ctx, principal)
	if err != nil {
		t.Fatalf("FindCalendarHomeSet(%q): %v", principal, err)
	}
	if home != "/calendars/edu/" {
		t.Errorf("home set = %q, want /calendars/edu/", home)
	}
	cals, err := client.FindCalendars(ctx, home)
	if err != nil {
		t.Fatalf("FindCalendars: %v", err)
	}
	if len(cals) != 1 || cals[0].Path != "/calendars/edu/default/" {
		t.Fatalf("FindCalendars = %+v, want the default calendar", cals)
	}
	if got := cals[0].SupportedComponentSet; len(got) != 1 || got[0] != "VEVENT" {
		t.Errorf("supported components = %v, want VEVENT alone", got)
	}
	calPath := cals[0].Path

	weekly, err := client.PutCalendarObject(ctx, calPath+"standup.ics",
		icsEvent(t, "standup", "20240102T090000Z", "RRULE:FREQ=WEEKLY\r\n"))
	if err != nil {
		t.Fatalf("PutCalendarObject: %v", err)
	}
	if _, err = client.PutCalendarObject(ctx, calPath+"once.ics", icsEvent(t, "once", "20240110T090000Z", "")); err != nil {
		t.Fatalf("PutCalendarObject: %v", err)
	}

	got, err := client.GetCalendarObject(ctx, calPath+"standup.ics")
	if err != nil {
		t.Fatalf("GetCalendarObject: %v", err)
	}
	if got.ETag != weekly.ETag || got.ETag == "" {
		t.Errorf("GET's ETag %q, PUT's %q: want the same one", got.ETag, weekly.ETag)
	}
	if uid, _ := got.Data.Events()[0].Props.Text(ical.PropUID); uid != "standup" {
		t.Errorf("GET returned UID %q", uid)
	}

	march, err := client.QueryCalendar(ctx, calPath, &caldav.CalendarQuery{
		CompRequest: caldav.CalendarCompRequest{Name: "VCALENDAR", Comps: []caldav.CalendarCompRequest{{Name: "VEVENT", AllProps: true}}},
		CompFilter: caldav.CompFilter{Name: "VCALENDAR", Comps: []caldav.CompFilter{{
			Name:  "VEVENT",
			Start: time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC),
			End:   time.Date(2024, 4, 1, 0, 0, 0, 0, time.UTC),
		}}},
	})
	if err != nil {
		t.Fatalf("QueryCalendar: %v", err)
	}
	if len(march) != 1 || march[0].Path != calPath+"standup.ics" {
		var paths []string
		for _, o := range march {
			paths = append(paths, o.Path)
		}
		t.Errorf("March holds %v, want the weekly standup alone", paths)
	}

	put := func(name, ifMatch string, cal *ical.Calendar) int {
		t.Helper()
		var body strings.Builder
		if eerr := ical.NewEncoder(&body).Encode(cal); eerr != nil {
			t.Fatal(eerr)
		}
		r, rerr := http.NewRequestWithContext(ctx, http.MethodPut, srv.URL+calPath+name, strings.NewReader(body.String()))
		if rerr != nil {
			t.Fatal(rerr)
		}
		r.Header.Set("Content-Type", ical.MIMEType)
		if ifMatch != "" {
			r.Header.Set("If-Match", ifMatch)
		}
		answer, rerr := hc.Do(r)
		if rerr != nil {
			t.Fatal(rerr)
		}
		_ = answer.Body.Close()
		return answer.StatusCode
	}
	moved := icsEvent(t, "standup", "20240102T100000Z", "RRULE:FREQ=WEEKLY\r\n")
	if code := put("standup.ics", `"somebody else's"`, moved); code != http.StatusPreconditionFailed {
		t.Errorf("PUT over a changed object = %d, want 412", code)
	}
	if code := put("standup.ics", `"`+weekly.ETag+`"`, moved); code != http.StatusCreated {
		t.Errorf("PUT over the object it read = %d, want 201", code)
	}
	if code := put("again.ics", "", icsEvent(t, "standup", "20240102T090000Z", "")); code != http.StatusConflict {
		t.Errorf("PUT of a UID another object holds = %d, want 409", code)
	}
	task := icsEvent(t, "task", "20240102T090000Z", "")
	task.Children[0].Name = "VTODO"
	if code := put("task.ics", "", task); code != http.StatusConflict {
		t.Errorf("PUT of a task = %d, want 409 with supported-calendar-component", code)
	}

	if err = client.RemoveAll(ctx, calPath+"once.ics"); err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	if _, err = client.GetCalendarObject(ctx, calPath+"once.ics"); err == nil {
		t.Error("a deleted object is still served")
	}

	// Somebody else's calendars are not there, rather than forbidden.
	req, err := http.NewRequestWithContext(ctx, "PROPFIND", srv.URL+"/calendars/someone/default/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Depth", "0")
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("PROPFIND of another owner's calendar = %d, want 404", resp.StatusCode)
	}
}

// TestCalDAVEdges are the answers a client gets off the path above: a calendar
// made with the extended MKCOL, one that is not there, and a request nobody
// authenticated.
func TestCalDAVEdges(t *testing.T) {
	t.Parallel()
	_, meta := filestest.Backends(t)
	h := withUser(dav.CalDAV("/calendars/", calendar.New(meta)), "edu")

	mkcol := `<?xml version="1.0" encoding="utf-8"?>
<D:mkcol xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <D:set><D:prop>
    <D:resourcetype><D:collection/><C:calendar/></D:resourcetype>
    <D:displayname>Work</D:displayname>
  </D:prop></D:set>
</D:mkcol>`
	if rec := do(t, h, "MKCOL", "/calendars/edu/work/", mkcol, "Content-Type", "application/xml"); rec.Code != http.StatusCreated {
		t.Fatalf("MKCOL = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, h, "MKCOL", "/calendars/edu/work/", mkcol, "Content-Type", "application/xml"); rec.Code != http.StatusConflict {
		t.Errorf("a second MKCOL = %d, want 409", rec.Code)
	}
	rec := do(t, h, "PROPFIND", "/calendars/edu/work", "", "Depth", "0")
	if rec.Code != http.StatusMultiStatus || !strings.Contains(rec.Body.String(), "Work") {
		t.Errorf("PROPFIND of the new calendar without its slash = %d:\n%s", rec.Code, rec.Body.String())
	}

	for _, target := range []string{"/calendars/edu/nope/", "/calendars/edu/nope/a.ics", "/calendars/edu/a/b/c/d"} {
		if rec := do(t, h, "PROPFIND", target, "", "Depth", "1"); rec.Code != http.StatusNotFound {
			t.Errorf("PROPFIND %s = %d, want 404", target, rec.Code)
		}
	}
	if rec := do(t, h, http.MethodDelete, "/calendars/edu/default/nope.ics", ""); rec.Code != http.StatusNotFound {
		t.Errorf("DELETE of nothing = %d, want 404", rec.Code)
	}
	query := `<?xml version="1.0" encoding="utf-8"?>
<C:calendar-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <D:prop><D:getetag/></D:prop>
  <C:filter><C:comp-filter name="VCALENDAR"><C:comp-filter name="VEVENT"/></C:comp-filter></C:filter>
</C:calendar-query>`
	if rec := do(t, h, "REPORT", "/calendars/edu/nope/", query, "Depth", "1", "Content-Type", "application/xml"); rec.Code != http.StatusNotFound {
		t.Errorf("REPORT on a calendar nobody made = %d, want 404", rec.Code)
	}

	anonymous := dav.CalDAV("/calendars/", calendar.New(meta))
	for _, target := range []string{"/calendars/", "/calendars/edu/", "/calendars/edu/default/"} {
		if rec := do(t, anonymous, "PROPFIND", target, "", "Depth", "0"); rec.Code != http.StatusUnauthorized {
			t.Errorf("PROPFIND %s with nobody on the request = %d, want 401", target, rec.Code)
		}
	}
}
