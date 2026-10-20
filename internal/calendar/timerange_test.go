package calendar_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-ical"

	"github.com/C0piIot/stratus-backend/internal/calendar"
)

func component(t *testing.T, name string, props ...string) *ical.Component {
	t.Helper()
	text := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\nBEGIN:" + name + "\r\nUID:x\r\nDTSTAMP:20240101T000000Z\r\n" +
		strings.Join(props, "\r\n") + "\r\nEND:" + name + "\r\nEND:VCALENDAR\r\n"
	cal, err := ical.NewDecoder(strings.NewReader(strings.Replace(text, "\r\n\r\n", "\r\n", 1))).Decode()
	if err != nil {
		t.Fatal(err)
	}
	return cal.Children[0]
}

// TestATaskOrAJournalIsInAWindow walks RFC 4791 9.9 row by row, each with a
// window it falls in and one it does not. March is the window throughout.
func TestATaskOrAJournalIsInAWindow(t *testing.T) {
	t.Parallel()
	march := [2]time.Time{time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC), time.Date(2024, 4, 1, 0, 0, 0, 0, time.UTC)}
	open := [2]time.Time{march[0], {}}

	for _, tc := range []struct {
		name   string
		comp   string
		props  []string
		window [2]time.Time
		want   bool
	}{
		{"start and duration, inside", "VTODO", []string{"DTSTART:20240310T090000Z", "DURATION:PT1H"}, march, true},
		{"start and duration, before", "VTODO", []string{"DTSTART:20240210T090000Z", "DURATION:P2D"}, march, false},
		{"start and duration, across the start", "VTODO", []string{"DTSTART:20240228T090000Z", "DURATION:P5D"}, march, true},
		{"start and due, inside", "VTODO", []string{"DTSTART:20240305T090000Z", "DUE:20240306T090000Z"}, march, true},
		{"start and due, after", "VTODO", []string{"DTSTART:20240405T090000Z", "DUE:20240406T090000Z"}, march, false},
		{"start alone, inside", "VTODO", []string{"DTSTART:20240315T090000Z"}, march, true},
		{"start alone, after", "VTODO", []string{"DTSTART:20240415T090000Z"}, march, false},
		{"due alone, inside", "VTODO", []string{"DUE:20240320T090000Z"}, march, true},
		{"due alone, before", "VTODO", []string{"DUE:20240220T090000Z"}, march, false},
		{"completed and created, across", "VTODO", []string{"CREATED:20240201T090000Z", "COMPLETED:20240302T090000Z"}, march, true},
		{"completed and created, before", "VTODO", []string{"CREATED:20240101T090000Z", "COMPLETED:20240102T090000Z"}, march, false},
		{"completed alone, inside", "VTODO", []string{"COMPLETED:20240302T090000Z"}, march, true},
		{"completed alone, after", "VTODO", []string{"COMPLETED:20240502T090000Z"}, march, false},
		{"created alone, before the end", "VTODO", []string{"CREATED:20240101T090000Z"}, march, true},
		{"created alone, after the end", "VTODO", []string{"CREATED:20240501T090000Z"}, march, false},
		{"no date at all", "VTODO", nil, march, true},
		{"a window with no end", "VTODO", []string{"DUE:20300101T090000Z"}, open, true},
		{"weekly since January", "VTODO", []string{"DTSTART:20240102T090000Z", "RRULE:FREQ=WEEKLY"}, march, true},
		{"weekly, over before March", "VTODO", []string{"DTSTART:20240102T090000Z", "RRULE:FREQ=WEEKLY;COUNT=3"}, march, false},
		{"weekly with no end to the window", "VTODO", []string{"DTSTART:20240102T090000Z", "RRULE:FREQ=WEEKLY"}, open, true},
		{"a journal entry, inside", "VJOURNAL", []string{"DTSTART:20240312T090000Z"}, march, true},
		{"a journal entry, after", "VJOURNAL", []string{"DTSTART:20240412T090000Z"}, march, false},
		{"a journal day, its last", "VJOURNAL", []string{"DTSTART;VALUE=DATE:20240229"}, [2]time.Time{march[0].Add(-time.Hour), march[1]}, true},
		{"a journal day, before", "VJOURNAL", []string{"DTSTART;VALUE=DATE:20240228"}, march, false},
		{"a journal entry with no date", "VJOURNAL", nil, march, false},
		{"an event is not ours to judge", "VEVENT", []string{"DTSTART:20240310T090000Z"}, march, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := calendar.InRange(component(t, tc.comp, tc.props...), tc.window[0], tc.window[1])
			if err != nil || got != tc.want {
				t.Errorf("InRange = %v, %v; want %v", got, err, tc.want)
			}
		})
	}

	for _, bad := range [][]string{
		{"DTSTART:yesterday"}, {"DUE:soon"}, {"COMPLETED:never"}, {"CREATED:once"},
		{"DTSTART:20240310T090000Z", "DURATION:a while"}, {"DTSTART:20240310T090000Z", "RRULE:FREQ=SOMETIMES"},
	} {
		if _, err := calendar.InRange(component(t, "VTODO", bad...), march[0], march[1]); !errors.Is(err, calendar.ErrInvalid) {
			t.Errorf("InRange of a task with %v = %v, want ErrInvalid", bad, err)
		}
	}
	if _, err := calendar.InRange(component(t, "VJOURNAL", "DTSTART:yesterday"), march[0], march[1]); !errors.Is(err, calendar.ErrInvalid) {
		t.Errorf("InRange of a journal entry with a bad date = %v, want ErrInvalid", err)
	}
}
