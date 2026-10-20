package calendar_test

import (
	"errors"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/calendar"
	"github.com/C0piIot/stratus-backend/internal/db"
)

// TestWhatAPageDraws is Occurrences, which is what the month page reads: the
// instances in a zone, an all-day event as days that do not move with it, a
// floating time as the wall clock it says, and the whole day first.
func TestWhatAPageDraws(t *testing.T) {
	t.Parallel()
	madrid, err := time.LoadLocation("Europe/Madrid")
	if err != nil {
		t.Fatal(err)
	}
	object := func(name string, lines ...string) db.CalendarObject {
		data, encErr := calendar.Encode(calendarOf(t, lines...))
		if encErr != nil {
			t.Fatal(encErr)
		}
		return db.CalendarObject{Name: name, Data: data}
	}
	objects := []db.CalendarObject{
		object("standup", "BEGIN:VEVENT", "UID:s", "DTSTAMP:20240101T000000Z", "DTSTART:20240305T080000Z",
			"DURATION:PT30M", "RRULE:FREQ=DAILY;COUNT=2", "SUMMARY:Standup", "LOCATION:Room 2", "END:VEVENT"),
		object("trip", "BEGIN:VEVENT", "UID:t", "DTSTAMP:20240101T000000Z", "DTSTART;VALUE=DATE:20240305",
			"DTEND;VALUE=DATE:20240308", "SUMMARY:Trip", "END:VEVENT"),
		object("lunch", "BEGIN:VEVENT", "UID:l", "DTSTAMP:20240101T000000Z", "DTSTART:20240305T130000",
			"SUMMARY:Lunch", "END:VEVENT"),
		object("task", "BEGIN:VTODO", "UID:k", "DTSTAMP:20240101T000000Z", "DUE:20240305T090000Z", "SUMMARY:Task", "END:VTODO"),
	}
	start := time.Date(2024, 3, 1, 0, 0, 0, 0, madrid)
	got, err := calendar.Occurrences(objects, start, start.AddDate(0, 1, 0), madrid)
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		summary, start, end string
		allDay              bool
	}
	var rows []row
	for _, o := range got {
		rows = append(rows, row{o.Summary, o.Start.Format("02 15:04"), o.End.Format("02 15:04"), o.AllDay})
	}
	want := []row{
		{"Trip", "05 00:00", "08 00:00", true},
		{"Standup", "05 09:00", "05 09:30", false},
		{"Lunch", "05 13:00", "05 13:00", false},
		{"Standup", "06 09:00", "06 09:30", false},
	}
	if len(rows) != len(want) {
		t.Fatalf("Occurrences = %+v, want %+v", rows, want)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Errorf("occurrence %d = %+v, want %+v", i, rows[i], want[i])
		}
	}
	if got[1].Location != "Room 2" {
		t.Errorf("the standup's location = %q", got[1].Location)
	}

	if _, err := calendar.Occurrences([]db.CalendarObject{{Name: "x", Data: "not iCalendar"}}, start, start.AddDate(0, 1, 0), madrid); err == nil {
		t.Error("Occurrences took text that is not iCalendar")
	}
	bad := object("bad", "BEGIN:VEVENT", "UID:b", "DTSTAMP:20240101T000000Z", "DTSTART:20240305T080000Z", "RRULE:FREQ=DAILY",
		"DURATION:PT1H", "END:VEVENT")
	bad.Data = bad.Data[:len(bad.Data)-len("END:VCALENDAR\r\n")] // cut short, as a broken row would be
	if _, err := calendar.Occurrences([]db.CalendarObject{bad}, start, start.AddDate(0, 1, 0), madrid); err == nil {
		t.Error("Occurrences took a calendar cut short")
	}
	unreadable := object("u", "BEGIN:VEVENT", "UID:u", "DTSTAMP:20240101T000000Z", "DTSTART:20240305T080000Z",
		"RRULE:FREQ=SOMETIMES", "END:VEVENT")
	if _, err := calendar.Occurrences([]db.CalendarObject{unreadable}, start, start.AddDate(0, 1, 0), madrid); !errors.Is(err, calendar.ErrInvalid) {
		t.Errorf("Occurrences of a repetition nobody can read = %v, want ErrInvalid", err)
	}
}
