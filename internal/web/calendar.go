package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/emersion/go-ical"

	"github.com/stratus-private-cloud/stratus-backend/internal/calendar"
	"github.com/stratus-private-cloud/stratus-backend/internal/db"
)

// The calendar (#111): a month of one calendar's events at the
// same address CalDAV serves that calendar at -- one URL per thing, split by
// method in the composition root like the other collections, with one
// difference: an object's address stays CalDAV's for a GET too, because a
// client fetches an .ics by GET and a page has nothing to say about one event.
//
// **No script, and no stylesheet of our own.** A month is a table, which is
// what Bootstrap draws without help; on a narrow screen the same month is a
// list of days with something on them, both rendered and the breakpoint
// choosing which shows. The month moves by links, so it is in the URL and a
// month is something somebody can be sent.
//
// **The zone is the server's**, the TZ setting (#397) -- UTC where nobody
// set one. A page with no script cannot
// ask the browser, and what is drawn has to be one zone or the days would
// disagree with themselves; the README says how to set it.
const calendarsPrefix = "/calendars/"

// monthParam is the month a calendar page shows, as YYYY-MM.
const monthParam = "month"

// Calendars is the pages' and the event forms' view of the calendars:
// internal/calendar's service, so the default calendar a client would find is
// the one a page finds, and an event written here is one a client syncs.
type Calendars interface {
	Calendars(ctx context.Context, owner string) ([]db.Calendar, error)
	Calendar(ctx context.Context, owner, name string) (db.Calendar, error)
	Objects(ctx context.Context, owner, calendar string) ([]db.CalendarObject, error)
	Object(ctx context.Context, owner, calendar, name string) (db.CalendarObject, error)
	Put(ctx context.Context, owner, calendar, name string, cal *ical.Calendar, check calendar.Precondition) (db.CalendarObject, error)
	Delete(ctx context.Context, owner, calendar, name string) error
}

// calendarView is one month of one calendar.
type calendarView struct {
	Name      string
	Month     string
	Prev      string
	Next      string
	ThisMonth string
	// New is the form for an event, and empty on a calendar that does not
	// take them -- a task list.
	New      string
	Weekdays []string
	Weeks    [][]calendarDay
	// Days are the ones with something on them, for the narrow layout.
	Days   []calendarDay
	Others []calendarLink
	Zone   string
}

type calendarDay struct {
	// New is the form for an event on this day, as New is for the month.
	New     string
	Number  int
	Label   string
	InMonth bool
	Today   bool
	Events  []calendarEvent
}

type calendarEvent struct {
	Href     string
	Time     string
	Summary  string
	Location string
	AllDay   bool
}

type calendarLink struct {
	Name, Href string
	Current    bool
}

// calendarPages is every address under /calendars/ that a browser asks for.
func (h *handler) calendarPages(w http.ResponseWriter, r *http.Request, user string) {
	parts := strings.Split(strings.Trim(r.PathValue("path"), "/"), "/")
	switch {
	case parts[0] == "":
		redirectLocal(w, r, calendarsPrefix+url.PathEscape(user)+"/")
	case parts[0] != user || len(parts) > 2:
		// Somebody else's calendars are not there, rather than forbidden.
		h.fail(w, r, user, db.ErrNotFound)
	case len(parts) == 1:
		h.calendarIndex(w, r, user)
	default:
		h.calendarMonth(w, r, user, parts[1])
	}
}

// calendarIndex goes to the first calendar, which is where somebody opening
// the calendar wants to be; the others are a row of links on its page.
func (h *handler) calendarIndex(w http.ResponseWriter, r *http.Request, user string) {
	all, err := h.calendars.Calendars(r.Context(), user)
	if err != nil {
		h.fail(w, r, user, err)
		return
	}
	first := calendar.DefaultName
	if len(all) > 0 {
		first = all[0].Name
		for _, c := range all {
			if c.Name == calendar.DefaultName {
				first = c.Name
			}
		}
	}
	redirectLocal(w, r, calendarHref(user, first))
}

