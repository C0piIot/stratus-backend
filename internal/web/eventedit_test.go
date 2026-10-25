package web_test

import (
	htmlstd "html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-ical"

	"github.com/C0piIot/stratus-backend/internal/calendar"
	"github.com/C0piIot/stratus-backend/internal/db"
)

func putICS(t *testing.T, meta db.Store, cal, name, text string) {
	t.Helper()
	parsed, err := ical.NewDecoder(strings.NewReader(strings.ReplaceAll(text, "\n", "\r\n"))).Decode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := calendar.New(meta).Put(t.Context(), username, cal, name, parsed, nil); err != nil {
		t.Fatal(err)
	}
}

func storedEvent(t *testing.T, meta db.Store, name string) (*ical.Component, db.CalendarObject) {
	t.Helper()
	o, err := calendar.New(meta).Object(t.Context(), username, calendar.DefaultName, name)
	if err != nil {
		t.Fatal(err)
	}
	cal, err := calendar.Decode(o)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cal.Children {
		if c.Name == ical.CompEvent && c.Props.Get(ical.PropRecurrenceID) == nil {
			return c, o
		}
	}
	t.Fatalf("%s holds no event", name)
	return nil, o
}

var (
	inputField    = regexp.MustCompile(`<input[^>]*\bname="([^"]+)"[^>]*>`)
	inputValue    = regexp.MustCompile(`\bvalue="([^"]*)"`)
	textareaField = regexp.MustCompile(`(?s)<textarea[^>]*\bname="([^"]+)"[^>]*>(.*?)</textarea>`)
)

// formOf is what a browser would post back from a page untouched.
func formOf(t *testing.T, page string) url.Values {
	t.Helper()
	form := url.Values{}
	for _, m := range inputField.FindAllStringSubmatch(page, -1) {
		if strings.Contains(m[0], `type="checkbox"`) && !strings.Contains(m[0], " checked") {
			continue
		}
		v := ""
		if mv := inputValue.FindStringSubmatch(m[0]); mv != nil {
			v = htmlstd.UnescapeString(mv[1])
		}
		form.Add(m[1], v)
	}
	for _, m := range textareaField.FindAllStringSubmatch(page, -1) {
		form.Add(m[1], htmlstd.UnescapeString(m[2]))
	}
	return form
}

