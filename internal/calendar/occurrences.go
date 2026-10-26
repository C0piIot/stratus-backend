package calendar

import (
	"sort"
	"time"

	"github.com/emersion/go-ical"

	"github.com/stratus-private-cloud/stratus-backend/internal/db"
)

// Occurrence is one instance of an event as a page shows it (#111): what it is
// called, where, and from when to when in the zone the page is drawn in.
type Occurrence struct {
	// Object is the name of the object the event is stored in, which is
	// what a page links to for editing it.
	Object            string
	Summary, Location string
	Start, End        time.Time
	// AllDay is an event dated by the day: Start is that day's midnight and
	// End the midnight after its last, both in the page's zone, which is
	// what a date with no time means wherever it is read.
	AllDay bool
}

// Occurrences is every instance of every event in objects that overlaps the
// window from start to end, earliest first and the whole day before the hours
// of it, with times in loc. It is Expand read back, so a page and a client
// asking for expand see the same instances.
func Occurrences(objects []db.CalendarObject, start, end time.Time, loc *time.Location) ([]Occurrence, error) {
	var out []Occurrence
	for _, o := range objects {
		cal, err := Decode(o)
		if err != nil {
			return nil, err
		}
		expanded, err := Expand(cal, start, end)
		if err != nil {
			return nil, err
		}
		for _, comp := range expanded.Children {
			if comp.Name != ical.CompEvent {
				continue
			}
			occ, err := When(comp, loc)
			if err != nil {
				return nil, err
			}
			occ.Object = o.Name
			out = append(out, occ)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].Start.Equal(out[j].Start) {
			return out[i].Start.Before(out[j].Start)
		}
		return out[i].AllDay && !out[j].AllDay
	})
	return out, nil
}

// When is one event as a page reads it: what it is called, where, and from
// when to when in loc.
func When(comp *ical.Component, loc *time.Location) (Occurrence, error) {
	occ := Occurrence{}
	occ.Summary, _ = comp.Props.Text(ical.PropSummary)
	occ.Location, _ = comp.Props.Text(ical.PropLocation)

	length, err := duration(comp, time.UTC)
	if err != nil {
		return Occurrence{}, err
	}
	start := comp.Props.Get(ical.PropDateTimeStart)
	if start.ValueType() == ical.ValueDate {
		day, dayErr := start.DateTime(loc)
		if dayErr != nil {
			return Occurrence{}, dayErr
		}
		occ.AllDay = true
		occ.Start = day
		occ.End = day.AddDate(0, 0, max(1, int(length/(24*time.Hour))))
		return occ, nil
	}
	// A floating time, with neither a zone nor a Z, is the same wall clock
	// wherever it is read: it is read in loc, not moved into it.
	at, err := start.DateTime(loc)
	if err != nil {
		return Occurrence{}, err
	}
	occ.Start = at.In(loc)
	occ.End = occ.Start.Add(length)
	return occ, nil
}
