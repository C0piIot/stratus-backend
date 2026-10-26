package web

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-ical"

	"github.com/stratus-private-cloud/stratus-backend/internal/calendar"
	"github.com/stratus-private-cloud/stratus-backend/internal/db"
)

// Writing an event (#444): three forms over calendar.Service, the contacts'
// twins, under /event/ because an event's address is CalDAV's for every method.
//
// **An edit is made to the event as stored**, as a contact's is: alarms,
// attendees and a client's X- properties stay, and a field that comes back as
// it was shown is not written. A repeating event is edited as a series -- the
// form changes the event its instances come from, keeps its rule and leaves
// the instances a client moved alone. One instance on its own, and making an
// event repeat, are #445.
//
// **A time is written in UTC unless the event already had a zone.** A new
// event would need a VTIMEZONE to name one, which is a table of rules this
// package would have to keep; one a client wrote in a zone keeps it, and its
// VTIMEZONE, when Go knows that zone by its name.
const (
	eventNewPrefix    = "/event/new/"
	eventEditPrefix   = "/event/edit/"
	eventDeletePrefix = "/event/delete/"
)

// dateParam is the day a new event's form starts on, as YYYY-MM-DD.
const dateParam = "date"

const productID = "-//Stratus//Stratus//EN"

// eventForm is one event as the form shows it, or as it was posted. Its
// dates and times are in the zone the month page draws, with an all-day
// event's end the last day it covers rather than the day after.
type eventForm struct {
	Heading, Action, Back, Delete          string
	ETag                                   string
	Summary, Location, Description         string
	AllDay                                 bool
	StartDate, StartTime, EndDate, EndTime string
	// Repeats is how a repeating event's rule reads, and empty for one that
	// does not repeat.
	Repeats    string
	Zone       string
	SummaryErr string
	WhenErr    string
	// Changed is the event having been written somewhere else since this
	// form was drawn.
	Changed bool
}

func eventHref(prefix, cal, name string) string {
	h := prefix + url.PathEscape(cal)
	if name != "" {
		h += "/" + url.PathEscape(name)
	}
	return h
}

func (h *handler) newEventForm(w http.ResponseWriter, r *http.Request, user string) {
	cal := r.PathValue("cal")
	if err := h.holdsEvents(r, user, cal); err != nil {
		h.fail(w, r, user, err)
		return
	}
	now := time.Now().In(h.zone())
	day := now.Format(time.DateOnly)
	if d := r.URL.Query().Get(dateParam); d != "" {
		if _, err := time.Parse(time.DateOnly, d); err == nil {
			day = d
		}
	}
	f := &eventForm{StartDate: day, StartTime: "09:00", EndDate: day, EndTime: "10:00"}
	h.renderEvent(w, http.StatusOK, user, cal, "", f)
}

func (h *handler) editEventForm(w http.ResponseWriter, r *http.Request, user string) {
	cal, name := r.PathValue("cal"), r.PathValue("name")
	stored, etag, err := h.storedEvent(r, user, cal, name)
	if err != nil {
		h.fail(w, r, user, err)
		return
	}
	f, err := eventFormOf(master(stored), h.zone())
	if err != nil {
		h.fail(w, r, user, err)
		return
	}
	f.ETag = etag
	h.renderEvent(w, http.StatusOK, user, cal, name, f)
}

func (h *handler) createEvent(w http.ResponseWriter, r *http.Request, user string) {
	h.saveEvent(w, r, user, r.PathValue("cal"), "")
}

func (h *handler) updateEvent(w http.ResponseWriter, r *http.Request, user string) {
	h.saveEvent(w, r, user, r.PathValue("cal"), r.PathValue("name"))
}

