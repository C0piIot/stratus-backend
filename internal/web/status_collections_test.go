package web_test

import (
	"strings"
	"testing"

	"github.com/C0piIot/stratus-backend/internal/auth"
	"github.com/C0piIot/stratus-backend/internal/calendar"
	"github.com/C0piIot/stratus-backend/internal/contacts"
	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/db/dbtest"
	"github.com/C0piIot/stratus-backend/internal/web"
)

// TestStatusCountsCalendarsAndContacts: each calendar and address book is a
// row with what it holds and when that last changed, on the page and not in
// the fragment htmx refreshes.
func TestStatusCountsCalendarsAndContacts(t *testing.T) {
	t.Parallel()
	h, _, meta := browserOver(t)
	cookie := signIn(t, h)
	putEvent(t, meta, calendar.DefaultName, "one", "DTSTART:20240104T090000Z", "SUMMARY:One")
	putEvent(t, meta, calendar.DefaultName, "two", "DTSTART:20240105T090000Z", "SUMMARY:Two")
	if _, err := calendar.New(meta).Create(t.Context(), username, db.Calendar{Name: "tasks", DisplayName: "Chores", Components: "VTODO"}); err != nil {
		t.Fatal(err)
	}
	putICS(t, meta, "tasks", "milk.ics", "BEGIN:VCALENDAR\nVERSION:2.0\nPRODID:-//test//EN\nBEGIN:VTODO\nUID:milk\nDTSTAMP:20240101T000000Z\nSUMMARY:Milk\nEND:VTODO\nEND:VCALENDAR\n")
	putContact(t, meta, contacts.DefaultName, "ada", "FN:Ada Lovelace")

	body := get(t, h, "/status", cookie).Body.String()
	for _, want := range []string{
		"Calendars and contacts",
		`<a href="/calendars/edu/default/">Calendar</a></dt>`, "2 events",
		`<a href="/calendars/edu/tasks/">Chores</a></dt>`, "1 task",
		`<a href="/contacts/edu/contacts/">Contacts</a></dt>`, "1 contact", "last changed 20",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the status page has no %q", want)
		}
	}
	if fragment := getHTMX(t, h, "/status", cookie).Body.String(); strings.Contains(fragment, "data-collections") {
		t.Error("the fragment htmx refreshes counts the calendars every thirty seconds")
	}
}

// TestStatusWithoutCalendarsOrContacts: an empty calendar says so, and a
// database that will not list them leaves the rows out rather than failing
// the page.
func TestStatusWithoutCalendarsOrContacts(t *testing.T) {
	t.Parallel()
	h, _, _ := browserOver(t)
	if body := get(t, h, "/status", signIn(t, h)).Body.String(); !strings.Contains(body, ">empty<") {
		t.Errorf("an empty calendar does not say it is empty:\n%s", body)
	}

	for _, method := range []string{"Calendars", "CalendarObjects", "AddressBooks", "Contacts"} {
		t.Run(method, func(t *testing.T) {
			t.Parallel()
			s, thumbs, meta := pieces(t)
			broken := dbtest.FailOn(t, meta, method)
			creds := credentials()
			h := web.Handler(web.Deps{
				Version: version, BuildDate: buildDate, Verifier: creds,
				Sessions: auth.NewSessions(creds, auth.DefaultSessionTTL), Shares: auth.NewShares(creds),
				Files: s, Thumbs: thumbs, Indexing: indexing(meta), Playback: web.Playback{},
				Calendars: calendar.New(broken), Contacts: contacts.New(broken),
			})
			rec := get(t, h, "/status", signIn(t, h))
			if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Status") {
				t.Errorf("the status page with %s broken = %d", method, rec.Code)
			}
		})
	}
}