// TestANewEventIsWrittenAsAClientWould is #444's first form: what is typed
// becomes an iCalendar event under a UUID, in the calendar a client syncs,
// with its times in UTC.
func TestANewEventIsWrittenAsAClientWould(t *testing.T) {
	t.Parallel()
	h, _, meta := browserOver(t)
	cookie := signIn(t, h)

	month := get(t, h, "/calendars/edu/default/?month=2024-03", cookie).Body.String()
	for _, want := range []string{`href="/event/new/default">New event</a>`, `href="/event/new/default?date=2024-03-05"`} {
		if !strings.Contains(month, want) {
			t.Errorf("the month has no %s", want)
		}
	}
	form := get(t, h, "/event/new/default?date=2024-03-05", cookie)
	if form.Code != http.StatusOK || !strings.Contains(form.Body.String(), `name="start-date" type="date" value="2024-03-05"`) {
		t.Fatalf("the new event form = %d:\n%s", form.Code, form.Body)
	}

	rec := post(t, h, "/event/new/default", url.Values{
		"summary": {"Launch, finally"}, "start-date": {"2024-03-05"}, "start-time": {"09:30"},
		"end-date": {"2024-03-05"}, "end-time": {"11:00"}, "location": {"Room 2"},
		"description": {"Bring\r\nthe cake"},
	}, cookie)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/calendars/edu/default/?month=2024-03" {
		t.Fatalf("saving a new event = %d to %q:\n%s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}

	objects, err := calendar.New(meta).Objects(t.Context(), username, calendar.DefaultName)
	if err != nil || len(objects) != 1 {
		t.Fatalf("the calendar holds %d objects, %v", len(objects), err)
	}
	ev, o := storedEvent(t, meta, objects[0].Name)
	if o.Name != o.UID+".ics" || !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4`).MatchString(o.UID) {
		t.Errorf("the event is %q with UID %q", o.Name, o.UID)
	}
	start := time.Date(2024, 3, 5, 9, 30, 0, 0, time.Local).UTC().Format("20060102T150405Z")
	end := time.Date(2024, 3, 5, 11, 0, 0, 0, time.Local).UTC().Format("20060102T150405Z")
	for _, want := range []string{
		"VERSION:2.0", "PRODID:-//Stratus//Stratus//EN", "DTSTART:" + start, "DTEND:" + end,
		`SUMMARY:Launch\, finally`, "LOCATION:Room 2", `DESCRIPTION:Bring\nthe cake`, "CREATED:", "DTSTAMP:",
	} {
		if !strings.Contains(o.Data, want) {
			t.Errorf("the event has no %s:\n%s", want, o.Data)
		}
	}
	if ev.Props.Get(ical.PropSequence) != nil {
		t.Errorf("a new event carries a SEQUENCE:\n%s", o.Data)
	}

	page := htmlstd.UnescapeString(get(t, h, "/calendars/edu/default/?month=2024-03", cookie).Body.String())
	if !strings.Contains(page, `href="/event/edit/default/`+o.Name+`"`) || !strings.Contains(page, "Launch, finally") {
		t.Errorf("the month does not link the event:\n%s", page)
	}
}

// TestAnAllDayEventEndsOnItsLastDay: the form says the last day an event
// covers, and the iCalendar says the day after it.
func TestAnAllDayEventEndsOnItsLastDay(t *testing.T) {
	t.Parallel()
	h, _, meta := browserOver(t)
	cookie := signIn(t, h)

	rec := post(t, h, "/event/new/default", url.Values{
		"summary": {"Holiday"}, "allday": {"1"}, "start-date": {"2024-03-18"}, "start-time": {"09:00"},
		"end-date": {"2024-03-19"}, "end-time": {"10:00"},
	}, cookie)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("saving = %d:\n%s", rec.Code, rec.Body)
	}
	objects, _ := calendar.New(meta).Objects(t.Context(), username, calendar.DefaultName)
	_, o := storedEvent(t, meta, objects[0].Name)
	if !strings.Contains(o.Data, "DTSTART;VALUE=DATE:20240318") || !strings.Contains(o.Data, "DTEND;VALUE=DATE:20240320") {
		t.Errorf("the holiday is stored as:\n%s", o.Data)
	}
	form := get(t, h, "/event/edit/default/"+o.Name, cookie).Body.String()
	for _, want := range []string{`name="allday" value="1" checked`, `name="end-date" type="date" value="2024-03-19"`} {
		if !strings.Contains(form, want) {
			t.Errorf("the form has no %s", want)
		}
	}

	// One day, and an end left empty, is the start's own day.
	rec = post(t, h, "/event/new/default", url.Values{"summary": {"Day off"}, "allday": {"1"}, "start-date": {"2024-04-01"}}, cookie)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("saving one day = %d:\n%s", rec.Code, rec.Body)
	}
}

// TestAnEventEditKeepsWhatTheFormDoesNotShow: an alarm, the attendees, a
// client's own properties, the rule and the zone survive a web edit, and a
// form posted back untouched writes nothing.
func TestAnEventEditKeepsWhatTheFormDoesNotShow(t *testing.T) {
	t.Parallel()
	h, _, meta := browserOver(t)
	cookie := signIn(t, h)
	putICS(t, meta, calendar.DefaultName, "standup.ics", `BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//phone//EN
BEGIN:VTIMEZONE
TZID:Europe/Madrid
BEGIN:STANDARD
DTSTART:19701025T030000
TZOFFSETFROM:+0200
TZOFFSETTO:+0100
RRULE:FREQ=YEARLY;BYMONTH=10;BYDAY=-1SU
END:STANDARD
BEGIN:DAYLIGHT
DTSTART:19700329T020000
TZOFFSETFROM:+0100
TZOFFSETTO:+0200
RRULE:FREQ=YEARLY;BYMONTH=3;BYDAY=-1SU
END:DAYLIGHT
END:VTIMEZONE
BEGIN:VEVENT
UID:standup
DTSTAMP:20240101T000000Z
DTSTART;TZID=Europe/Madrid:20240104T090000
DURATION:PT30M
RRULE:FREQ=WEEKLY
SUMMARY:Standup
ATTENDEE;CN=Ada:mailto:ada@example.com
X-PHONE-COLOR:teal
BEGIN:VALARM
ACTION:DISPLAY
TRIGGER:-PT10M
DESCRIPTION:Standup
END:VALARM
END:VEVENT
END:VCALENDAR
`)
	_, was := storedEvent(t, meta, "standup.ics")

	page := get(t, h, "/event/edit/default/standup.ics", cookie)
	body := htmlstd.UnescapeString(page.Body.String())
	madrid, _ := time.LoadLocation("Europe/Madrid")
	start := time.Date(2024, 1, 4, 9, 0, 0, 0, madrid).In(time.Local)
	for _, want := range []string{
		"Repeats weekly", `value="Standup"`, `value="` + start.Format("15:04") + `"`,
		`value="` + start.Add(30*time.Minute).Format("15:04") + `"`, `href="/event/delete/default/standup.ics"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the form has no %q", want)
		}
	}

	untouched := formOf(t, page.Body.String())
	if rec := post(t, h, "/event/edit/default/standup.ics", untouched, cookie); rec.Code != http.StatusSeeOther ||
		rec.Header().Get("Location") != "/calendars/edu/default/?month=2024-01" {
		t.Fatalf("saving untouched = %d to %q:\n%s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	if _, o := storedEvent(t, meta, "standup.ics"); o.Data != was.Data {
		t.Errorf("saving untouched changed the event:\n%s\nto\n%s", was.Data, o.Data)
	}

	retitled := formOf(t, page.Body.String())
	retitled.Set("summary", "Daily standup")
	if rec := post(t, h, "/event/edit/default/standup.ics", retitled, cookie); rec.Code != http.StatusSeeOther {
		t.Fatalf("saving a title = %d:\n%s", rec.Code, rec.Body)
	}
	_, o := storedEvent(t, meta, "standup.ics")
	for _, want := range []string{
		"SUMMARY:Daily standup", "DTSTART;TZID=Europe/Madrid:20240104T090000", "DURATION:PT30M",
		"RRULE:FREQ=WEEKLY", "ATTENDEE;CN=Ada:mailto:ada@example.com", "X-PHONE-COLOR:teal",
		"TRIGGER:-PT10M", "BEGIN:VTIMEZONE", "SEQUENCE:1", "LAST-MODIFIED:",
	} {
		if !strings.Contains(o.Data, want) {
			t.Errorf("after a new title the event has no %s:\n%s", want, o.Data)
		}
	}
	if strings.Contains(o.Data, "20240101T000000Z") {
		t.Errorf("the DTSTAMP was not moved:\n%s", o.Data)
	}

	moved := formOf(t, get(t, h, "/event/edit/default/standup.ics", cookie).Body.String())
	later := time.Date(2024, 1, 4, 10, 0, 0, 0, madrid).In(time.Local)
	moved.Set("start-time", later.Format("15:04"))
	moved.Set("end-time", later.Add(time.Hour).Format("15:04"))
	if rec := post(t, h, "/event/edit/default/standup.ics", moved, cookie); rec.Code != http.StatusSeeOther {
		t.Fatalf("saving a time = %d:\n%s", rec.Code, rec.Body)
	}
	ev, o := storedEvent(t, meta, "standup.ics")
	for _, want := range []string{
		"DTSTART;TZID=Europe/Madrid:20240104T100000", "DTEND;TZID=Europe/Madrid:20240104T110000", "SEQUENCE:2", "RRULE:FREQ=WEEKLY",
	} {
		if !strings.Contains(o.Data, want) {
			t.Errorf("after a new time the event has no %s:\n%s", want, o.Data)
		}
	}
	if ev.Props.Get(ical.PropDuration) != nil {
		t.Errorf("a DURATION was left beside the DTEND:\n%s", o.Data)
	}
}