// saveEvent writes what was posted onto the event as it is stored, or onto a
// new one when name is empty.
func (h *handler) saveEvent(w http.ResponseWriter, r *http.Request, user, cal, name string) {
	ctx := r.Context()
	zone := h.zone()
	posted := postedEvent(r)

	var stored *ical.Calendar
	shown := &eventForm{}
	if name == "" {
		if err := h.holdsEvents(r, user, cal); err != nil {
			h.fail(w, r, user, err)
			return
		}
		stored = newEvent()
		uid, _ := master(stored).Props.Text(ical.PropUID)
		name = uid + ".ics"
		posted.ETag = ""
	} else {
		var err error
		stored, _, err = h.storedEvent(r, user, cal, name)
		switch {
		case errors.Is(err, db.ErrNotFound):
			posted.Changed = true
			h.renderEvent(w, http.StatusConflict, user, cal, name, posted)
			return
		case err != nil:
			h.fail(w, r, user, err)
			return
		}
		if shown, err = eventFormOf(master(stored), zone); err != nil {
			h.fail(w, r, user, err)
			return
		}
	}

	start, end, ok := posted.check(shown, zone)
	if !ok {
		h.renderEvent(w, http.StatusBadRequest, user, cal, r.PathValue("name"), posted)
		return
	}
	back := monthHref(user, cal, start)
	ev := master(stored)
	if !posted.applyTo(ev, shown, start, end) {
		redirectLocal(w, r, back)
		return
	}
	touch(ev, time.Now(), r.PathValue("name") != "")

	_, err := h.calendars.Put(ctx, user, cal, name, stored, func(etag string, exists bool) error {
		if exists != (posted.ETag != "") || etag != posted.ETag {
			return errChanged
		}
		return nil
	})
	switch {
	case errors.Is(err, errChanged):
		posted.Changed = true
		h.renderEvent(w, http.StatusConflict, user, cal, r.PathValue("name"), posted)
	case err != nil:
		h.fail(w, r, user, err)
	default:
		redirectLocal(w, r, back)
	}
}

func (h *handler) deleteEventForm(w http.ResponseWriter, r *http.Request, user string) {
	cal, name := r.PathValue("cal"), r.PathValue("name")
	stored, _, err := h.storedEvent(r, user, cal, name)
	if err != nil {
		h.fail(w, r, user, err)
		return
	}
	f, err := eventFormOf(master(stored), h.zone())
	if err != nil {
		h.fail(w, r, user, err)
		return
	}
	h.render(w, http.StatusOK, pageUnevent, view{
		Title: "Delete event", User: user, Event: f,
		Action: eventHref(eventDeletePrefix, cal, name), Back: eventHref(eventEditPrefix, cal, name),
	})
}

func (h *handler) deleteEvent(w http.ResponseWriter, r *http.Request, user string) {
	cal := r.PathValue("cal")
	if err := h.calendars.Delete(r.Context(), user, cal, r.PathValue("name")); err != nil {
		h.fail(w, r, user, err)
		return
	}
	redirectLocal(w, r, calendarHref(user, cal))
}

// holdsEvents is a calendar that exists and takes events: a task list has
// no form for one.
func (h *handler) holdsEvents(r *http.Request, user, cal string) error {
	c, err := h.calendars.Calendar(r.Context(), user, cal)
	if err != nil {
		return err
	}
	if !c.Holds(ical.CompEvent) {
		return db.ErrNotFound
	}
	return nil
}

// storedEvent is an object holding an event, and not found for one holding
// a task.
func (h *handler) storedEvent(r *http.Request, user, cal, name string) (*ical.Calendar, string, error) {
	o, err := h.calendars.Object(r.Context(), user, cal, name)
	if err != nil {
		return nil, "", err
	}
	if o.Component != ical.CompEvent {
		return nil, "", db.ErrNotFound
	}
	stored, err := calendar.Decode(o)
	return stored, o.ETag, err
}

func (h *handler) renderEvent(w http.ResponseWriter, status int, user, cal, name string, f *eventForm) {
	f.Zone = time.Now().In(h.zone()).Format("MST")
	f.Back = calendarHref(user, cal)
	if d, err := time.ParseInLocation(time.DateOnly, f.StartDate, h.zone()); err == nil {
		f.Back = monthHref(user, cal, d)
	}
	if name == "" {
		f.Heading, f.Action = "New event", eventHref(eventNewPrefix, cal, "")
	} else {
		f.Heading, f.Action = "Edit event", eventHref(eventEditPrefix, cal, name)
		f.Delete = eventHref(eventDeletePrefix, cal, name)
	}
	h.render(w, status, pageEvent, view{Title: f.Heading, User: user, Event: f})
}

