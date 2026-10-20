package calendar

import (
	"fmt"
	"strings"
	"time"

	"github.com/emersion/go-ical"
	"github.com/teambition/rrule-go"
)

// Whether something falls in a window of time, which is what a calendar-query
// with a time-range asks (RFC 4791 9.9). It is answered here for every kind
// of component rather than by go-webdav's matcher, which says no to anything
// that is not an event (#79), and which reads repetitions through go-ical's
// RecurrenceSet -- and that ignores RDATE altogether and reads only the first
// value of a property that may carry several, so an event repeated on dates
// of its own was matched by its first date alone (#78).

// Overlaps reports whether some instance of a component named name in cal
// falls in the window from start to end, an end that is zero being a window
// with no end. An instance that was moved or changed is judged where it went:
// the master's occurrence it replaces is left out, and the override stands
// for it.
func Overlaps(cal *ical.Calendar, name string, start, end time.Time) (bool, error) {
	overridden := map[time.Time]bool{}
	for _, comp := range cal.Children {
		if comp.Name != name {
			continue
		}
		if p := comp.Props.Get(ical.PropRecurrenceID); p != nil {
			id, err := p.DateTime(start.Location())
			if err != nil {
				return false, fmt.Errorf("%w: RECURRENCE-ID: %w", ErrInvalid, err)
			}
			overridden[id.Truncate(time.Second)] = true
		}
	}
	for _, comp := range cal.Children {
		if comp.Name != name {
			continue
		}
		var skip map[time.Time]bool
		if comp.Props.Get(ical.PropRecurrenceID) == nil {
			skip = overridden
		}
		in, err := inRange(comp, start, end, skip)
		if err != nil || in {
			return in, err
		}
	}
	return false, nil
}

// InRange is Overlaps for one component on its own.
func InRange(comp *ical.Component, start, end time.Time) (bool, error) {
	return inRange(comp, start, end, nil)
}

func inRange(comp *ical.Component, start, end time.Time, skip map[time.Time]bool) (bool, error) {
	loc := start.Location()
	set, err := recurrence(comp, loc, skip)
	if err != nil {
		return false, err
	}
	w := window{start: start, end: end}
	if set != nil {
		length, err := duration(comp, loc)
		if err != nil {
			return false, err
		}
		return w.holdsOne(set, length), nil
	}

	switch comp.Name {
	case ical.CompEvent:
		return w.event(comp, loc)
	case ical.CompToDo:
		return w.todo(comp, loc)
	case ical.CompJournal:
		return w.journal(comp, loc)
	}
	return false, nil
}

// recurrence is a repeating component's instances, or nil for one that does
// not repeat. DTSTART is always the first of them, RDATE adds and EXDATE
// takes away, every one of a property's values counts, and skip is taken away
// too -- the occurrences an override stands for.
func recurrence(comp *ical.Component, loc *time.Location, skip map[time.Time]bool) (*rrule.Set, error) {
	rule, err := comp.Props.RecurrenceRule()
	if err != nil {
		return nil, fmt.Errorf("%w: RRULE: %w", ErrInvalid, err)
	}
	if rule == nil && len(comp.Props[ical.PropRecurrenceDates]) == 0 {
		return nil, nil
	}
	dtstart, err := comp.Props.DateTime(ical.PropDateTimeStart, loc)
	if err != nil {
		return nil, fmt.Errorf("%w: DTSTART: %w", ErrInvalid, err)
	}
	if dtstart.IsZero() {
		return nil, fmt.Errorf("%w: it repeats from no DTSTART", ErrInvalid)
	}

	set := &rrule.Set{}
	if rule != nil {
		r, err := rrule.NewRRule(*rule)
		if err != nil {
			return nil, fmt.Errorf("%w: RRULE: %w", ErrInvalid, err)
		}
		set.RRule(r)
	}
	set.DTStart(dtstart)
	set.RDate(dtstart)
	for _, name := range []string{ical.PropRecurrenceDates, ical.PropExceptionDates} {
		for i := range comp.Props[name] {
			dates, err := dateValues(&comp.Props[name][i], loc)
			if err != nil {
				return nil, fmt.Errorf("%w: %s: %w", ErrInvalid, name, err)
			}
			for _, d := range dates {
				if name == ical.PropRecurrenceDates {
					set.RDate(d)
				} else {
					set.ExDate(d)
				}
			}
		}
	}
	for t := range skip {
		set.ExDate(t)
	}
	return set, nil
}