func (h *handler) calendarMonth(w http.ResponseWriter, r *http.Request, user, name string) {
	ctx := r.Context()
	c, err := h.calendars.Calendar(ctx, user, name)
	if err != nil {
		h.fail(w, r, user, err)
		return
	}
	zone := h.zone()
	now := time.Now().In(zone)
	first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, zone)
	if m := r.URL.Query().Get(monthParam); m != "" {
		parsed, perr := time.ParseInLocation("2006-01", m, zone)
		if perr != nil {
			h.fail(w, r, user, db.ErrInvalidPath)
			return
		}
		first = parsed
	}

	// The grid starts on the first day of the week on or before the first of
	// the month and ends on the last on or after its last day, and what is
	// fetched is the grid: a day of the month before is drawn, so what is on
	// it is drawn too.
	weekStart := h.weekStart()
	into := func(day time.Time) int { return (int(day.Weekday()) - int(weekStart) + 7) % 7 }
	gridStart := first.AddDate(0, 0, -into(first))
	last := first.AddDate(0, 1, -1)
	gridEnd := last.AddDate(0, 0, 7-into(last))

	objects, err := h.calendars.Objects(ctx, user, name)
	if err != nil {
		h.fail(w, r, user, err)
		return
	}
	events, err := calendar.Occurrences(objects, gridStart, gridEnd, zone)
	if err != nil {
		h.fail(w, r, user, err)
		return
	}

	cv := &calendarView{
		Name:      displayName(c),
		Month:     first.Format("January 2006"),
		Prev:      monthHref(user, name, first.AddDate(0, -1, 0)),
		Next:      monthHref(user, name, first.AddDate(0, 1, 0)),
		ThisMonth: calendarHref(user, name),
		Weekdays:  weekdays(weekStart),
		Zone:      now.Format("MST"),
	}
	if c.Holds(ical.CompEvent) {
		cv.New = eventHref(eventNewPrefix, name, "")
	}
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, zone)
	var week []calendarDay
	for day := gridStart; day.Before(gridEnd); day = day.AddDate(0, 0, 1) {
		d := calendarDay{
			Number: day.Day(), Label: day.Format("Monday 2 January"),
			InMonth: day.Month() == first.Month(), Today: day.Equal(today),
		}
		if cv.New != "" {
			d.New = cv.New + "?" + dateParam + "=" + day.Format(time.DateOnly)
		}
		next := day.AddDate(0, 0, 1)
		for _, e := range events {
			if e.Start.Before(next) && (e.End.After(day) || e.End.Equal(e.Start) && !e.Start.Before(day)) {
				d.Events = append(d.Events, eventRow(e, name, day))
			}
		}
		week = append(week, d)
		if len(week) == 7 {
			cv.Weeks = append(cv.Weeks, week)
			week = nil
		}
		if d.InMonth && len(d.Events) > 0 {
			cv.Days = append(cv.Days, d)
		}
	}

	if all, err := h.calendars.Calendars(ctx, user); err == nil && len(all) > 1 {
		for _, other := range all {
			cv.Others = append(cv.Others, calendarLink{
				Name: displayName(other), Href: calendarHref(user, other.Name), Current: other.Name == name,
			})
		}
	}
	h.render(w, http.StatusOK, pageCalendar, view{Title: cv.Name, User: user, Calendar: cv})
}

// weekdays are the grid's headings, from the day the week starts on.
func weekdays(start time.Weekday) []string {
	out := make([]string, 7)
	for i := range out {
		out[i] = time.Weekday((int(start) + i) % 7).String()[:3]
	}
	return out
}

// eventRow is one event as one day shows it: its time when it starts that
// day, and nothing when it is all day or began on a day before.
func eventRow(e calendar.Occurrence, cal string, day time.Time) calendarEvent {
	row := calendarEvent{
		Href: eventHref(eventEditPrefix, cal, e.Object), Summary: e.Summary, Location: e.Location, AllDay: e.AllDay,
	}
	if row.Summary == "" {
		row.Summary = "(no title)"
	}
	if !e.AllDay && !e.Start.Before(day) {
		row.Time = e.Start.Format("15:04")
	}
	return row
}

func displayName(c db.Calendar) string {
	if c.DisplayName != "" {
		return c.DisplayName
	}
	return c.Name
}

func calendarHref(user, name string) string {
	return calendarsPrefix + url.PathEscape(user) + "/" + url.PathEscape(name) + "/"
}

func monthHref(user, name string, month time.Time) string {
	return calendarHref(user, name) + "?" + monthParam + "=" + month.Format("2006-01")
}