// TestAnEventOfAnotherShape: dates of its own instead of a rule, a rule the
// form has no word for, an object holding only an instance moved out of a
// series, and a field cleared.
func TestAnEventOfAnotherShape(t *testing.T) {
	t.Parallel()
	h, _, meta := browserOver(t)
	cookie := signIn(t, h)
	putEvent(t, meta, calendar.DefaultName, "dates", "DTSTART:20240104T090000Z", "RDATE:20240110T090000Z",
		"SUMMARY:Dates", "LOCATION:Room 2", "X-KEPT:yes")
	putEvent(t, meta, calendar.DefaultName, "hourly", "DTSTART:20240104T090000Z", "RRULE:FREQ=HOURLY;COUNT=3", "SUMMARY:Hourly")
	putEvent(t, meta, calendar.DefaultName, "moved", "RECURRENCE-ID:20240104T090000Z", "DTSTART:20240104T100000Z", "SUMMARY:Moved")

	page := get(t, h, "/event/edit/default/dates.ics", cookie)
	if !strings.Contains(page.Body.String(), "Repeats on chosen dates") {
		t.Errorf("an event with RDATE does not say it repeats:\n%s", page.Body)
	}
	if body := get(t, h, "/event/edit/default/hourly.ics", cookie).Body.String(); !strings.Contains(body, "Repeats:") {
		t.Errorf("an hourly event does not say it repeats:\n%s", body)
	}
	if body := get(t, h, "/event/edit/default/moved.ics", cookie).Body.String(); !strings.Contains(body, `value="Moved"`) {
		t.Errorf("an object holding one moved instance has no form:\n%s", body)
	}

	form := formOf(t, page.Body.String())
	form.Set("location", "")
	form.Set("description", "Agenda")
	if rec := post(t, h, "/event/edit/default/dates.ics", form, cookie); rec.Code != http.StatusSeeOther {
		t.Fatalf("saving = %d:\n%s", rec.Code, rec.Body)
	}
	ev, o := storedEvent(t, meta, "dates.ics")
	if ev.Props.Get(ical.PropLocation) != nil || !strings.Contains(o.Data, "DESCRIPTION:Agenda") ||
		!strings.Contains(o.Data, "RDATE:20240110T090000Z") || !strings.Contains(o.Data, "X-KEPT:yes") {
		t.Errorf("clearing the location stored:\n%s", o.Data)
	}
}

