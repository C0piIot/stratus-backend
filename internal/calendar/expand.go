package calendar

import (
	"fmt"
	"time"

	"github.com/emersion/go-ical"
)

// Expand is cal as a client asks for it with CALDAV:expand (RFC 4791 9.6.5,
// #78): every instance of every repeating component that overlaps the window
// from start to end, each a component of its own with a RECURRENCE-ID and no
// rule, in UTC and with the time zones gone. A moved instance comes as it was
// moved, and one moved out of the window does not come at all.
//
// It is a representation for a report and not the object: what is stored, and
// what the ETag summarises, is cal as it was written.
func Expand(cal *ical.Calendar, start, end time.Time) (*ical.Calendar, error) {
	start, end = start.UTC(), end.UTC()
	out := ical.NewCalendar()
	for name, props := range cal.Props {
		out.Props[name] = append([]ical.Prop(nil), props...)
	}

	overrides := map[time.Time]bool{}
	for _, comp := range cal.Children {
		p := comp.Props.Get(ical.PropRecurrenceID)
		if p == nil {
			continue
		}
		id, err := p.DateTime(time.UTC)
		if err != nil {
			return nil, fmt.Errorf("%w: RECURRENCE-ID: %w", ErrInvalid, err)
		}
		overrides[id.Truncate(time.Second)] = true
	}

	for _, comp := range cal.Children {
		if comp.Name == ical.CompTimezone {
			continue
		}
		if comp.Props.Get(ical.PropRecurrenceID) != nil {
			in, err := InRange(comp, start, end)
			if err != nil {
				return nil, err
			}
			if in {
				out.Children = append(out.Children, inUTC(comp))
			}
			continue
		}

		set, err := recurrence(comp, time.UTC, overrides)
		if err != nil {
			return nil, err
		}
		if set == nil {
			in, rangeErr := InRange(comp, start, end)
			if rangeErr != nil {
				return nil, rangeErr
			}
			if in {
				out.Children = append(out.Children, inUTC(comp))
			}
			continue
		}

		length, err := duration(comp, time.UTC)
		if err != nil {
			return nil, err
		}
		w := window{start: start, end: end}
		for _, at := range set.Between(start.Add(-length), end, true) {
			if w.holdsInstance(at, length) {
				out.Children = append(out.Children, instance(comp, at, length))
			}
		}
	}
	return out, nil
}

// holdsInstance is whether one instance of length beginning at at overlaps the
// window, by the same rule holdsOne applies to the first.
func (w window) holdsInstance(at time.Time, length time.Duration) bool {
	if length > 0 {
		return at.Add(length).After(w.start) && w.endsAfter(at)
	}
	return w.startsBy(at) && w.endsAfter(at)
}

// instance is the occurrence of master that begins at at: the master's
// properties with the rule taken off, the dates moved to it, and a
// RECURRENCE-ID saying which occurrence it is.
func instance(master *ical.Component, at time.Time, length time.Duration) *ical.Component {
	comp := inUTC(master)
	for _, name := range []string{ical.PropRecurrenceRule, ical.PropRecurrenceDates, ical.PropExceptionDates} {
		comp.Props.Del(name)
	}
	date := master.Props.Get(ical.PropDateTimeStart).ValueType() == ical.ValueDate
	set := func(name string, t time.Time) {
		if date {
			comp.Props.SetDate(name, t)
		} else {
			comp.Props.SetDateTime(name, t.UTC())
		}
	}
	set(ical.PropDateTimeStart, at)
	set(ical.PropRecurrenceID, at)
	for _, name := range []string{ical.PropDateTimeEnd, ical.PropDue} {
		if comp.Props.Get(name) != nil {
			set(name, at.Add(length))
		}
	}
	return comp
}

// inUTC is a copy of comp with every date and time in it written in UTC, which
// is what an expanded answer carries: the VTIMEZONEs a TZID would name are not
// sent with it. A date with no time stays a date, and a floating time -- one
// with neither a zone nor a Z -- stays floating, as the RFC has it.
func inUTC(comp *ical.Component) *ical.Component {
	out := &ical.Component{Name: comp.Name, Props: ical.Props{}}
	for name, props := range comp.Props {
		for _, p := range props {
			p.Params = cloneParams(p.Params)
			if p.Params.Get(ical.PropTimezoneID) != "" && p.ValueType() != ical.ValueDate {
				if t, err := p.DateTime(time.UTC); err == nil {
					p.Params.Del(ical.PropTimezoneID)
					p.SetDateTime(t.UTC())
				}
			}
			out.Props[name] = append(out.Props[name], p)
		}
	}
	for _, child := range comp.Children {
		out.Children = append(out.Children, inUTC(child))
	}
	return out
}

func cloneParams(params ical.Params) ical.Params {
	out := ical.Params{}
	for k, v := range params {
		out[k] = append([]string(nil), v...)
	}
	return out
}
