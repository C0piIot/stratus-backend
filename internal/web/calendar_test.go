package web_test

import (
	htmlstd "html"
	"net/http"
	"strings"
	"testing"

	"github.com/emersion/go-ical"

	"github.com/C0piIot/stratus-backend/internal/calendar"
	"github.com/C0piIot/stratus-backend/internal/db"
)

func putEvent(t *testing.T, meta db.Store, cal, name string, lines ...string) {
	t.Helper()
	text := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\nBEGIN:VEVENT\r\nUID:" + name +
		"\r\nDTSTAMP:20240101T000000Z\r\n" + strings.Join(lines, "\r\n") + "\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	parsed, err := ical.NewDecoder(strings.NewReader(text)).Decode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := calendar.New(meta).Put(t.Context(), username, cal, name+".ics", parsed, nil); err != nil {
		t.Fatal(err)
	}
}

func TestTheCalendarNeedsASession(t *testing.T) {
	t.Parallel()
	h := newHandler(t, nil)
	rec := get(t, h, "/calendars/edu/default/")
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/login") {
		t.Errorf("the calendar with no session = %d", rec.Code)
	}
}

// TestAMonthOfACalendar is #111: the month a calendar's events fall in, a
// weekly one by every instance, an all-day one across the days it covers, and
// the links that move the month.
func TestAMonthOfACalendar(t *testing.T) {
	t.Parallel()
	h, _, meta := browserOver(t)
	cookie := signIn(t, h)

	putEvent(t, meta, calendar.DefaultName, "standup",
		"DTSTART:20240104T090000Z", "DURATION:PT30M", "RRULE:FREQ=WEEKLY", "SUMMARY:Standup", "LOCATION:Room 2")
	putEvent(t, meta, calendar.DefaultName, "holiday",
		"DTSTART;VALUE=DATE:20240318", "DTEND;VALUE=DATE:20240320", "SUMMARY:Holiday & rest")
	putEvent(t, meta, calendar.DefaultName, "april", "DTSTART:20240410T090000Z", "SUMMARY:Somewhere in April")

	// The way in is the bar's link, which lands on the default calendar.
	rec := get(t, h, "/calendars/", cookie)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/calendars/edu/" {
		t.Fatalf("/calendars/ = %d to %q", rec.Code, rec.Header().Get("Location"))
	}
	rec = get(t, h, "/calendars/edu/", cookie)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/calendars/edu/default/" {
		t.Fatalf("/calendars/edu/ = %d to %q", rec.Code, rec.Header().Get("Location"))
	}

	page := get(t, h, "/calendars/edu/default/?month=2024-03", cookie)
	if page.Code != http.StatusOK {
		t.Fatalf("the month = %d:\n%s", page.Code, page.Body.String())
	}
	body := htmlstd.UnescapeString(page.Body.String())
	for _, want := range []string{
		"March 2024", "Holiday & rest", "Room 2",
		`href="/calendars/edu/default/?month=2024-02"`, `href="/calendars/edu/default/?month=2024-04"`,
		`href="/calendars/">Calendar</a>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the month has no %q", want)
		}
	}
	// Four Thursdays in March and the 29th of February, which the grid draws
	// because its first row starts on that Monday.
	if n := strings.Count(body, "09:00</span> Standup"); n != 5 {
		t.Errorf("the grid shows the standup %d times, want 5", n)
	}
	// Two days of holiday, each in the grid and in the list.
	if n := strings.Count(body, "Holiday & rest</span>"); n != 2 {
		t.Errorf("the list shows the holiday on %d days, want 2", n)
	}
	if strings.Contains(body, "Somewhere in April") {
		t.Error("April's event is drawn in March")
	}
	if strings.Contains(body, "<script") && strings.Count(body, "<script") > strings.Count(get(t, h, "/files/", cookie).Body.String(), "<script") {
		t.Error("the calendar page loads a script the rest of the UI does not")
	}
}

func TestTheOtherCalendarsAreARowOfLinks(t *testing.T) {
	t.Parallel()
	h, _, meta := browserOver(t)
	cookie := signIn(t, h)
	svc := calendar.New(meta)
	if _, err := svc.Calendars(t.Context(), username); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(t.Context(), username, db.Calendar{Name: "work", DisplayName: "Work"}); err != nil {
		t.Fatal(err)
	}

	body := get(t, h, "/calendars/edu/work/", cookie).Body.String()
	for _, want := range []string{`href="/calendars/edu/default/">Calendar</a>`, `active" href="/calendars/edu/work/" aria-current="page">Work</a>`} {
		if !strings.Contains(body, want) {
			t.Errorf("the calendar has no %q:\n%s", want, body)
		}
	}
}

func TestACalendarThatIsNotThere(t *testing.T) {
	t.Parallel()
	h, _, _ := browserOver(t)
	cookie := signIn(t, h)
	for target, code := range map[string]int{
		"/calendars/edu/nope/":             http.StatusNotFound,
		"/calendars/someone/default/":      http.StatusNotFound,
		"/calendars/edu/default/x/":        http.StatusNotFound,
		"/calendars/edu/default/?month=13": http.StatusBadRequest,
	} {
		if rec := get(t, h, target, cookie); rec.Code != code {
			t.Errorf("%s = %d, want %d", target, rec.Code, code)
		}
	}
}
