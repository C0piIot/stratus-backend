package calendar

import (
	"fmt"
	"time"

	"github.com/emersion/go-ical"
)

// InRange reports whether a task or a journal entry overlaps the window from
// start to end (#79), by the table in RFC 4791 9.9. An end that is zero is a
// window with no end.
//
// It exists because the library's own matcher answers this for events and
// says no to everything else, which a client asking for this week's tasks
// sees as an empty list and nothing else. Events are left to it.
func InRange(comp *ical.Component, start, end time.Time) (bool, error) {
	loc := start.Location()
	rset, err := comp.RecurrenceSet(loc)
	if err != nil {
		return false, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if rset != nil {
		until := end
		if until.IsZero() {
			until = start.AddDate(100, 0, 0)
		}
		return len(rset.Between(start, until, true)) > 0, nil
	}

	w := window{start: start, end: end}
	switch comp.Name {
	case ical.CompToDo:
		return w.todo(comp, loc)
	case ical.CompJournal:
		return w.journal(comp, loc)
	}
	return false, nil
}

type window struct{ start, end time.Time }

// endsAfter is end > t, and endsAtOrAfter end >= t, for a window whose end
// may be open.
func (w window) endsAfter(t time.Time) bool     { return w.end.IsZero() || w.end.After(t) }
func (w window) endsAtOrAfter(t time.Time) bool { return w.end.IsZero() || !w.end.Before(t) }

// startsBy is start <= t.
func (w window) startsBy(t time.Time) bool { return !w.start.After(t) }

func (w window) todo(comp *ical.Component, loc *time.Location) (bool, error) {
	at := func(name string) (time.Time, bool, error) {
		t, err := comp.Props.DateTime(name, loc)
		if err != nil {
			return time.Time{}, false, fmt.Errorf("%w: %s: %w", ErrInvalid, name, err)
		}
		return t, !t.IsZero(), nil
	}
	dtstart, hasStart, err := at(ical.PropDateTimeStart)
	if err != nil {
		return false, err
	}
	due, hasDue, err := at(ical.PropDue)
	if err != nil {
		return false, err
	}
	completed, hasCompleted, err := at(ical.PropCompleted)
	if err != nil {
		return false, err
	}
	created, hasCreated, err := at(ical.PropCreated)
	if err != nil {
		return false, err
	}
	var duration time.Duration
	if p := comp.Props.Get(ical.PropDuration); p != nil {
		if duration, err = p.Duration(); err != nil {
			return false, fmt.Errorf("%w: DURATION: %w", ErrInvalid, err)
		}
	}

	switch {
	case hasStart && duration != 0:
		finish := dtstart.Add(duration)
		return w.startsBy(finish) && (w.endsAfter(dtstart) || w.endsAtOrAfter(finish)), nil
	case hasStart && hasDue:
		return (w.start.Before(due) || w.startsBy(dtstart)) && (w.endsAfter(dtstart) || w.endsAtOrAfter(due)), nil
	case hasStart:
		return w.startsBy(dtstart) && w.endsAfter(dtstart), nil
	case hasDue:
		return w.start.Before(due) && w.endsAtOrAfter(due), nil
	case hasCompleted && hasCreated:
		return (w.startsBy(created) || w.startsBy(completed)) && (w.endsAtOrAfter(created) || w.endsAtOrAfter(completed)), nil
	case hasCompleted:
		return w.startsBy(completed) && w.endsAtOrAfter(completed), nil
	case hasCreated:
		return w.endsAfter(created), nil
	}
	// A task with no date at all is in every window.
	return true, nil
}

func (w window) journal(comp *ical.Component, loc *time.Location) (bool, error) {
	prop := comp.Props.Get(ical.PropDateTimeStart)
	if prop == nil {
		return false, nil
	}
	dtstart, err := prop.DateTime(loc)
	if err != nil {
		return false, fmt.Errorf("%w: DTSTART: %w", ErrInvalid, err)
	}
	if prop.ValueType() == ical.ValueDate {
		return w.start.Before(dtstart.AddDate(0, 0, 1)) && w.endsAfter(dtstart), nil
	}
	return w.startsBy(dtstart) && w.endsAfter(dtstart), nil
}
