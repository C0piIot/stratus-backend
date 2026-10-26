package web_test

import (
	"context"
	"errors"
	htmlstd "html"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/stratus-private-cloud/stratus-backend/internal/auth"
	"github.com/stratus-private-cloud/stratus-backend/internal/calendar"
	"github.com/stratus-private-cloud/stratus-backend/internal/contacts"
	"github.com/stratus-private-cloud/stratus-backend/internal/db"
	"github.com/stratus-private-cloud/stratus-backend/internal/web"
)

// TestTheSearchFindsEventsAndContacts: the box answers an event by its title,
// place or description and a contact by name, company, email or note --
// whole words in order, case and accents ignored -- each linking to its form.
func TestTheSearchFindsEventsAndContacts(t *testing.T) {
	t.Parallel()
	h, _, meta := browserOver(t)
	cookie := signIn(t, h)
	putEvent(t, meta, calendar.DefaultName, "lunch", "DTSTART:20240305T120000Z", "SUMMARY:Lunch with Doc Brown",
		"LOCATION:Lou's Café")
	putEvent(t, meta, calendar.DefaultName, "standup", "DTSTART;VALUE=DATE:20240104", "RRULE:FREQ=WEEKLY",
		"SUMMARY:Standup", "DESCRIPTION:Bring the flux capacitor")
	putEvent(t, meta, calendar.DefaultName, "other", "DTSTART:20240306T120000Z", "SUMMARY:Brownies")
	putContact(t, meta, contacts.DefaultName, "doc", "FN:Emmett Brown", "ORG:Brown Laboratories;R&D",
		"EMAIL:doc@example.com")
	putContact(t, meta, contacts.DefaultName, "amelie", "FN:Amélie Poulain", "NOTE:Garden gnome on holiday")

	search := func(q string) string {
		t.Helper()
		rec := get(t, h, "/search?q="+strings.ReplaceAll(q, " ", "+"), cookie)
		if rec.Code != http.StatusOK {
			t.Fatalf("searching %q = %d", q, rec.Code)
		}
		return htmlstd.UnescapeString(rec.Body.String())
	}

	body := search("doc brown")
	for _, want := range []string{
		"Calendar", "Lunch with Doc Brown", `href="/event/edit/default/lunch.ics"`, "Tue 5 Mar 2024, ",
		"Contacts", "No event says that.",
	} {
		if want == "No event says that." {
			if strings.Contains(body, want) {
				t.Errorf("a search that found an event says it found none")
			}
			continue
		}
		if !strings.Contains(body, want) {
			t.Errorf("searching doc brown: no %q", want)
		}
	}
	if strings.Contains(body, "Brownies") {
		t.Error("a word matched part of another one")
	}

	for q, want := range map[string]string{
		"brown":              `href="/contact/edit/contacts/doc.vcf"`,
		"brown laboratories": "Emmett Brown",
		"doc example com":    "Emmett Brown",
		"amelie":             "Amélie Poulain",
		"GNOME":              "Amélie Poulain",
		"lou s cafe":         "Lunch with Doc Brown",
		"flux capacitor":     "Standup",
	} {
		if got := search(q); !strings.Contains(got, want) {
			t.Errorf("searching %q found no %q", q, want)
		}
	}
	if got := search("standup"); !strings.Contains(got, "Thu 4 Jan 2024<") || !strings.Contains(got, "repeats") {
		t.Errorf("an all-day repeating event is not shown as one:\n%s", got)
	}
	if got := search("capacitor flux"); strings.Contains(got, "Standup") {
		t.Error("words out of order matched")
	}
	if got := search("nothing like it"); !strings.Contains(got, "No event says that.") || !strings.Contains(got, "Nobody is called that.") {
		t.Error("a search that found nothing does not say so")
	}

	// Narrowed to one, the page shows that bucket alone.
	body = htmlstd.UnescapeString(get(t, h, "/search?q=brown&in=contacts", cookie).Body.String())
	if !strings.Contains(body, "Emmett Brown") || strings.Contains(body, "Lunch with Doc Brown") || strings.Contains(body, `text-body-secondary">Files</h2>`) {
		t.Errorf("the page narrowed to contacts:\n%s", body)
	}
}

