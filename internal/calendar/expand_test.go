package calendar_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-ical"

	"github.com/stratus-private-cloud/stratus-backend/internal/calendar"
)

func calendarOf(t *testing.T, lines ...string) *ical.Calendar {
	t.Helper()
	text := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\n" + strings.Join(lines, "\r\n") + "\r\nEND:VCALENDAR\r\n"
	cal, err := ical.NewDecoder(strings.NewReader(text)).Decode()
	if err != nil {
		t.Fatal(err)
	}
	return cal
}

// starts lists each expanded instance as DTSTART and RECURRENCE-ID, with the
// summary when it is not the master's, and fails on anything a rule left.
func starts(t *testing.T, cal *ical.Calendar) []string {
	t.Helper()
	var out []string
	for _, comp := range cal.Children {
		if comp.Name == ical.CompTimezone {
			t.Error("an expanded calendar still carries a VTIMEZONE")
		}
		for _, gone := range []string{ical.PropRecurrenceRule, ical.PropRecurrenceDates, ical.PropExceptionDates} {
			if comp.Props.Get(gone) != nil {
				t.Errorf("an expanded instance still carries %s", gone)
			}
		}
		line := comp.Props.Get(ical.PropDateTimeStart).Value
		if id := comp.Props.Get(ical.PropRecurrenceID); id != nil {
			line += " for " + id.Value
		}
		if s, _ := comp.Props.Text(ical.PropSummary); s != "Standup" {
			line += " " + s
		}
		out = append(out, line)
	}
	return out
}

var (
	marchStart = time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)
	marchEnd   = time.Date(2024, 4, 1, 0, 0, 0, 0, time.UTC)
)

// TestARepetitionIsExpandedIntoItsInstances is RFC 4791 9.6.5 on a weekly
// standup through March: one instance taken away, one moved inside the month,
// one moved out of it, and one from a moved instance of February that landed
// in March.
func TestARepetitionIsExpandedIntoItsInstances(t *testing.T) {
	t.Parallel()
	cal := calendarOf(t,
		"BEGIN:VEVENT", "UID:s", "DTSTAMP:20240101T000000Z", "DTSTART:20240104T090000Z", "DTEND:20240104T093000Z",
		"RRULE:FREQ=WEEKLY", "EXDATE:20240307T090000Z", "SUMMARY:Standup", "END:VEVENT",
		"BEGIN:VEVENT", "UID:s", "DTSTAMP:20240101T000000Z", "RECURRENCE-ID:20240314T090000Z",
		"DTSTART:20240315T100000Z", "DTEND:20240315T103000Z", "SUMMARY:Friday", "END:VEVENT",
		"BEGIN:VEVENT", "UID:s", "DTSTAMP:20240101T000000Z", "RECURRENCE-ID:20240321T090000Z",
		"DTSTART:20240521T090000Z", "DTEND:20240521T093000Z", "SUMMARY:May", "END:VEVENT",
		"BEGIN:VEVENT", "UID:s", "DTSTAMP:20240101T000000Z", "RECURRENCE-ID:20240229T090000Z",
		"DTSTART:20240301T090000Z", "DTEND:20240301T093000Z", "SUMMARY:Late", "END:VEVENT",
	)
	got, err := calendar.Expand(cal, marchStart, marchEnd)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"20240328T090000Z for 20240328T090000Z",
		"20240315T100000Z for 20240314T090000Z Friday",
		"20240301T090000Z for 20240229T090000Z Late",
	}
	if g := starts(t, got); !slices.Equal(g, want) {
		t.Errorf("expanded March =\n%v\nwant\n%v", g, want)
	}
	if end := got.Children[0].Props.Get(ical.PropDateTimeEnd).Value; end != "20240328T093000Z" {
		t.Errorf("the instance's DTEND = %s, want it moved with its DTSTART", end)
	}
	if got.Props.Get(ical.PropProductID) == nil {
		t.Error("the expanded calendar lost its PRODID")
	}
}

func TestExpandingTheRestOfIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		lines []string
		want  []string
	}{
		{"on dates of its own", []string{
			"BEGIN:VEVENT", "UID:d", "DTSTAMP:20240101T000000Z", "DTSTART:20240104T090000Z", "DURATION:PT1H",
			"RDATE:20240201T090000Z,20240315T090000Z", "SUMMARY:Standup", "END:VEVENT",
		}, []string{"20240315T090000Z for 20240315T090000Z"}},
		{"in a zone that changes its clocks in March", []string{
			"BEGIN:VTIMEZONE", "TZID:Europe/Madrid", "BEGIN:STANDARD", "DTSTART:19701025T030000", "TZOFFSETFROM:+0200",
			"TZOFFSETTO:+0100", "END:STANDARD", "END:VTIMEZONE",
			"BEGIN:VEVENT", "UID:z", "DTSTAMP:20240101T000000Z", "DTSTART;TZID=Europe/Madrid:20240322T100000",
			"DURATION:PT1H", "RRULE:FREQ=WEEKLY;COUNT=2", "SUMMARY:Standup", "END:VEVENT",
		}, []string{"20240322T090000Z for 20240322T090000Z", "20240329T090000Z for 20240329T090000Z"}},
		{"all day, every other week", []string{
			"BEGIN:VEVENT", "UID:a", "DTSTAMP:20240101T000000Z", "DTSTART;VALUE=DATE:20240301",
			"RRULE:FREQ=WEEKLY;INTERVAL=2", "SUMMARY:Standup", "END:VEVENT",
		}, []string{"20240301 for 20240301", "20240315 for 20240315", "20240329 for 20240329"}},
		{"an instance across the start of the window", []string{
			"BEGIN:VEVENT", "UID:n", "DTSTAMP:20240101T000000Z", "DTSTART:20240222T230000Z", "DURATION:PT3H",
			"RRULE:FREQ=WEEKLY;COUNT=2", "SUMMARY:Standup", "END:VEVENT",
		}, []string{"20240229T230000Z for 20240229T230000Z"}},
		{"a task that repeats, due with each", []string{
			"BEGIN:VTODO", "UID:t", "DTSTAMP:20240101T000000Z", "DTSTART:20240310T090000Z", "DUE:20240311T090000Z",
			"RRULE:FREQ=WEEKLY;COUNT=2", "SUMMARY:Standup", "END:VTODO",
		}, []string{"20240310T090000Z for 20240310T090000Z", "20240317T090000Z for 20240317T090000Z"}},
		{"once, in the window", []string{
			"BEGIN:VEVENT", "UID:o", "DTSTAMP:20240101T000000Z", "DTSTART;TZID=Europe/Madrid:20240310T100000",
			"DURATION:PT1H", "SUMMARY:Standup", "END:VEVENT",
		}, []string{"20240310T090000Z"}},
		{"once, outside it", []string{
			"BEGIN:VEVENT", "UID:o", "DTSTAMP:20240101T000000Z", "DTSTART:20240410T100000Z", "SUMMARY:Standup", "END:VEVENT",
		}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := calendar.Expand(calendarOf(t, tc.lines...), marchStart, marchEnd)
			if err != nil {
				t.Fatal(err)
			}
			if g := starts(t, got); !slices.Equal(g, tc.want) {
				t.Errorf("expanded =\n%v\nwant\n%v", g, tc.want)
			}
		})
	}

	task, err := calendar.Expand(calendarOf(t,
		"BEGIN:VTODO", "UID:t", "DTSTAMP:20240101T000000Z", "DTSTART:20240310T090000Z", "DUE:20240311T090000Z",
		"RRULE:FREQ=WEEKLY;COUNT=1", "SUMMARY:Standup", "END:VTODO"), marchStart, marchEnd)
	if err != nil {
		t.Fatal(err)
	}
	if due := task.Children[0].Props.Get(ical.PropDue).Value; due != "20240311T090000Z" {
		t.Errorf("the task's DUE = %s", due)
	}

	for _, bad := range [][]string{
		{"BEGIN:VEVENT", "UID:b", "DTSTAMP:20240101T000000Z", "RECURRENCE-ID:whenever", "DTSTART:20240310T090000Z", "END:VEVENT"},
		{"BEGIN:VEVENT", "UID:b", "DTSTAMP:20240101T000000Z", "DTSTART:20240310T090000Z", "RRULE:FREQ=SOMETIMES", "END:VEVENT"},
		{"BEGIN:VEVENT", "UID:b", "DTSTAMP:20240101T000000Z", "DTSTART:soon", "END:VEVENT"},
		{"BEGIN:VEVENT", "UID:b", "DTSTAMP:20240101T000000Z", "DTSTART:20240310T090000Z", "RRULE:FREQ=DAILY", "DURATION:ages", "END:VEVENT"},
	} {
		if _, err := calendar.Expand(calendarOf(t, bad...), marchStart, marchEnd); !errors.Is(err, calendar.ErrInvalid) {
			t.Errorf("Expand of %v = %v, want ErrInvalid", bad[3:5], err)
		}
	}
}
