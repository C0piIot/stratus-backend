package dav_test

import (
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
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

func syncReport(token, level string) string {
	return `<?xml version="1.0" encoding="utf-8"?>
<D:sync-collection xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <D:sync-token>` + token + `</D:sync-token>
  <D:sync-level>` + level + `</D:sync-level>
  <D:prop><D:getetag/><C:calendar-data/><D:displayname/></D:prop>
</D:sync-collection>`
}

var syncTokenRE = regexp.MustCompile(`<sync-token xmlns="DAV:">([^<]+)</sync-token>`)

// wellFormed is what a client's parser asks first of a multistatus this
// package rewrote rather than the library wrote.
func wellFormed(t *testing.T, body string) {
	t.Helper()
	d := xml.NewDecoder(strings.NewReader(body))
	for {
		_, err := d.Token()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			t.Fatalf("not well-formed XML: %v\n%s", err, body)
		}
	}
}

// TestACalendarSyncsByToken is the third step of #75: a client that has a
// token is told what changed since it, deletions included, and nothing when
// nothing did -- and one whose token cannot be answered is told to start over.
func TestACalendarSyncsByToken(t *testing.T) {
	t.Parallel()
	_, meta := filestest.Backends(t)
	svc := calendar.New(meta)
	h := withUser(dav.CalDAV("/calendars/", svc), "edu")
	ctx := t.Context()
	put := func(name, uid, start string) {
		t.Helper()
		if _, err := svc.Put(ctx, "edu", calendar.DefaultName, name, icsEvent(t, uid, start, ""), nil); err != nil {
			t.Fatal(err)
		}
	}
	report := func(token, level string) (*httptest.ResponseRecorder, string) {
		t.Helper()
		rec := do(t, h, "REPORT", "/calendars/edu/default/", syncReport(token, level), "Content-Type", "application/xml")
		wellFormed(t, rec.Body.String())
		m := syncTokenRE.FindStringSubmatch(rec.Body.String())
		if m == nil {
			return rec, ""
		}
		return rec, m[1]
	}

	put("a.ics", "a", "20240102T090000Z")
	put("b.ics", "b", "20240103T090000Z")

	// What a client asks before it syncs at all.
	props := `<?xml version="1.0" encoding="utf-8"?>
<D:propfind xmlns:D="DAV:" xmlns:CS="http://calendarserver.org/ns/">
  <D:prop><D:displayname/><D:sync-token/><CS:getctag/><D:supported-report-set/><D:quota-used-bytes/></D:prop>
</D:propfind>`
	rec := do(t, h, "PROPFIND", "/calendars/edu/default/", props, "Depth", "0", "Content-Type", "application/xml")
	body := rec.Body.String()
	wellFormed(t, body)
	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("PROPFIND = %d:\n%s", rec.Code, body)
	}
	ok, missing, _ := strings.Cut(body, "200 OK")
	for _, want := range []string{"Calendar", "urn:stratus:sync:", "getctag", "<sync-collection", "calendar-multiget"} {
		if !strings.Contains(ok, want) {
			t.Errorf("the 200 propstat lacks %q:\n%s", want, body)
		}
	}
	if !strings.Contains(missing, "quota-used-bytes") || strings.Contains(missing, "sync-token") ||
		strings.Contains(body, `xmlns="DAV:" xmlns="DAV:"`) {
		t.Errorf("the 404 propstat is wrong:\n%s", body)
	}
	home := do(t, h, "PROPFIND", "/calendars/edu/", props, "Depth", "1", "Content-Type", "application/xml")
	wellFormed(t, home.Body.String())
	if strings.Count(home.Body.String(), "urn:stratus:sync:") != 2 {
		t.Errorf("listing the calendars does not carry each one's token twice, as sync-token and ctag:\n%s", home.Body.String())
	}

	first, token := report("", "1")
	if first.Code != http.StatusMultiStatus || token == "" {
		t.Fatalf("first sync = %d:\n%s", first.Code, first.Body.String())
	}
	for _, want := range []string{"/calendars/edu/default/a.ics", "/calendars/edu/default/b.ics", "BEGIN:VCALENDAR"} {
		if !strings.Contains(first.Body.String(), want) {
			t.Errorf("the first sync lacks %q:\n%s", want, first.Body.String())
		}
	}

	put("c.ics", "c", "20240104T090000Z")
	put("a.ics", "a", "20240105T090000Z")
	if err := svc.Delete(ctx, "edu", calendar.DefaultName, "b.ics"); err != nil {
		t.Fatal(err)
	}
	second, next := report(token, "1")
	got := second.Body.String()
	if second.Code != http.StatusMultiStatus || next == token {
		t.Fatalf("second sync = %d, token %q after %q:\n%s", second.Code, next, token, got)
	}
	if strings.Count(got, "<response") != 3 || !strings.Contains(got, "c.ics") || !strings.Contains(got, "a.ics") {
		t.Errorf("the second sync is not the three changes:\n%s", got)
	}
	if !regexp.MustCompile(`b\.ics</href>\s*<status xmlns="DAV:">HTTP/1.1 404`).MatchString(got) {
		t.Errorf("the deletion is not a 404:\n%s", got)
	}

	third, same := report(next, "1")
	if strings.Contains(third.Body.String(), "<response") || same != next {
		t.Errorf("a sync with nothing new = %q:\n%s", same, third.Body.String())
	}

	for _, tc := range []struct{ token, level string }{
		{"not a token", "1"},
		{"urn:stratus:sync:999:1", "1"},
		{next + "0", "1"},
		{next, "infinite"},
	} {
		refused, _ := report(tc.token, tc.level)
		if refused.Code != http.StatusForbidden || !strings.Contains(refused.Body.String(), "<valid-sync-token/>") && tc.level == "1" {
			t.Errorf("sync with %q at level %s = %d:\n%s", tc.token, tc.level, refused.Code, refused.Body.String())
		}
	}

	// A deletion forgotten makes every token from before it unanswerable.
	if _, err := meta.PruneCalendarTombstones(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if stale, _ := report(token, "1"); stale.Code != http.StatusForbidden {
		t.Errorf("sync from before a forgotten deletion = %d, want 403", stale.Code)
	}
	if fresh, _ := report(next, "1"); fresh.Code != http.StatusMultiStatus {
		t.Errorf("sync from after it = %d, want 207", fresh.Code)
	}

	// The rest of what a synced object can be asked about, and a property it
	// cannot, which is a 404 of its own beside them.
	asked := `<?xml version="1.0" encoding="utf-8"?>
<D:sync-collection xmlns:D="DAV:"><D:sync-token/><D:sync-level>1</D:sync-level>
  <D:prop><D:getcontenttype/><D:getcontentlength/><D:getlastmodified/><D:owner/></D:prop>
</D:sync-collection>`
	rec = do(t, h, "REPORT", "/calendars/edu/default/", asked, "Content-Type", "application/xml")
	wellFormed(t, rec.Body.String())
	for _, want := range []string{"text/calendar", "<getcontentlength", "GMT</getlastmodified>", "<owner", "404 Not Found"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("a sync asking for more lacks %q:\n%s", want, rec.Body.String())
		}
	}

	// Only the three a library does not know, so there is no 200 to join.
	only := `<?xml version="1.0" encoding="utf-8"?>
<D:propfind xmlns:D="DAV:"><D:prop><D:sync-token/></D:prop></D:propfind>`
	rec = do(t, h, "PROPFIND", "/calendars/edu/default/", only, "Depth", "0", "Content-Type", "application/xml")
	wellFormed(t, rec.Body.String())
	if !strings.Contains(rec.Body.String(), "200 OK") || strings.Contains(rec.Body.String(), "404") {
		t.Errorf("PROPFIND for the token alone:\n%s", rec.Body.String())
	}

	// A calendar nobody made is the library's 404 and this package's alike.
	if rec := do(t, h, "PROPFIND", "/calendars/edu/nope/", only, "Depth", "0", "Content-Type", "application/xml"); rec.Code != http.StatusNotFound {
		t.Errorf("PROPFIND for the token of a calendar nobody made = %d, want 404", rec.Code)
	}
	if rec := do(t, h, "REPORT", "/calendars/edu/nope/", syncReport("", "1"), "Content-Type", "application/xml"); rec.Code != http.StatusNotFound {
		t.Errorf("sync of a calendar nobody made = %d, want 404", rec.Code)
	}
}