// TestASearchOfManyEventsLinksToTheRest: the page shows twenty of each and
// a link to the page narrowed to them, which shows them all.
func TestASearchOfManyEventsLinksToTheRest(t *testing.T) {
	t.Parallel()
	h, _, meta := browserOver(t)
	cookie := signIn(t, h)
	for i := range 25 {
		n := strconv.Itoa(i)
		putEvent(t, meta, calendar.DefaultName, "e"+n, "DTSTART:202403"+strconv.Itoa(10+i%20)+"T120000Z", "SUMMARY:Review "+n)
		putContact(t, meta, contacts.DefaultName, "c"+n, "FN:Reviewer "+n, "NOTE:review")
	}
	body := get(t, h, "/search?q=review", cookie).Body.String()
	if got := strings.Count(body, `href="/event/edit/`); got != 20 {
		t.Errorf("the page shows %d events, want 20", got)
	}
	if got := strings.Count(body, `href="/contact/edit/`); got != 20 {
		t.Errorf("the page shows %d contacts, want 20", got)
	}
	for _, want := range []string{`href="/search?in=events&amp;q=review">More events`, `href="/search?in=contacts&amp;q=review">More contacts`} {
		if !strings.Contains(body, want) {
			t.Errorf("the page has no %s", want)
		}
	}
	if got := strings.Count(get(t, h, "/search?q=review&in=events", cookie).Body.String(), `href="/event/edit/`); got != 25 {
		t.Errorf("the page narrowed to events shows %d, want 25", got)
	}
}

// brokenCalendars and brokenContacts refuse the listing a search reads first,
// or the one under it.
type brokenCalendars struct {
	*calendar.Service
	objects bool
}

func (b brokenCalendars) Calendars(ctx context.Context, owner string) ([]db.Calendar, error) {
	if b.objects {
		return b.Service.Calendars(ctx, owner)
	}
	return nil, errors.New("no calendars today")
}

func (b brokenCalendars) Objects(context.Context, string, string) ([]db.CalendarObject, error) {
	return nil, errors.New("no events today")
}

type brokenContacts struct {
	*contacts.Service
	cards bool
}

func (b brokenContacts) AddressBooks(ctx context.Context, owner string) ([]db.AddressBook, error) {
	if b.cards {
		return b.Service.AddressBooks(ctx, owner)
	}
	return nil, errors.New("no address books today")
}

func (b brokenContacts) Contacts(context.Context, string, string) ([]db.Contact, error) {
	return nil, errors.New("no contacts today")
}

// TestASearchSurvivesWhatItCannotRead: a calendar or an address book that
// will not list, and an object that will not decode, leave the rest of the
// answer standing -- the files are what the box was for first.
func TestASearchSurvivesWhatItCannotRead(t *testing.T) {
	t.Parallel()
	s, thumbs, meta := pieces(t)
	cal := calendar.New(meta)
	c, err := cal.Calendar(t.Context(), username, calendar.DefaultName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = meta.PutCalendarObject(t.Context(), db.CalendarObject{CalendarID: c.ID, Name: "x.ics", UID: "x", Component: "VEVENT", Data: "not iCalendar"}); err != nil {
		t.Fatal(err)
	}
	book, err := contacts.New(meta).AddressBook(t.Context(), username, contacts.DefaultName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = meta.PutContact(t.Context(), db.Contact{AddressBookID: book.ID, Name: "x.vcf", UID: "x", Data: "not a vCard"}); err != nil {
		t.Fatal(err)
	}
	putICS(t, meta, calendar.DefaultName, "moved.ics", "BEGIN:VCALENDAR\nVERSION:2.0\nPRODID:-//test//EN\nBEGIN:VTODO\nUID:moved\nDTSTAMP:20240101T000000Z\nSUMMARY:Untitled task\nEND:VTODO\nEND:VCALENDAR\n")
	putEvent(t, meta, calendar.DefaultName, "bare", "LOCATION:Untitled place")

	creds := credentials()
	for name, deps := range map[string][2]any{
		"working":    {cal, contacts.New(meta)},
		"no listing": {brokenCalendars{Service: cal}, brokenContacts{Service: contacts.New(meta)}},
		"no objects": {brokenCalendars{Service: cal, objects: true}, brokenContacts{Service: contacts.New(meta), cards: true}},
	} {
		h := web.Handler(web.Deps{
			Version: version, BuildDate: buildDate, Verifier: creds,
			Sessions: auth.NewSessions(creds, auth.DefaultSessionTTL), Shares: auth.NewShares(creds),
			Files: s, Thumbs: thumbs, Index: meta, Indexing: indexing(meta), Playback: web.Playback{},
			Calendars: deps[0].(web.Calendars), Contacts: deps[1].(web.Contacts),
		})
		rec := get(t, h, "/search?q=untitled", signIn(t, h))
		if rec.Code != http.StatusOK {
			t.Errorf("%s: a search = %d", name, rec.Code)
		}
		if name == "working" && (!strings.Contains(rec.Body.String(), "(no title)") || strings.Contains(rec.Body.String(), "Untitled task")) {
			t.Errorf("an event with no title or date is not found as one, or a task is:\n%s", rec.Body)
		}
	}

	h, _, _ := browserOver(t)
	if body := get(t, h, "/search?q=%21%21%21", signIn(t, h)).Body.String(); !strings.Contains(body, "No event says that.") {
		t.Error("a term with no words in it matched something")
	}
}