// TestAStaleEventFormSavesNothing is the form's If-Match, as a contact's is.
func TestAStaleEventFormSavesNothing(t *testing.T) {
	t.Parallel()
	h, _, meta := browserOver(t)
	cookie := signIn(t, h)
	putEvent(t, meta, calendar.DefaultName, "lunch", "DTSTART:20240305T120000Z", "SUMMARY:Lunch")
	form := formOf(t, get(t, h, "/event/edit/default/lunch.ics", cookie).Body.String())

	putEvent(t, meta, calendar.DefaultName, "lunch", "DTSTART:20240305T130000Z", "SUMMARY:Lunch, moved")
	_, phone := storedEvent(t, meta, "lunch.ics")
	form.Set("summary", "Lunch with Ada")
	rec := post(t, h, "/event/edit/default/lunch.ics", form, cookie)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "changed or deleted somewhere else") ||
		!strings.Contains(rec.Body.String(), `value="Lunch with Ada"`) {
		t.Errorf("a stale form = %d:\n%s", rec.Code, rec.Body)
	}
	if _, o := storedEvent(t, meta, "lunch.ics"); o.Data != phone.Data {
		t.Errorf("a stale form overwrote the phone's change:\n%s", o.Data)
	}

	if err := calendar.New(meta).Delete(t.Context(), username, calendar.DefaultName, "lunch.ics"); err != nil {
		t.Fatal(err)
	}
	if rec := post(t, h, "/event/edit/default/lunch.ics", form, cookie); rec.Code != http.StatusConflict {
		t.Errorf("a form for a deleted event = %d", rec.Code)
	}
	if rec := get(t, h, "/event/edit/default/lunch.ics", cookie); rec.Code != http.StatusNotFound {
		t.Errorf("the form for a deleted event = %d", rec.Code)
	}
}