// dateValues reads every value of a property that may carry several, comma
// separated, each in the property's own zone. A PERIOD counts by its start.
func dateValues(prop *ical.Prop, loc *time.Location) ([]time.Time, error) {
	var out []time.Time
	for _, v := range strings.Split(prop.Value, ",") {
		one := *prop
		one.Value, _, _ = strings.Cut(v, "/")
		if one.ValueType() == ical.ValuePeriod {
			one.SetValueType(ical.ValueDateTime)
		}
		t, err := one.DateTime(loc)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

// duration is how long each instance of a component lasts: what DURATION
// says, or the distance to DTEND or DUE, or a whole day for an event or a
// journal entry dated by the day, and otherwise nothing.
func duration(comp *ical.Component, loc *time.Location) (time.Duration, error) {
	if p := comp.Props.Get(ical.PropDuration); p != nil {
		d, err := p.Duration()
		if err != nil {
			return 0, fmt.Errorf("%w: DURATION: %w", ErrInvalid, err)
		}
		return d, nil
	}
	dtstart, err := comp.Props.DateTime(ical.PropDateTimeStart, loc)
	if err != nil {
		return 0, fmt.Errorf("%w: DTSTART: %w", ErrInvalid, err)
	}
	for _, name := range []string{ical.PropDateTimeEnd, ical.PropDue} {
		t, err := comp.Props.DateTime(name, loc)
		if err != nil {
			return 0, fmt.Errorf("%w: %s: %w", ErrInvalid, name, err)
		}
		if !t.IsZero() {
			return t.Sub(dtstart), nil
		}
	}
	start := comp.Props.Get(ical.PropDateTimeStart)
	if start != nil && start.ValueType() == ical.ValueDate && comp.Name != ical.CompToDo {
		return 24 * time.Hour, nil
	}
	return 0, nil
}

type window struct{ start, end time.Time }

// endsAfter is end > t, and endsAtOrAfter end >= t, for a window whose end
// may be open.
func (w window) endsAfter(t time.Time) bool     { return w.end.IsZero() || w.end.After(t) }
func (w window) endsAtOrAfter(t time.Time) bool { return w.end.IsZero() || !w.end.Before(t) }

// startsBy is start <= t.
func (w window) startsBy(t time.Time) bool { return !w.start.After(t) }

// holdsOne is whether an instance of length begins before the window ends and
// ends after it begins -- or, for an instance of no length, begins inside it.
// The first instance that could is the only one that has to be asked about,
// since every instance lasts as long as every other.
func (w window) holdsOne(set *rrule.Set, length time.Duration) bool {
	var first time.Time
	if length > 0 {
		first = set.After(w.start.Add(-length), false)
	} else {
		first = set.After(w.start, true)
	}
	return !first.IsZero() && w.endsAfter(first)
}

func (w window) event(comp *ical.Component, loc *time.Location) (bool, error) {
	prop := comp.Props.Get(ical.PropDateTimeStart)
	if prop == nil {
		return false, nil
	}
	dtstart, err := prop.DateTime(loc)
	if err != nil {
		return false, fmt.Errorf("%w: DTSTART: %w", ErrInvalid, err)
	}
	length, err := duration(comp, loc)
	if err != nil {
		return false, err
	}
	if length > 0 {
		return w.start.Before(dtstart.Add(length)) && w.endsAfter(dtstart), nil
	}
	return w.startsBy(dtstart) && w.endsAfter(dtstart), nil
}

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
	var length time.Duration
	if p := comp.Props.Get(ical.PropDuration); p != nil {
		if length, err = p.Duration(); err != nil {
			return false, fmt.Errorf("%w: DURATION: %w", ErrInvalid, err)
		}
	}

	switch {
	case hasStart && length != 0:
		finish := dtstart.Add(length)
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