func newEvent() *ical.Calendar {
	cal := ical.NewCalendar()
	cal.Props.SetText(ical.PropVersion, "2.0")
	cal.Props.SetText(ical.PropProductID, productID)
	ev := ical.NewComponent(ical.CompEvent)
	ev.Props.SetText(ical.PropUID, newUID())
	cal.Children = append(cal.Children, ev)
	return cal
}

// master is the event a repeating one's instances come from, or the first
// there is when an object holds only instances moved from a series.
func master(cal *ical.Calendar) *ical.Component {
	var first *ical.Component
	for _, c := range cal.Children {
		if c.Name != ical.CompEvent {
			continue
		}
		if c.Props.Get(ical.PropRecurrenceID) == nil {
			return c
		}
		if first == nil {
			first = c
		}
	}
	return first
}

// eventFormOf is what the form shows of an event, trimmed as a post is so
// that a field nobody touched compares equal.
func eventFormOf(ev *ical.Component, zone *time.Location) (*eventForm, error) {
	if ev == nil {
		return nil, db.ErrNotFound
	}
	text := func(k string) string {
		v, _ := ev.Props.Text(k)
		return strings.TrimSpace(strings.ReplaceAll(v, "\r\n", "\n"))
	}
	f := &eventForm{
		Summary: text(ical.PropSummary), Location: text(ical.PropLocation),
		Description: text(ical.PropDescription), Repeats: repeats(ev),
	}
	if ev.Props.Get(ical.PropDateTimeStart) == nil {
		return f, nil
	}
	occ, err := calendar.When(ev, zone)
	if err != nil {
		return nil, err
	}
	f.AllDay = occ.AllDay
	end := occ.End
	if occ.AllDay {
		end = end.AddDate(0, 0, -1)
	} else {
		f.StartTime, f.EndTime = occ.Start.Format("15:04"), end.Format("15:04")
	}
	f.StartDate, f.EndDate = occ.Start.Format(time.DateOnly), end.Format(time.DateOnly)
	return f, nil
}

// repeats is how an event's rule reads in a sentence's subject position.
func repeats(ev *ical.Component) string {
	rule := ev.Props.Get(ical.PropRecurrenceRule)
	if rule == nil {
		if ev.Props.Get(ical.PropRecurrenceDates) != nil {
			return "Repeats on chosen dates"
		}
		return ""
	}
	for _, part := range strings.Split(rule.Value, ";") {
		if freq, ok := strings.CutPrefix(strings.ToUpper(part), "FREQ="); ok {
			switch freq {
			case "DAILY", "WEEKLY", "MONTHLY", "YEARLY":
				return "Repeats " + strings.ToLower(freq)
			}
		}
	}
	return "Repeats"
}

func postedEvent(r *http.Request) *eventForm {
	text := func(k string) string { return strings.TrimSpace(r.PostFormValue(k)) }
	return &eventForm{
		ETag: r.PostFormValue("etag"), Summary: text("summary"), Location: text("location"),
		Description: strings.TrimSpace(strings.ReplaceAll(r.PostFormValue("description"), "\r\n", "\n")),
		AllDay:      r.PostFormValue("allday") != "",
		StartDate:   text("start-date"), StartTime: text("start-time"),
		EndDate: text("end-date"), EndTime: text("end-time"),
	}
}