func TestAnEventFormWithAMistakeSavesNothing(t *testing.T) {
	t.Parallel()
	h, _, meta := browserOver(t)
	cookie := signIn(t, h)
	putEvent(t, meta, calendar.DefaultName, "standup", "DTSTART:20240104T090000Z", "RRULE:FREQ=DAILY", "SUMMARY:Standup")

	for _, c := range []struct {
		target string
		form   url.Values
		want   string
	}{
		{"/event/new/default", url.Values{"start-date": {"2024-03-05"}, "start-time": {"09:00"}}, "An event needs a title."},
		{"/event/new/default", url.Values{"summary": {"x"}, "start-date": {"tomorrow"}}, "the day it starts on"},
		{"/event/new/default", url.Values{"summary": {"x"}, "start-date": {"2024-03-05"}}, "the time it starts at"},
		{"/event/new/default", url.Values{"summary": {"x"}, "start-date": {"2024-03-05"}, "start-time": {"09:00"}, "end-date": {"soon"}}, "The end is not a date."},
		{"/event/new/default", url.Values{"summary": {"x"}, "start-date": {"2024-03-05"}, "start-time": {"09:00"}, "end-time": {"late"}}, "The end is not a time."},
		{"/event/new/default", url.Values{"summary": {"x"}, "start-date": {"2024-03-05"}, "start-time": {"09:00"}, "end-time": {"08:00"}}, "cannot end before it starts"},
		{"/event/new/default", url.Values{"summary": {"x"}, "allday": {"1"}, "start-date": {"2024-03-05"}, "end-date": {"2024-03-04"}}, "cannot end before it starts"},
		{"/event/edit/default/standup.ics", url.Values{"summary": {"Standup"}, "allday": {"1"}, "start-date": {"2024-01-04"}}, "cannot change between all day and timed"},
	} {
		rec := post(t, h, c.target, c.form, cookie)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), c.want) {
			t.Errorf("%v = %d, want 400 saying %q:\n%s", c.form, rec.Code, c.want, rec.Body)
		}
	}
	if objects, _ := calendar.New(meta).Objects(t.Context(), username, calendar.DefaultName); len(objects) != 1 {
		t.Errorf("a form with a mistake saved something: %d objects", len(objects))
	}

	// The seconds a browser may send with a time are read, not refused.
	rec := post(t, h, "/event/new/default", url.Values{"summary": {"x"}, "start-date": {"2024-03-05"}, "start-time": {"09:00:00"}}, cookie)
	if rec.Code != http.StatusSeeOther {
		t.Errorf("a time with seconds = %d:\n%s", rec.Code, rec.Body)
	}
}

func TestDeletingAnEventAsksFirst(t *testing.T) {
	t.Parallel()
	h, _, meta := browserOver(t)
	cookie := signIn(t, h)
	putEvent(t, meta, calendar.DefaultName, "standup", "DTSTART:20240104T090000Z", "RRULE:FREQ=WEEKLY", "SUMMARY:Standup")

	page := get(t, h, "/event/delete/default/standup.ics", cookie).Body.String()
	for _, want := range []string{"<strong>Standup</strong>, every occurrence of it,", `action="/event/delete/default/standup.ics"`} {
		if !strings.Contains(page, want) {
			t.Errorf("the page has no %q:\n%s", want, page)
		}
	}
	rec := post(t, h, "/event/delete/default/standup.ics", url.Values{}, cookie)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/calendars/edu/default/" {
		t.Fatalf("deleting = %d to %q", rec.Code, rec.Header().Get("Location"))
	}
	if _, err := calendar.New(meta).Object(t.Context(), username, calendar.DefaultName, "standup.ics"); err == nil {
		t.Error("the event is still there")
	}
	for _, target := range []string{"/event/delete/default/standup.ics"} {
		if rec := get(t, h, target, cookie); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d", target, rec.Code)
		}
		if rec := post(t, h, target, url.Values{}, cookie); rec.Code != http.StatusNotFound {
			t.Errorf("POST %s = %d", target, rec.Code)
		}
	}
}

// TestATaskListHasNoEventForm: a calendar made for tasks is offered no new
// event, and a task is not an event to edit.
func TestATaskListHasNoEventForm(t *testing.T) {
	t.Parallel()
	h, _, meta := browserOver(t)
	cookie := signIn(t, h)
	if _, err := calendar.New(meta).Create(t.Context(), username, db.Calendar{Name: "tasks", Components: "VTODO"}); err != nil {
		t.Fatal(err)
	}
	putICS(t, meta, "tasks", "milk.ics", "BEGIN:VCALENDAR\nVERSION:2.0\nPRODID:-//test//EN\nBEGIN:VTODO\nUID:milk\nDTSTAMP:20240101T000000Z\nSUMMARY:Milk\nEND:VTODO\nEND:VCALENDAR\n")

	if page := get(t, h, "/calendars/edu/tasks/", cookie).Body.String(); strings.Contains(page, "/event/new/") {
		t.Errorf("a task list offers a new event:\n%s", page)
	}
	for _, target := range []string{"/event/new/tasks", "/event/edit/tasks/milk.ics", "/event/new/nowhere", "/event/edit/default/nothing.ics"} {
		if rec := get(t, h, target, cookie); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d", target, rec.Code)
		}
	}
	if rec := post(t, h, "/event/new/tasks", url.Values{"summary": {"x"}}, cookie); rec.Code != http.StatusNotFound {
		t.Errorf("POST a new event to a task list = %d", rec.Code)
	}
}