// check marks what cannot be stored and reads when the event is, with an
// all-day event's end the day after the last it covers. An end left empty is
// the start's day, and an hour after it.
func (f *eventForm) check(shown *eventForm, zone *time.Location) (start, end time.Time, ok bool) {
	ok = true
	if f.Summary == "" {
		f.SummaryErr, ok = "An event needs a title.", false
	}
	if shown.Repeats != "" && f.AllDay != shown.AllDay {
		f.WhenErr = "A repeating event cannot change between all day and timed here."
		return start, end, false
	}
	day, err := time.ParseInLocation(time.DateOnly, f.StartDate, zone)
	if err != nil {
		f.WhenErr = "An event needs the day it starts on."
		return start, end, false
	}
	lastDay := day
	if f.EndDate != "" {
		if lastDay, err = time.ParseInLocation(time.DateOnly, f.EndDate, zone); err != nil {
			f.WhenErr = "The end is not a date."
			return start, end, false
		}
	}
	if f.AllDay {
		if lastDay.Before(day) {
			f.WhenErr = "An event cannot end before it starts."
			return start, end, false
		}
		return day, lastDay.AddDate(0, 0, 1), ok
	}
	if start, err = clock(day, f.StartTime); err != nil {
		f.WhenErr = "An event needs the time it starts at, or to be all day."
		return start, end, false
	}
	end = start.Add(time.Hour)
	if f.EndTime != "" {
		if end, err = clock(lastDay, f.EndTime); err != nil {
			f.WhenErr = "The end is not a time."
			return start, end, false
		}
	}
	if end.Before(start) {
		f.WhenErr = "An event cannot end before it starts."
		return start, end, false
	}
	return start, end, ok
}

// clock is a time of day on day, as a time field sends it with or without
// its seconds.
func clock(day time.Time, v string) (time.Time, error) {
	t, err := time.Parse("15:04", v)
	if err != nil {
		t, err = time.Parse(time.TimeOnly, v)
	}
	return time.Date(day.Year(), day.Month(), day.Day(), t.Hour(), t.Minute(), t.Second(), 0, day.Location()), err
}

// applyTo writes onto ev what differs between the posted form and shown, and
// says whether anything did.
func (f *eventForm) applyTo(ev *ical.Component, shown *eventForm, start, end time.Time) bool {
	changed := false
	for _, field := range []struct{ name, now, was string }{
		{ical.PropSummary, f.Summary, shown.Summary},
		{ical.PropLocation, f.Location, shown.Location},
		{ical.PropDescription, f.Description, shown.Description},
	} {
		if field.now != field.was {
			setEventText(ev, field.name, field.now)
			changed = true
		}
	}
	if f.AllDay != shown.AllDay || f.StartDate != shown.StartDate || f.EndDate != shown.EndDate ||
		!f.AllDay && (f.StartTime != shown.StartTime || f.EndTime != shown.EndTime) {
		zone := time.UTC
		if p := ev.Props.Get(ical.PropDateTimeStart); p != nil {
			if loc, err := time.LoadLocation(p.Params.Get(ical.PropTimezoneID)); err == nil && loc != time.UTC {
				zone = loc
			}
		}
		setWhen(ev, ical.PropDateTimeStart, start, f.AllDay, zone)
		setWhen(ev, ical.PropDateTimeEnd, end, f.AllDay, zone)
		ev.Props.Del(ical.PropDuration)
		changed = true
	}
	return changed
}

// setEventText sets a text property keeping its parameters, or removes it
// when v is empty.
func setEventText(ev *ical.Component, name, v string) {
	switch p := ev.Props.Get(name); {
	case v == "":
		ev.Props.Del(name)
	case p != nil:
		p.SetText(v)
	default:
		ev.Props.SetText(name, v)
	}
}

func setWhen(ev *ical.Component, name string, t time.Time, allDay bool, zone *time.Location) {
	p := ical.NewProp(name)
	if allDay {
		p.SetDate(t)
	} else {
		p.SetDateTime(t.In(zone))
	}
	ev.Props.Set(p)
}

// touch marks an event as written now, and as a new revision of one a client
// already has: SEQUENCE is how iOS and DAVx5 tell which of two copies wins.
func touch(ev *ical.Component, now time.Time, revised bool) {
	now = now.UTC().Truncate(time.Second)
	ev.Props.SetDateTime(ical.PropDateTimeStamp, now)
	ev.Props.SetDateTime(ical.PropLastModified, now)
	if !revised {
		ev.Props.SetDateTime(ical.PropCreated, now)
		return
	}
	n := 0
	if p := ev.Props.Get(ical.PropSequence); p != nil {
		n, _ = p.Int()
	}
	seq := ical.NewProp(ical.PropSequence)
	seq.Value = strconv.Itoa(n + 1)
	ev.Props.Set(seq)
}
